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
			headPath := writeCodexUsagePage(t, root, 0, true, true)
			writeCodexUsagePage(t, root, 1, true, true)
			missingPath := writeCodexUsagePage(t, root, 2, true, true)
			missingBytes, err := os.ReadFile(missingPath)
			require.NoError(t, err)
			require.NoError(t, os.Remove(missingPath))
			database := openTestDB(t)
			thread := paginationSessionID(0)
			require.NoError(t, database.UpsertSession(ctx, db.Session{ID: thread, Agent: "codex", Project: "sample", Machine: "local", FilePath: &headPath, MessageCount: 1}))
			require.NoError(t, database.InsertMessages(ctx, []db.Message{{SessionID: thread, Role: "user", Content: "Trashed saved page"}}))
			require.NoError(t, database.SoftDeleteSession(ctx, thread))
			path := database.Path()
			require.NoError(t, database.Close())
			raw, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			_, err = raw.ExecContext(ctx, "PRAGMA user_version=129")
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
				missing, err := reopened.GetSessionFull(ctx, paginationSessionID(2))
				require.NoError(t, err, phase)
				require.Nil(t, missing, "returning page must have no row before its file returns: %s", phase)
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

func TestLegacyCodexTrashRestorePreservesArchivedPage(t *testing.T) {
	for _, restorePage := range []bool{false, true} {
		name := "restore_thread"
		if restorePage {
			name = "restore_page"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			writeCodexUsagePage(t, root, 0, true, true)
			missingPath := writeCodexUsagePage(t, root, 2, true, true)
			require.NoError(t, os.Remove(missingPath))
			database := openTestDB(t)
			thread, page := paginationSessionID(0), paginationSessionID(2)
			require.NoError(t, database.UpsertSession(ctx, db.Session{
				ID: thread, Agent: "codex", Project: "sample", Machine: "local",
				FilePath: &missingPath, MessageCount: 1,
			}))
			require.NoError(t, database.InsertMessages(ctx, []db.Message{{
				SessionID: thread, Role: "assistant", Content: "Saved page answer", SourceUUID: "saved-reply",
			}}))
			require.NoError(t, database.ReplaceSessionUsageEvents(ctx, thread, []db.UsageEvent{{
				SessionID: thread, Source: "codex", Model: "gpt-5", OutputTokens: 30, DedupKey: "saved-usage",
			}}))
			require.NoError(t, database.RenameSession(ctx, thread, new("Saved page")))
			_, err := database.StarSession(ctx, thread)
			require.NoError(t, err)
			messages, err := database.GetAllMessages(ctx, thread)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			_, err = database.PinMessage(ctx, thread, messages[0].ID, new("Keep this answer"))
			require.NoError(t, err)
			changes, err := database.ExportConversationChanges(ctx, db.ConversationExportOptions{})
			require.NoError(t, err)
			require.Len(t, changes.Changes, 1)
			messageID := changes.Changes[0].MessageID
			require.NoError(t, database.SoftDeleteSession(ctx, thread))
			path := database.Path()
			require.NoError(t, database.Close())
			raw, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			_, err = raw.ExecContext(ctx, "PRAGMA user_version=124")
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			reopened, err := db.OpenIsolated(ctx, path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reopened.Close() })
			engine := newCodexRevertEngine(t, reopened, root)
			stats, err := engine.SyncThenRun(ctx, true, nil, func(bool) error { return nil })
			require.NoError(t, err)
			require.False(t, stats.Aborted)
			require.Zero(t, stats.Failed)
			assert.Equal(t, []string{"Saved page answer"}, messageContents(t, reopened, page))
			assert.True(t, reopened.IsSessionTrashed(ctx, thread))
			assert.True(t, reopened.IsSessionTrashed(ctx, paginationSessionID(1)), "retain the thread-wide trash scope")

			restoreID := thread
			if restorePage {
				restoreID = page
			}
			n, err := reopened.RestoreSession(ctx, restoreID)
			require.NoError(t, err)
			require.EqualValues(t, 1, n)
			stats, err = engine.SyncThenRun(ctx, true, nil, func(bool) error { return nil })
			require.NoError(t, err)
			require.False(t, stats.Aborted)
			require.Zero(t, stats.Failed)
			assert.Equal(t, []string{"Saved page answer"}, messageContents(t, reopened, page))
			if !restorePage {
				assert.Equal(t, []string{"Request 0", "Response 0"}, messageContents(t, reopened, thread))
				_, err = reopened.RestoreSession(ctx, page)
				require.NoError(t, err)
			}
			session, err := reopened.GetSession(ctx, page)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, new("Saved page"), session.DisplayName)
			stars, err := reopened.ListStarredSessionIDs(ctx)
			require.NoError(t, err)
			assert.Contains(t, stars, page)
			pins, err := reopened.ListPinnedMessages(ctx, "", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, page, pins[0].SessionID)
			assert.Equal(t, new("Keep this answer"), pins[0].Note)
			usage, err := reopened.GetUsageEvents(ctx, page)
			require.NoError(t, err)
			require.Len(t, usage, 1)
			assert.Equal(t, 30, usage[0].OutputTokens)
			changes, err = reopened.ExportConversationChanges(ctx, db.ConversationExportOptions{})
			require.NoError(t, err)
			var exported []string
			for _, change := range changes.Changes {
				if change.SessionID == page && change.MessageID != "" {
					assert.Equal(t, messageID, change.MessageID)
					body, err := reopened.GetConversationMessage(ctx, db.ConversationMessageOptions{
						DatabaseID: changes.DatabaseID, SessionID: page, MessageID: change.MessageID, Revision: change.Revision,
					})
					require.NoError(t, err)
					require.NotNil(t, body.Text)
					exported = append(exported, *body.Text)
				}
			}
			assert.Equal(t, []string{"Saved page answer"}, exported)
		})
	}
}
