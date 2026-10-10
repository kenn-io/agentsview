//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestCodexTrashScopeRespectsArchiveOwnership(t *testing.T) {
	for _, tc := range []struct {
		name, childAgent, childMachine, legacyMachine string
		otherArchive, legacyOwner, belongs, renamed   bool
	}{
		{"same_archive", "codex", "machine", "", false, false, true, false},
		{"other_archive", "codex", "machine", "", true, false, false, false},
		{"other_agent", "traex", "machine", "", false, false, false, false},
		{"legacy_same_machine", "codex", "machine", "machine", true, true, true, false},
		{"legacy_other_machine", "codex", "other-machine", "machine", true, true, false, false},
		{"legacy_local", "codex", "machine", "local", true, true, true, false},
		{"legacy_empty", "codex", "machine", "", true, true, true, false},
		{"legacy_renamed", "codex", "new-machine", "old-machine", true, true, true, true},
	} {
		for _, action := range []string{"purge", "empty", "restore"} {
			t.Run(tc.name+"/"+action, func(t *testing.T) {
				ctx := t.Context()
				const schema = "agentsview_scope_ownership_probe"
				pgURL := testPGURL(t)
				cleanNamedPGSchema(t, pgURL, schema)
				t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
				pg, err := Open(pgURL, schema, true)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, pg.Close()) })
				require.NoError(t, EnsureSchema(ctx, pg, schema))
				store := &Store{pg: pg}
				archiveA := testDB(t)
				parentMachine := "machine"
				if tc.renamed {
					parentMachine = "old-machine"
				}
				require.NoError(t, archiveA.UpsertSession(ctx, db.Session{ID: codexTrashThread, Agent: "codex", Project: "sample", Machine: parentMachine, DataVersion: 129, MessageCount: 1}))
				require.NoError(t, archiveA.InsertMessages(ctx, []db.Message{{SessionID: codexTrashThread, Role: "user", Content: "Archive A thread", SourceUUID: "archive-a-thread"}}))
				pushA := &Sync{pg: pg, local: archiveA, machine: parentMachine, schema: schema, schemaDone: true}
				_, err = pushA.Push(ctx, true, nil)
				require.NoError(t, err)
				require.NoError(t, store.SoftDeleteSession(ctx, codexTrashThread))
				// Only the pre-page-split schema shape is restored; all curation above
				// was performed through the public production API.
				_, err = pg.ExecContext(ctx, `ALTER TABLE sessions DROP COLUMN trash_includes_codex_pages, DROP COLUMN source_trash_includes_codex_pages; ALTER TABLE excluded_sessions DROP COLUMN include_codex_pages`)
				require.NoError(t, err)
				_, err = pg.ExecContext(ctx, `DELETE FROM sync_metadata WHERE key=$1`, codexThreadScopeMetadataKey)
				require.NoError(t, err)
				require.NoError(t, EnsureSchema(ctx, pg, schema))
				if tc.legacyOwner {
					_, err = pg.ExecContext(ctx, `UPDATE sessions SET owner_marker='',machine=$2 WHERE id=$1`, codexTrashThread, tc.legacyMachine)
					require.NoError(t, err)
				}
				archiveB := archiveA
				if tc.otherArchive {
					archiveB = testDB(t)
				}
				if tc.renamed {
					marker, err := pushA.pushMarkerID(ctx)
					require.NoError(t, err)
					require.NoError(t, archiveB.SetSyncState(ctx, pushMarkerIDStateKey, marker))
				}
				pushB := &Sync{pg: pg, local: archiveB, machine: tc.childMachine, schema: schema, schemaDone: true}
				require.NoError(t, archiveB.UpsertSession(ctx, db.Session{ID: codexTrashPage, Agent: tc.childAgent, Project: "sample", Machine: tc.childMachine, DataVersion: db.CurrentDataVersion(), MessageCount: 1, ParentSessionID: new(codexTrashThread), RelationshipType: "continuation"}))
				require.NoError(t, archiveB.InsertMessages(ctx, []db.Message{{SessionID: codexTrashPage, Role: "user", Content: "Retained page", SourceUUID: "retained-page"}}))
				_, err = pushB.Push(ctx, true, nil)
				require.NoError(t, err)
				page, err := store.GetSessionFull(ctx, codexTrashPage)
				require.NoError(t, err)
				require.NotNil(t, page)
				require.Equal(t, tc.belongs, page.DeletedAt != nil, "inherited_trash must discriminate owners before the action")
				var parentOwner, pageOwner, parentArchive, pageArchive string
				require.NoError(t, pg.QueryRowContext(ctx, `SELECT p.owner_marker,c.owner_marker,p.source_archive_id,c.source_archive_id FROM sessions p JOIN sessions c ON c.id=$2 WHERE p.id=$1`, codexTrashThread, codexTrashPage).Scan(&parentOwner, &pageOwner, &parentArchive, &pageArchive))
				require.Equal(t, !tc.otherArchive && !tc.legacyOwner, parentOwner == pageOwner)
				require.Equal(t, !tc.otherArchive, parentArchive == pageArchive)
				if action == "restore" {
					if !tc.belongs {
						require.NoError(t, store.SoftDeleteSession(ctx, codexTrashPage))
					}
					n, err := store.RestoreSession(ctx, codexTrashPage)
					require.NoError(t, err)
					require.EqualValues(t, 1, n)
					head, err := store.GetSessionFull(ctx, codexTrashThread)
					require.NoError(t, err)
					require.NotNil(t, head)
					assert.Equal(t, !tc.belongs, head.TrashIncludesCodexPages, "restoring a foreign archive page must not end the parent's independent action")
					require.NoError(t, archiveA.UpsertSession(ctx, db.Session{ID: codexTrashReturning, Agent: "codex", Project: "sample", Machine: "machine", DataVersion: db.CurrentDataVersion()}))
					_, err = pushA.Push(ctx, true, nil)
					require.NoError(t, err)
					returning, err := store.GetSessionFull(ctx, codexTrashReturning)
					require.NoError(t, err)
					require.NotNil(t, returning)
					assert.Equal(t, !tc.belongs, returning.DeletedAt != nil, "archive A scope must still apply after restoring only archive B")
					return
				}
				var removed int64
				if action == "purge" {
					removed, err = store.DeleteSessionIfTrashed(ctx, codexTrashThread)
				} else {
					var n int
					n, err = store.EmptyTrash(ctx)
					removed = int64(n)
				}
				require.NoError(t, err)
				wantRemoved := int64(1)
				if tc.belongs {
					wantRemoved = 2
				}
				assert.Equal(t, wantRemoved, removed, "purge only the selected thread's ownership scope")
				page, err = store.GetSessionFull(ctx, codexTrashPage)
				require.NoError(t, err)
				if tc.belongs {
					assert.Nil(t, page)
				} else {
					assert.NotNil(t, page, "active foreign archive page must survive")
				}
				require.NoError(t, archiveB.UpsertSession(ctx, db.Session{ID: codexTrashReturning, Agent: tc.childAgent, Project: "sample", Machine: tc.childMachine, DataVersion: db.CurrentDataVersion()}))
				// Exercise the row writer as well as the earlier batch filter.
				incoming, err := archiveB.GetSessionFull(ctx, codexTrashReturning)
				require.NoError(t, err)
				require.NotNil(t, incoming)
				marker, err := pushB.pushMarkerID(ctx)
				require.NoError(t, err)
				tx, err := pg.BeginTx(ctx, nil)
				require.NoError(t, err)
				defer func() { _ = tx.Rollback() }()
				err = pushB.pushSession(ctx, tx, *incoming, marker, nil)
				if tc.belongs {
					require.ErrorIs(t, err, errSessionExcluded)
				} else {
					require.NoError(t, err)
				}
				require.NoError(t, tx.Commit())
				_, err = pushB.Push(ctx, true, nil)
				require.NoError(t, err)
				returning, err := store.GetSessionFull(ctx, codexTrashReturning)
				require.NoError(t, err)
				if tc.belongs {
					assert.Nil(t, returning)
				} else {
					assert.NotNil(t, returning, "scoped exclusion must not block another archive")
				}
				if tc.name == "same_archive" {
					foreign := testDB(t)
					require.NoError(t, foreign.UpsertSession(ctx, db.Session{ID: codexTrashPage, Agent: "codex", Machine: "machine", Project: "sample"}))
					foreignPush := &Sync{pg: pg, local: foreign, machine: "machine", schema: schema, schemaDone: true}
					_, err = foreignPush.Push(ctx, true, nil)
					require.NoError(t, err)
					foreignPage, err := store.GetSessionFull(ctx, codexTrashPage)
					require.NoError(t, err)
					require.NotNil(t, foreignPage, "purging descendants must not create global tombstones")

					// An explicit per-file purge still records a global exact-ID
					// decision, even when the ID was previously under a scope.
					require.NoError(t, store.SoftDeleteSession(ctx, codexTrashPage))
					removed, err := store.DeleteSessionIfTrashed(ctx, codexTrashPage)
					require.NoError(t, err)
					require.EqualValues(t, 1, removed)
					another := testDB(t)
					require.NoError(t, another.UpsertSession(ctx, db.Session{ID: codexTrashPage, Agent: "codex", Machine: "machine", Project: "sample"}))
					anotherPush := &Sync{pg: pg, local: another, machine: "machine", schema: schema, schemaDone: true}
					_, err = anotherPush.Push(ctx, true, nil)
					require.NoError(t, err)
					excludedPage, err := store.GetSessionFull(ctx, codexTrashPage)
					require.NoError(t, err)
					assert.Nil(t, excludedPage, "explicit per-file exclusions retain their global meaning")
				}
				if tc.otherArchive && !tc.belongs {
					// A later attempt from the excluded source must neither delete
					// foreign rows nor turn their IDs into global tombstones.
					for _, id := range []string{codexTrashPage, codexTrashReturning} {
						require.NoError(t, archiveA.UpsertSession(ctx, db.Session{ID: id, Agent: "codex", Machine: "machine", Project: "sample"}))
					}
					_, err = pushA.Push(ctx, true, nil)
					require.NoError(t, err)
					_, err = pushB.Push(ctx, true, nil)
					require.NoError(t, err)
					for _, id := range []string{codexTrashPage, codexTrashReturning} {
						kept, err := store.GetSessionFull(ctx, id)
						require.NoError(t, err)
						assert.NotNil(t, kept, "excluded source push must preserve the other archive")
					}
				}
			})
		}
	}
}
