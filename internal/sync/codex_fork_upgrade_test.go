package sync

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexForkRebuildBeforeProvenanceSchema(t *testing.T) {
	e, database, cp, pp, meta := forkReviewEngine(t, 1<<30, parser.AgentCodex)
	require.NoError(t, os.WriteFile(cp, []byte(meta+testjsonl.JoinJSONL(
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime),
		testjsonl.CodexMsgJSON("assistant", "fork answer", forkReviewTime),
		testjsonl.CodexTokenCountJSON(forkReviewTime, 20, 5, 0),
	)), 0o600))
	id := "codex:" + forkReviewChild
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: id, Agent: "codex", Machine: "local", Project: "project", FilePath: &cp}))
	require.NoError(t, database.ReplaceSessionMessages(t.Context(), id, []db.Message{{SessionID: id, Role: "assistant", Content: "fork answer", Timestamp: forkReviewTime, OutputTokens: 5, HasOutputTokens: true}}))
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "codex:orphan", Agent: "codex", Machine: "local", Project: "project"}))
	require.NoError(t, database.ReplaceSessionMessages(t.Context(), "codex:orphan", []db.Message{{SessionID: "codex:orphan", Role: "assistant", Content: "archived answer"}}))
	// Archives from before fork provenance have no state table. A rebuild
	// reads them without migrations before constructing the replacement.
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "DROP TABLE codex_fork_state")
		return err
	}))
	stats := e.ResyncAll(t.Context(), nil)
	require.True(t, stats.ArchiveRebuilt)
	require.Zero(t, stats.Failed)
	assert.Equal(t, 1, stats.Deferred)
	assert.Equal(t, []string{"fork answer"}, messageContents(t, database, id))
	assert.Equal(t, []string{"archived answer"}, messageContents(t, database, "codex:orphan"))
	require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
	require.Zero(t, e.SyncAll(t.Context(), nil).Deferred)
	usage, err := database.GetSessionUsage(t.Context(), id, true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 5, usage.TotalOutputTokens)
	assert.Equal(t, []string{"archived answer"}, messageContents(t, database, "codex:orphan"))
}

func TestCodexForkUpgradeDoesNotRestoreReparsedParentIdentity(t *testing.T) {
	const (
		parent    = "11111111-1111-4111-8111-111111111111"
		fork      = "22222222-2222-4222-8222-222222222222"
		timestamp = "2026-09-01T10:00:01Z"
	)
	for _, filename := range []string{
		"rollout-2026-09-01T10-00-00-" + fork + ".jsonl",
		"rollout-manually-restored.jsonl",
	} {
		t.Run(filename, func(t *testing.T) {
			root := t.TempDir()
			forkPath := writeCodexRollout(t, root, "01", filename, testjsonl.JoinJSONL(
				testjsonl.CodexForkedSessionMetaJSON(fork, parent, "/work/project", "user", timestamp),
				testjsonl.CodexSessionMetaJSON(parent, "/other/project", "user", timestamp),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", timestamp),
				testjsonl.CodexMsgJSON("user", "fork question", timestamp),
				testjsonl.CodexMsgJSON("assistant", "fork answer", timestamp),
				testjsonl.CodexTokenCountJSON(timestamp, 1000, 500, 0),
			))
			// The parent is readable for replay detection but outside the CWD
			// filter, so the fresh archive has no legitimate row under its ID.
			writeCodexRollout(t, root, "01", "rollout-2026-09-01T09-00-00-"+parent+".jsonl", testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(parent, "/other/project", "user", timestamp),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", timestamp),
				testjsonl.CodexMsgJSON("user", "parent question", timestamp),
			))
			database := openTestDB(t)
			legacyID := "codex:" + parent
			// Before version 40, the copied parent metadata replaced the fork ID.
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: legacyID, Agent: "codex", Project: "project", Machine: "local", FilePath: &forkPath, Cwd: "/other/project",
				DataVersion: 39, MessageCount: 2, UserMessageCount: 1, TotalOutputTokens: 500, HasTotalOutputTokens: true,
				StartedAt: new(timestamp), EndedAt: new(timestamp),
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{
				{SessionID: legacyID, Ordinal: 0, Role: "user", Content: "fork question", Timestamp: timestamp},
				{
					SessionID: legacyID, Ordinal: 1, Role: "assistant", Content: "fork answer", Timestamp: timestamp,
					Model: "gpt-5", TokenUsage: []byte(`{"input_tokens":1000,"output_tokens":500}`), OutputTokens: 500, HasOutputTokens: true,
				},
			}))
			dbPath := database.Path()
			require.NoError(t, database.Close())
			raw, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, raw.Close()) })
			_, err = raw.ExecContext(t.Context(), "PRAGMA user_version=39")
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			reopened, err := db.OpenIsolated(t.Context(), dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
			require.True(t, reopened.NeedsResync())
			engine := NewEngine(t.Context(), reopened, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local",
				IncludeCwdPrefixes: []string{"/work"},
			})
			t.Cleanup(engine.Close)
			stats, err := engine.SyncThenRun(t.Context(), false, nil, func(full bool) error {
				assert.True(t, full)
				return nil
			})
			require.NoError(t, err)
			require.Zero(t, stats.Failed)
			require.False(t, stats.Aborted)
			require.True(t, stats.ArchiveRebuilt)
			require.Zero(t, stats.Deferred)
			assert.Zero(t, stats.OrphanedCopied)
			stale, err := reopened.GetSession(t.Context(), legacyID)
			require.NoError(t, err)
			assert.Nil(t, stale, "the reparsed fork must not also survive under its old parent identity")
			assert.Equal(t, []string{"fork question", "fork answer"}, messageContents(t, reopened, "codex:"+fork))
			usage, err := reopened.GetDailyUsage(t.Context(), db.UsageFilter{
				From: "2026-09-01", To: "2026-09-01", Timezone: "UTC",
			})
			require.NoError(t, err)
			assert.Equal(t, 500, usage.Totals.OutputTokens)
		})
	}
}
