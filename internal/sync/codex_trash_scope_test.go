package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestLegacyCodexTrashScopeSurvivesReturningPage(t *testing.T) {
	for _, moved := range []bool{false, true} {
		name := "same_path"
		if moved {
			name = "archived_path"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			root, archived := t.TempDir(), t.TempDir()
			writeCodexUsagePage(t, root, 0, true, true)
			writeCodexUsagePage(t, root, 1, true, true)
			missingPath := writeCodexUsagePage(t, root, 2, true, true)
			missingBytes, err := os.ReadFile(missingPath)
			require.NoError(t, err)
			require.NoError(t, os.Remove(missingPath))
			database := openTestDB(t)
			thread := paginationSessionID(0)
			require.NoError(t, database.UpsertSession(ctx, db.Session{ID: thread, Agent: "codex", Project: "sample", Machine: "local", FilePath: &missingPath, MessageCount: 1}))
			require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: thread, Role: "user", Content: "Trashed saved page"}}))
			require.NoError(t, database.SoftDeleteSession(ctx, thread))
			path := database.Path()
			require.NoError(t, database.Close())
			raw, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			_, err = raw.ExecContext(ctx, "PRAGMA user_version=122")
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			reopened, err := db.OpenIsolated(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reopened.Close() })
			engine := newCodexRevertEngine(t, reopened, root, archived)
			for _, phase := range []string{"upgrade", "later_rebuild"} {
				stats, err := engine.SyncThenRun(ctx, true, nil, func(bool) error { return nil })
				require.NoError(t, err, phase)
				require.False(t, stats.Aborted, phase)
				require.Zero(t, stats.Failed, phase)
				require.True(t, reopened.IsSessionTrashed(ctx, thread), phase)
			}
			returnedPath := missingPath
			if moved {
				returnedPath = filepath.Join(archived, filepath.Base(missingPath))
			}
			require.NoError(t, os.WriteFile(returnedPath, missingBytes, 0o600))
			stats := engine.SyncAll(ctx, nil)
			require.Zero(t, stats.Failed)
			page, err := reopened.GetSession(ctx, paginationSessionID(2))
			require.NoError(t, err)
			assert.Nil(t, page, "returning page must remain hidden by legacy thread trash")
		})
	}
}
