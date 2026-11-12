package sync_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func codexRateLimitEvent(ts string, used float64) string {
	return fmt.Sprintf(`{"type":"event_msg","timestamp":%q,"payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"codex","primary":{"used_percent":%v,"window_minutes":10080,"resets_at":1794890601},"secondary":null,"plan_type":"pro"}}}`, ts, used)
}

// A rollout that grows records each new rate-limit report through the
// incremental append path, keeps the account key the session meta recorded,
// and adds nothing for a report the committed prefix already holds.
func TestCodexRateLimitsRecordedOnAppend(t *testing.T) {
	env := setupTestEnv(t)
	ctx := t.Context()
	const id = "019eb791-cf7d-75c1-8439-9ed74c122c09"
	path := env.writeCodexSession(t, filepath.Join("2026", "11", "01"),
		"rollout-2026-11-01T12-00-00-"+id+".jsonl",
		testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaWithFieldsJSON(id, "/tmp/proj", "codex_cli_rs",
				"2026-11-01T12:00:00Z", map[string]any{"creator_account_id": "account-y"}),
			testjsonl.CodexMsgJSON("user", "first", "2026-11-01T12:00:01Z"),
			codexRateLimitEvent("2026-11-01T12:00:02Z", 20),
		))
	env.engine.SyncAll(ctx, nil)
	first, err := env.db.GetRateLimitSnapshots(ctx, 0)
	require.NoError(t, err)
	require.Len(t, first, 1)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON("user", "second", "2026-11-01T12:00:03Z"),
		codexRateLimitEvent("2026-11-01T12:00:04Z", 20),
		codexRateLimitEvent("2026-11-01T12:00:05Z", 21),
	))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Equal(t, 1, env.engine.SyncAll(ctx, nil).Synced)

	sess, err := env.db.GetSessionFull(ctx, "codex:"+id)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.True(t, sess.LastWriteIncremental, "the append must take the incremental path")
	all, err := env.db.GetRateLimitSnapshots(ctx, 0)
	require.NoError(t, err)
	require.Len(t, all, 2, "the repeated report adds nothing")
	assert.Equal(t, "2026-11-01T12:00:05.000Z", all[1].ObservedAt)
	require.NotNil(t, all[1].Primary)
	assert.InDelta(t, 21.0, all[1].Primary.UsedPercent, 0)
	assert.Equal(t, first[0].AccountKey, all[1].AccountKey)
	assert.NotEmpty(t, all[1].AccountKey)
}

// A failed report write fails the source instead of advancing its
// checkpoint past the reports, so the next pass over the unchanged file
// stores them.
func TestCodexRateLimitWriteFailureIsRetried(t *testing.T) {
	env := setupTestEnv(t)
	ctx := t.Context()
	const id = "019eb791-cf7d-75c1-8439-9ed74c122c0a"
	env.writeCodexSession(t, filepath.Join("2026", "11", "01"),
		"rollout-2026-11-01T12-00-00-"+id+".jsonl",
		testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(id, "/tmp/proj", "codex_cli_rs", "2026-11-01T12:00:00Z"),
			testjsonl.CodexMsgJSON("user", "first", "2026-11-01T12:00:01Z"),
			codexRateLimitEvent("2026-11-01T12:00:02Z", 30),
		))
	require.NoError(t, env.db.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TRIGGER fail_rate_limits BEFORE INSERT ON rate_limit_snapshots
			BEGIN SELECT RAISE(ABORT, 'database is busy'); END`)
		return err
	}))
	env.engine.SyncAll(ctx, nil)
	none, err := env.db.GetRateLimitSnapshots(ctx, 0)
	require.NoError(t, err)
	require.Empty(t, none)

	require.NoError(t, env.db.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DROP TRIGGER fail_rate_limits`)
		return err
	}))
	env.engine.SyncAll(ctx, nil)
	got, err := env.db.GetRateLimitSnapshots(ctx, 0)
	require.NoError(t, err)
	require.Len(t, got, 1, "the unchanged source is read again")
	assert.Equal(t, "codex:"+id, got[0].SessionID)
}
