package parser

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Hosted raw derivation parses materialized SQLite snapshots under an attempt
// context: an attempt timeout or a lost lease must abort the provider's
// database work instead of being dropped before the first query. Each case
// seeds a real provider database and parses through the same per-session
// entrypoint the provider's dbBackedProviderSpec.parse closure calls, with an
// already-canceled context. The provider-level Parse entrypoints cannot be
// used for this: they reject a dead context before touching the database, so
// they would mask a dropped-context regression inside the loading helpers.
// With an already-dead context each case fails at the provider's first
// context-aware query, so these cases pin that boundary only; deeper
// helper-chain propagation past that first query is pinned separately for the
// mtime path by TestZCodeMtimeQueryPropagatesCanceledContext below.
func TestDBBackedParsePropagatesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	t.Run("forge", func(t *testing.T) {
		dbPath, seeder, db := newForgeTestDB(t)
		defer db.Close()
		seedForgeConversation(t, seeder)

		sess, msgs, err := parseForgeSession(ctx, dbPath, "conv-001", "testmachine")
		require.Error(t, err, "canceled context must abort forge parsing")
		assert.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, sess)
		assert.Empty(t, msgs)
	})

	t.Run("piebald", func(t *testing.T) {
		dbPath := newPiebaldTestDB(t)
		execPiebaldTestSQL(t, dbPath,
			`INSERT INTO chats
			 (id, title, created_at, updated_at, is_deleted, message_count, current_directory)
			 VALUES (42, 'Fix bug', '2026-05-01T10:00:00Z', '2026-05-01T10:05:00Z', 0, 1, '/repo/app')`)
		execPiebaldTestSQL(t, dbPath,
			`INSERT INTO messages
			 (id, parent_chat_id, role, model, created_at, updated_at, status)
			 VALUES (100, 42, 'user', '', '2026-05-01T10:00:01Z', '2026-05-01T10:00:01Z', 'completed')`)
		seedPiebaldTextPart(t, dbPath, 200, 100, 0, "Please fix this", false)

		results, err := parsePiebaldSessionResults(ctx, dbPath, "42", "testmachine")
		require.Error(t, err, "canceled context must abort piebald parsing")
		assert.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, results)
	})

	t.Run("warp", func(t *testing.T) {
		dbPath, seeder, db := newWarpTestDB(t)
		defer db.Close()
		seedWarpConversation(t, seeder)

		sess, msgs, err := parseWarpSession(ctx, dbPath, "conv-001", "testmachine")
		require.Error(t, err, "canceled context must abort warp parsing")
		assert.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, sess)
		assert.Empty(t, msgs)
	})

	t.Run("zcode", func(t *testing.T) {
		fixture := newZCodeTestFixture(t)
		fixture.insertSession(
			t, "session-ctx", "/workspace/app", "Canceled",
			"2026-07-06T13:00:00Z", "2026-07-06T13:01:00Z", "", "",
		)
		fixture.insertMessage(t, "m1", "session-ctx", "2026-07-06T13:00:01Z", `{"role":"user"}`)
		fixture.insertPart(t, "p1", "m1", "session-ctx",
			`{"type":"text","text":"Please fix this"}`)

		result, err := parseZCodeSession(ctx, fixture.DBPath, "session-ctx", "testmachine")
		require.Error(t, err, "canceled context must abort zcode parsing")
		assert.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, result)
	})

	t.Run("goose", func(t *testing.T) {
		fixture := newGooseTestFixture(t)
		fixture.insertSession(t, "child", "Auth review", "main_session", "")
		fixture.insertMessage(t, "child", "user", `[
			{"type":"text","text":"Inspect the authentication flow.","annotations":[]}
		]`, 1_700_000_000)

		result, err := parseGooseSession(ctx, fixture.dbPath, "child", "testmachine")
		require.Error(t, err, "canceled context must abort goose parsing")
		assert.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, result)
	})
}

// The zcode mtime lookup is the deepest database work in the metadata and
// parse paths, and it tolerates a missing model_usage table, so a swallowed
// query error there would silently degrade FileMtime instead of aborting the
// parse. This reaches the usage-mtime query itself: the session row is loaded
// under a live context and only the mtime lookup observes the dead one.
func TestZCodeMtimeQueryPropagatesCanceledContext(t *testing.T) {
	fixture := newZCodeTestFixture(t)
	fixture.insertSession(
		t, "session-mtime-ctx", "/workspace/app", "Canceled",
		"2026-07-06T13:00:00Z", "2026-07-06T13:01:00Z", "", "",
	)
	row, err := loadZCodeSessionRow(
		context.Background(), fixture.database, "session-mtime-ctx",
	)
	require.NoError(t, err)

	t.Run("canceled context aborts the usage-mtime query", func(t *testing.T) {
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		mtime, err := zcodeSessionFileMtime(canceled, fixture.DBPath, fixture.database, row)
		require.Error(t, err, "canceled context must abort the zcode usage-mtime query")
		assert.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, mtime)
	})

	t.Run("missing usage table stays tolerated", func(t *testing.T) {
		_, err := fixture.database.Exec(`DROP TABLE model_usage`)
		require.NoError(t, err)
		oldDBMtime := time.Date(2026, 7, 6, 13, 0, 0, 0, time.UTC)
		require.NoError(t, os.Chtimes(fixture.DBPath, oldDBMtime, oldDBMtime))

		mtime, err := zcodeSessionFileMtime(
			context.Background(), fixture.DBPath, fixture.database, row,
		)
		require.NoError(t, err)
		assert.Equal(
			t, int64(1783342860000000000), mtime,
			"a missing model_usage table must not turn into an error",
		)
	})
}
