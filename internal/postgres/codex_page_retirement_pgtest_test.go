//go:build pgtest

package postgres

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestCodexPageUpgradeRetiresMissingPostgresHead(t *testing.T) {
	for _, tc := range []struct {
		name              string
		pinned            bool
		trashAction       string
		localTrash        bool
		newerPage         bool
		precedingSessions int
		restoreLocal      string
	}{
		{name: "unpinned"},
		{name: "pinned", pinned: true},
		{name: "newer_page_curation", pinned: true, newerPage: true},
		{name: "restore_page", pinned: true, trashAction: "page"},
		{name: "restore_thread", pinned: true, trashAction: "thread"},
		{name: "purge_thread", pinned: true, trashAction: "purge"},
		{name: "local_trash_restore_page", pinned: true, trashAction: "page", localTrash: true},
		{name: "local_trash_same_batch", pinned: true, trashAction: "page", localTrash: true, precedingSessions: 48},
		{name: "local_trash_cross_batch", pinned: true, trashAction: "page", localTrash: true, precedingSessions: 49},
		{name: "restored_anchor_cross_batch", pinned: true, trashAction: "page", localTrash: true, precedingSessions: 49, restoreLocal: "thread"},
		{name: "restored_page_cross_batch", pinned: true, trashAction: "page", localTrash: true, precedingSessions: 49, restoreLocal: "page"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const thread = "codex:11111111-1111-4111-8111-111111111111"
			const page = thread + "_22222222-2222-4222-8222-222222222222"
			const schema = "agentsview_codex_page_retirement_test"
			ctx := t.Context()
			pgURL := testPGURL(t)
			cleanNamedPGSchema(t, pgURL, schema)
			t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
			source := testDB(t)
			sourcePath := source.Path()
			pagePath := filepath.Join(t.TempDir(), "rollout-2026-09-25T12-00-00-"+page[len("codex:"):]+".jsonl")
			sess := db.Session{
				ID: thread, Agent: "codex", Project: "sample", Machine: "machine",
				FilePath: &pagePath, MessageCount: 1, CreatedAt: "2026-09-25T12:00:00Z",
				StartedAt: new("2026-09-25T12:00:00Z"), TotalOutputTokens: 5, HasTotalOutputTokens: true,
			}
			require.NoError(t, source.UpsertSession(ctx, sess))
			// Codex messages have no source UUID. Usage must disappear with the
			// stale transcript, rather than depending on cross-session dedup.
			message := db.Message{
				SessionID: thread, Role: "assistant", Content: "retained page answer",
				Timestamp: "2026-09-25T12:00:00Z", Model: "gpt-5",
				TokenUsage: []byte(`{"input_tokens":10,"output_tokens":5}`), OutputTokens: 5, HasOutputTokens: true,
			}
			require.NoError(t, source.InsertMessages(ctx, []db.Message{message}))
			require.NoError(t, source.SetSessionDataVersion(ctx, thread, 129))
			syncer, err := New(pgURL, schema, source, "machine", true, storage.PusherOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, syncer.Close()) })
			pushed, err := syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			require.Zero(t, pushed.Errors)
			store := &Store{pg: syncer.pg}
			require.NoError(t, store.RenameSession(ctx, thread, new("remote page title")))
			starred, err := store.StarSession(ctx, thread)
			require.NoError(t, err)
			require.True(t, starred)
			if tc.pinned {
				_, err = store.PinMessage(ctx, thread, 0, new("remote note"))
				require.NoError(t, err)
			}
			filter := db.UsageFilter{From: "2026-09-25", To: "2026-09-25", Timezone: "UTC"}
			before, err := store.GetDailyUsage(ctx, filter)
			require.NoError(t, err)
			require.Equal(t, 10, before.Totals.InputTokens)
			require.Equal(t, 5, before.Totals.OutputTokens)
			if tc.trashAction != "" {
				if tc.localTrash {
					require.NoError(t, source.SoftDeleteSession(ctx, thread))
					_, err = syncer.Push(ctx, true, nil)
					require.NoError(t, err)
				} else {
					require.NoError(t, store.SoftDeleteSession(ctx, thread))
				}
				// Run the actual one-time PostgreSQL upgrade on legacy trash.
				_, err = syncer.pg.ExecContext(ctx, `ALTER TABLE sessions DROP COLUMN trash_includes_codex_pages, DROP COLUMN source_trash_includes_codex_pages;
					ALTER TABLE excluded_sessions DROP COLUMN include_codex_pages`)
				require.NoError(t, err)
				_, err = syncer.pg.ExecContext(ctx, `DELETE FROM sync_metadata WHERE key=$1`, codexThreadScopeMetadataKey)
				require.NoError(t, err)
				require.NoError(t, EnsureSchema(ctx, syncer.pg, schema))
			}
			require.NoError(t, source.Close())
			raw, err := sql.Open("sqlite3", sourcePath)
			require.NoError(t, err)
			_, err = raw.ExecContext(ctx, "PRAGMA user_version=129")
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			rebuilt := testDB(t)
			require.NoError(t, rebuilt.CopyArchiveIdentityFrom(sourcePath))
			require.NoError(t, rebuilt.CopySyncStateFrom(sourcePath))
			// Resync copies trash before parsing; trashed files are not reparsed.
			_, err = rebuilt.CopyTrashedDataFrom(sourcePath)
			require.NoError(t, err)
			sess.ID = page
			sess.ParentSessionID = new(thread)
			sess.RelationshipType = "continuation"
			if !tc.localTrash {
				require.NoError(t, rebuilt.UpsertSession(ctx, sess))
				message.SessionID = page
				require.NoError(t, rebuilt.InsertMessages(ctx, []db.Message{message}))
				require.NoError(t, rebuilt.SetSessionDataVersion(ctx, page, db.CurrentDataVersion()))
			}
			_, err = rebuilt.CopyOrphanedDataFromExcluding(sourcePath, nil)
			require.NoError(t, err)
			require.NoError(t, rebuilt.CopySessionMetadataFrom(sourcePath))
			localHead, err := rebuilt.GetSessionFull(ctx, thread)
			require.NoError(t, err)
			if tc.localTrash {
				require.NotNil(t, localHead)
				require.Nil(t, localHead.FilePath)
				require.Zero(t, localHead.MessageCount)
				require.Zero(t, localHead.DataVersion)
			} else {
				require.Nil(t, localHead)
			}
			if tc.restoreLocal != "" {
				restoredID := page
				if tc.restoreLocal == "thread" {
					restoredID = thread
				}
				n, err := rebuilt.RestoreSession(ctx, restoredID)
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
				localHead, err = rebuilt.GetSessionFull(ctx, thread)
				require.NoError(t, err)
				require.NotNil(t, localHead)
				require.False(t, localHead.TrashIncludesCodexPages)
				require.Less(t, localHead.DataVersion, 130)
			}
			// Lexical push order puts 48 earlier sessions and the anchor/page in
			// one batch; 49 puts the empty anchor at the end of the first batch.
			for i := range tc.precedingSessions {
				require.NoError(t, rebuilt.UpsertSession(ctx, db.Session{
					ID: fmt.Sprintf("claude:earlier-%03d", i), Agent: "claude", Project: "sample", Machine: "machine",
				}))
			}
			upgraded, err := New(pgURL, schema, rebuilt, "machine", true, storage.PusherOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
			if tc.newerPage {
				// A page already published by an earlier push has its own newer
				// curation. Seed it through the normal session/message writers.
				upgraded.archiveID, err = rebuilt.GetArchiveID(ctx)
				require.NoError(t, err)
				marker, err := upgraded.pushMarkerID(ctx)
				require.NoError(t, err)
				pageSession, err := rebuilt.GetArtifactExportSession(ctx, page)
				require.NoError(t, err)
				require.NotNil(t, pageSession)
				tx, err := upgraded.pg.BeginTx(ctx, nil)
				require.NoError(t, err)
				t.Cleanup(func() { _ = tx.Rollback() })
				require.NoError(t, upgraded.pushSession(ctx, tx, *pageSession, marker, nil))
				_, err = upgraded.pushMessages(ctx, tx, page, true, nil, nil)
				require.NoError(t, err)
				require.NoError(t, tx.Commit())
				require.NoError(t, store.RenameSession(ctx, page, new("newer page title")))
				starred, err := store.StarSession(ctx, page)
				require.NoError(t, err)
				require.True(t, starred)
				require.NoError(t, store.UnstarSession(ctx, page))
				_, err = store.PinMessage(ctx, page, 0, new("newer note"))
				require.NoError(t, err)
				require.NoError(t, store.SoftDeleteSession(ctx, page))
			}
			for range 2 {
				var batchEnds []int
				pushed, err = upgraded.Push(ctx, true, func(progress storage.PushProgress) {
					if progress.Phase == "" && progress.SessionsDone > 0 {
						batchEnds = append(batchEnds, progress.SessionsDone)
					}
				})
				require.NoError(t, err)
				require.Zero(t, pushed.Errors)
				switch tc.precedingSessions {
				case 48:
					assert.Equal(t, []int{50}, batchEnds)
				case 49:
					assert.Equal(t, []int{50, 51}, batchEnds)
				}
			}
			visible, err := store.ListSessions(ctx, db.SessionFilter{IDs: []string{thread, page}, IDsExact: true})
			require.NoError(t, err)
			headMessages, err := store.GetMessages(ctx, thread, 0, 10, true)
			require.NoError(t, err)
			assert.Empty(t, headMessages, "the retired thread must not retain the page transcript")
			pageMessages, err := store.GetMessages(ctx, page, 0, 10, true)
			require.NoError(t, err)
			require.Len(t, pageMessages, 1)
			assert.Equal(t, "retained page answer", pageMessages[0].Content)
			pageRow, err := store.GetSessionFull(ctx, page)
			require.NoError(t, err)
			require.NotNil(t, pageRow)
			wantTitle := "remote page title"
			if tc.newerPage {
				wantTitle = "newer page title"
			}
			require.NotNil(t, pageRow.DisplayName)
			assert.Equal(t, wantTitle, *pageRow.DisplayName)
			stars, err := store.ListStarredSessionIDs(ctx)
			require.NoError(t, err)
			if tc.newerPage {
				assert.Empty(t, stars, "an existing unstarred page keeps its newer choice")
			} else {
				assert.Equal(t, []string{page}, stars)
			}
			pins, err := store.ListPinnedMessages(ctx, page, "")
			require.NoError(t, err)
			if tc.pinned {
				require.Len(t, pins, 1)
				assert.Equal(t, page, pins[0].SessionID)
				require.NotNil(t, pins[0].Note)
				wantNote := "remote note"
				if tc.newerPage {
					wantNote = "newer note"
				}
				assert.Equal(t, wantNote, *pins[0].Note)
			} else {
				assert.Empty(t, pins)
			}
			if tc.restoreLocal != "" {
				anchor, err := store.GetSessionFull(ctx, thread)
				require.NoError(t, err)
				require.NotNil(t, anchor)
				if tc.restoreLocal == "thread" {
					assert.Nil(t, anchor.DeletedAt)
					assert.NotNil(t, pageRow.DeletedAt)
				} else {
					assert.NotNil(t, anchor.DeletedAt)
					assert.Nil(t, pageRow.DeletedAt)
				}
				return
			}
			if tc.trashAction == "" {
				if tc.newerPage {
					assert.Zero(t, visible.Total, "the existing page must retain its own trash state")
					require.NotNil(t, pageRow.DeletedAt)
					n, err := store.RestoreSession(ctx, page)
					require.NoError(t, err)
					require.EqualValues(t, 1, n)
					_, err = upgraded.Push(ctx, true, nil)
					require.NoError(t, err)
					visible, err = store.ListSessions(ctx, db.SessionFilter{})
					require.NoError(t, err)
				}
				assert.Equal(t, 1, visible.Total, "only the surviving rollout remains visible")
				after, err := store.GetDailyUsage(ctx, filter)
				require.NoError(t, err)
				assert.Equal(t, before.Totals, after.Totals, "the upgrade must not double billed usage")
				return
			}
			assert.Zero(t, visible.Total)
			require.NotNil(t, pageRow.DeletedAt)
			anchor, err := store.GetSessionFull(ctx, thread)
			require.NoError(t, err)
			require.NotNil(t, anchor)
			assert.Zero(t, anchor.MessageCount)
			assert.Zero(t, anchor.TotalOutputTokens)
			require.NotNil(t, anchor.DeletedAt)
			if tc.trashAction == "purge" {
				n, err := store.DeleteSessionIfTrashed(ctx, thread)
				require.NoError(t, err)
				assert.EqualValues(t, 2, n)
			} else {
				restoredID := page
				if tc.trashAction == "thread" {
					restoredID = thread
				}
				n, err := store.RestoreSession(ctx, restoredID)
				require.NoError(t, err)
				assert.EqualValues(t, 1, n)
			}
			returning := thread + "_33333333-3333-4333-8333-333333333333"
			err = rebuilt.UpsertSession(ctx, db.Session{ID: returning, Agent: "codex", Project: "sample", Machine: "machine"})
			if tc.localTrash {
				require.ErrorIs(t, err, db.ErrSessionTrashed)
			} else {
				require.NoError(t, err)
			}
			pushed, err = upgraded.Push(ctx, true, nil)
			require.NoError(t, err)
			require.Zero(t, pushed.Errors)
			returned, err := store.GetSessionFull(ctx, returning)
			require.NoError(t, err)
			if tc.localTrash {
				assert.Nil(t, returned, "the source still blocks imports covered by its own trash scope")
			} else if tc.trashAction == "purge" {
				assert.Nil(t, returned, "purging the empty anchor must exclude returning pages")
			} else {
				require.NotNil(t, returned)
				assert.Nil(t, returned.DeletedAt, "restoring a member must end inherited trash")
			}
			after, err := store.GetDailyUsage(ctx, filter)
			require.NoError(t, err)
			if tc.trashAction == "page" {
				assert.Equal(t, before.Totals, after.Totals, "restoring the page must count the saved usage once")
			} else {
				assert.Zero(t, after.Totals.InputTokens)
				assert.Zero(t, after.Totals.OutputTokens)
			}
		})
	}
}
