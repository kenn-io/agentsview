package sync

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

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
