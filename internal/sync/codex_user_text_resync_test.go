package sync

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexUserTextUpgradeReparsesUnchangedSource(t *testing.T) {
	for _, version := range []int{111, 112} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			const id = "019eb791-cf7d-75c1-8439-9ed74c122b05"
			const sessionID = "codex:" + id
			root := t.TempDir()
			day := filepath.Join(root, "2026", "09", "01")
			require.NoError(t, os.MkdirAll(day, 0o755))
			content := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_cli_rs", "2026-09-01T10:00:00Z"),
				`{"type":"response_item","timestamp":"2026-09-01T10:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<INSTRUCTIONS>Repository rules</INSTRUCTIONS>"},{"type":"input_text","text":"Review the changes"}]}}`,
				`{"type":"response_item","timestamp":"2026-09-01T10:00:02Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Keep the public API"},{"type":"input_text","text":"<environment_context>Runtime context</environment_context>"}]}}`,
			)
			require.NoError(t, os.WriteFile(filepath.Join(day, "rollout-2026-09-01T10-00-00-"+id+".jsonl"), []byte(content), 0o600))
			database := openTestDB(t)
			cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local"}
			engine := NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			engine.Close()

			// Preserve source fingerprints, but restore the old parser's stored
			// output: the first prompt was dropped and the second kept context.
			const stale = "Keep the public API\n<environment_context>Runtime context</environment_context>"
			require.NoError(t, database.ReplaceSessionMessages(t.Context(), sessionID, []db.Message{{
				SessionID: sessionID, Role: "user", Content: stale,
			}}))
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: "retained", Agent: "gemini", Project: "sample", Machine: "local", MessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{
				SessionID: "retained", Role: "user", Content: "Only stored in the archive",
			}}))
			path := database.Path()
			require.NoError(t, database.Close())
			raw, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			_, err = raw.ExecContext(t.Context(), `UPDATE sessions SET first_message=?,message_count=1,user_message_count=1,data_version=? WHERE id=?`, stale, version, sessionID)
			require.NoError(t, err)
			_, err = raw.ExecContext(t.Context(), fmt.Sprintf("PRAGMA user_version = %d", version))
			require.NoError(t, err)
			require.NoError(t, raw.Close())

			reopened, err := db.OpenIsolated(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reopened.Close()) })
			require.True(t, reopened.NeedsResync(), "the parser migration must reach unchanged sources")
			upgraded := NewEngine(t.Context(), reopened, cfg)
			t.Cleanup(upgraded.Close)
			stats, err := upgraded.SyncThenRun(t.Context(), false, nil, func(full bool) error {
				assert.True(t, full, "startup must select a full resync without an explicit request")
				return nil
			})
			require.NoError(t, err)
			require.False(t, stats.Aborted)
			require.Zero(t, stats.Failed)
			assert.False(t, reopened.NeedsResync())
			session, err := reopened.GetSession(t.Context(), sessionID)
			require.NoError(t, err)
			require.NotNil(t, session)
			require.NotNil(t, session.FirstMessage)
			assert.Equal(t, "Review the changes", *session.FirstMessage)
			assert.Equal(t, 2, session.UserMessageCount)
			messages, err := reopened.GetAllMessages(t.Context(), sessionID)
			require.NoError(t, err)
			require.Len(t, messages, 2)
			assert.Equal(t, "Review the changes", messages[0].Content)
			assert.Equal(t, "Keep the public API", messages[1].Content)
			retained, err := reopened.GetAllMessages(t.Context(), "retained")
			require.NoError(t, err)
			require.Len(t, retained, 1)
			assert.Equal(t, "Only stored in the archive", retained[0].Content)
		})
	}
}
