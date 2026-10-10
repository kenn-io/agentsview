//go:build pgtest

package postgres

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

const (
	codexTrashThread    = "codex:11111111-1111-4111-8111-111111111111"
	codexTrashPage      = codexTrashThread + "_22222222-2222-4222-8222-222222222222"
	codexTrashSibling   = codexTrashThread + "_33333333-3333-4333-8333-333333333333"
	codexTrashReturning = codexTrashThread + "_44444444-4444-4444-8444-444444444444"
)

func TestPreSplitPGTrashKeepsPagesHidden(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, agent string
	}{
		{"restore_page", "codex:", "codex"},
		{"restore_thread", "host-a~traex:", "traex"},
		{"purge_thread", "augure-code:", "augure-code"},
		{"new_per_file", "codex:", "codex"},
		{"other_archive", "codex:", "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			const schema = "agentsview_presplit_pgtrash_test"
			pgURL := testPGURL(t)
			cleanNamedPGSchema(t, pgURL, schema)
			t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
			pg, err := Open(pgURL, schema, true)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, pg.Close()) })
			require.NoError(t, EnsureSchema(ctx, pg, schema))
			thread := tc.prefix + "11111111-1111-4111-8111-111111111111"
			page := thread + "_22222222-2222-4222-8222-222222222222"
			returning := thread + "_33333333-3333-4333-8333-333333333333"
			local := testDB(t)
			require.NoError(t, local.UpsertSession(ctx, db.Session{ID: thread, Agent: tc.agent, Project: "sample", Machine: "machine"}))
			syncer := &Sync{pg: pg, local: local, machine: "machine", schema: schema, schemaDone: true}
			_, err = syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			store := &Store{pg: pg}
			require.NoError(t, store.SoftDeleteSession(ctx, thread))
			if tc.name != "new_per_file" {
				// Recreate the pre-split schema after an actual PostgreSQL-only
				// trash action. The source archive remains active throughout.
				_, err = pg.ExecContext(ctx, `ALTER TABLE sessions DROP COLUMN trash_includes_codex_pages, DROP COLUMN source_trash_includes_codex_pages;
					ALTER TABLE excluded_sessions DROP COLUMN include_codex_pages`)
				require.NoError(t, err)
				_, err = pg.ExecContext(ctx, `DELETE FROM sync_metadata WHERE key=$1`, codexThreadScopeMetadataKey)
				require.NoError(t, err)
			}
			require.NoError(t, EnsureSchema(ctx, pg, schema))
			if tc.name == "other_archive" {
				local = testDB(t)
				syncer.local = local
			}
			child := db.Session{ID: page, Agent: tc.agent, Project: "sample", Machine: "machine", ParentSessionID: new(thread), RelationshipType: "continuation"}
			require.NoError(t, local.UpsertSession(ctx, child))
			_, err = syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			got, err := store.GetSessionFull(ctx, page)
			require.NoError(t, err)
			require.NotNil(t, got)
			if tc.name == "new_per_file" || tc.name == "other_archive" {
				assert.Nil(t, got.DeletedAt)
				return
			}
			assert.NotNil(t, got.DeletedAt, "the old PostgreSQL trash action must cover its split page")
			child.Project = "updated-project"
			require.NoError(t, local.UpsertSession(ctx, child))
			_, err = syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			var sourceDeleted sql.NullTime
			require.NoError(t, pg.QueryRowContext(ctx, `SELECT source_deleted_at FROM sessions WHERE id=$1`, page).Scan(&sourceDeleted))
			assert.False(t, sourceDeleted.Valid, "inherited PostgreSQL trash is not a source deletion")
			if tc.name == "purge_thread" {
				n, err := store.DeleteSessionIfTrashed(ctx, thread)
				require.NoError(t, err)
				assert.EqualValues(t, 2, n)
			} else {
				restoredID := page
				if tc.name == "restore_thread" {
					restoredID = thread
				}
				n, err := store.RestoreSession(ctx, restoredID)
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
			}
			require.NoError(t, EnsureSchema(ctx, pg, schema))
			require.NoError(t, local.UpsertSession(ctx, db.Session{ID: returning, Agent: tc.agent, Project: "sample", Machine: "machine"}))
			_, err = syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			got, err = store.GetSessionFull(ctx, returning)
			require.NoError(t, err)
			if tc.name == "purge_thread" {
				assert.Nil(t, got, "purged thread scope also excludes returning pages")
			} else {
				require.NotNil(t, got)
				assert.Nil(t, got.DeletedAt, "restoring a member ends inherited thread trash")
				trash, err := store.ListTrashedSessions(ctx)
				require.NoError(t, err)
				assert.Len(t, trash, 1, "restore leaves the other materialized member in trash")
			}
		})
	}
}

// Exercise the real archive upgrade and push boundary, including pages whose
// messages were materialized before the legacy deletion scope was restored.
func newCodexTrashMirror(t *testing.T) (*db.DB, *Sync, *Store) {
	t.Helper()
	ctx := t.Context()
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	source, err := db.OpenIsolated(ctx, sourcePath)
	require.NoError(t, err)
	require.NoError(t, source.UpsertSession(ctx, db.Session{ID: codexTrashThread, Agent: "codex", Project: "sample", Machine: "machine"}))
	require.NoError(t, source.SoftDeleteSession(ctx, codexTrashThread))
	require.NoError(t, source.Close())
	raw, err := sql.Open("sqlite3", sourcePath)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, "PRAGMA user_version=129")
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	local, err := db.OpenIsolated(ctx, filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })
	require.NoError(t, local.CopyArchiveIdentityFrom(sourcePath))
	_, err = local.CopyTrashedDataFrom(sourcePath)
	require.NoError(t, err)
	for _, id := range []string{codexTrashPage, codexTrashSibling} {
		require.NoError(t, local.UpsertSession(ctx, db.Session{ID: id, Agent: "codex", Project: "sample", Machine: "machine", ParentSessionID: new(codexTrashThread), RelationshipType: "continuation"}))
		require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: id, Role: "user", Content: "Saved page", SourceUUID: id}}))
	}
	require.NoError(t, local.CopySessionMetadataFrom(sourcePath))
	// Start outside the next incremental push window. A page restore must
	// advance its anchor's marker and fingerprint even though the anchor's
	// other mirrored metadata stays unchanged.
	raw, err = sql.Open("sqlite3", local.Path())
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, `UPDATE sessions SET created_at='2020-01-01T00:00:00.000Z', local_modified_at='2020-01-01T00:00:00.000Z'`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	const schema = "agentsview_codex_trash_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, EnsureSchema(ctx, pg, schema))
	syncer := &Sync{pg: pg, local: local, machine: "machine", schema: schema, schemaDone: true}
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	return local, syncer, &Store{pg: pg}
}

func TestCodexLegacyTrashPurgeMirror(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "thread"
		if empty {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			local, syncer, store := newCodexTrashMirror(t)
			ctx := t.Context()
			if empty {
				count, err := store.EmptyTrash(ctx)
				require.NoError(t, err)
				assert.Equal(t, 3, count)
			} else {
				count, err := store.DeleteSessionIfTrashed(ctx, codexTrashThread)
				require.NoError(t, err)
				assert.EqualValues(t, 3, count)
			}
			for _, id := range []string{codexTrashThread, codexTrashPage, codexTrashSibling} {
				session, err := store.GetSessionFull(ctx, id)
				require.NoError(t, err)
				assert.Nil(t, session, "purging the thread must remove its saved pages")
			}
			// A later source restore and a newly discovered file cannot undo the
			// permanent whole-thread exclusion chosen in PostgreSQL.
			_, err := local.RestoreSession(ctx, codexTrashThread)
			require.NoError(t, err)
			require.NoError(t, local.UpsertSession(ctx, db.Session{ID: codexTrashReturning, Agent: "codex", Project: "sample", Machine: "machine"}))
			if !empty {
				// The per-session write must also reject a thread exclusion
				// created after the push's batched candidate check.
				returning, err := local.GetSessionFull(ctx, codexTrashReturning)
				require.NoError(t, err)
				require.NotNil(t, returning)
				marker, err := syncer.pushMarkerID(ctx)
				require.NoError(t, err)
				tx, err := store.pg.BeginTx(ctx, nil)
				require.NoError(t, err)
				err = syncer.pushSession(ctx, tx, *returning, marker, nil)
				assert.ErrorIs(t, err, errSessionExcluded)
				require.NoError(t, tx.Rollback())
			}
			_, err = syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			trash, err := store.ListTrashedSessions(ctx)
			require.NoError(t, err)
			assert.Empty(t, trash)
			session, err := store.GetSessionFull(ctx, codexTrashReturning)
			require.NoError(t, err)
			assert.Nil(t, session, "returning pages must stay permanently excluded")
		})
	}
}

func TestCodexLegacyTrashRestoreMirror(t *testing.T) {
	for _, localRestore := range []bool{false, true} {
		for _, id := range []string{codexTrashThread, codexTrashPage} {
			name := "remote/" + id
			if localRestore {
				name = "local/" + id
			}
			t.Run(name, func(t *testing.T) {
				local, syncer, store := newCodexTrashMirror(t)
				ctx := t.Context()
				var err error
				var count int64
				if localRestore {
					count, err = local.RestoreSession(ctx, id)
				} else {
					count, err = store.RestoreSession(ctx, id)
				}
				require.NoError(t, err)
				require.EqualValues(t, 1, count)
				_, err = syncer.Push(ctx, false, nil)
				require.NoError(t, err)
				anchor, err := store.GetSessionFull(ctx, codexTrashThread)
				require.NoError(t, err)
				require.NotNil(t, anchor)
				assert.False(t, anchor.TrashIncludesCodexPages, "the next incremental push must carry the ended thread action")
				// A later full push must preserve remote curation, including the
				// anchor's cleared scope after restoring only one child.
				_, err = syncer.Push(ctx, true, nil)
				require.NoError(t, err)
				restored, err := store.GetSession(ctx, id)
				require.NoError(t, err)
				require.NotNil(t, restored)
				assert.Nil(t, restored.DeletedAt)
				removed, err := store.EmptyTrash(ctx)
				require.NoError(t, err)
				assert.Equal(t, 2, removed)
				restored, err = store.GetSessionFull(ctx, id)
				require.NoError(t, err)
				assert.NotNil(t, restored, "purging the remaining trash must retain the restored file")
				_, err = local.RestoreSession(ctx, codexTrashThread)
				require.NoError(t, err)
				require.NoError(t, local.UpsertSession(ctx, db.Session{ID: codexTrashReturning, Agent: "codex", Project: "sample", Machine: "machine"}))
				_, err = syncer.Push(ctx, true, nil)
				require.NoError(t, err)
				returning, err := store.GetSession(ctx, codexTrashReturning)
				require.NoError(t, err)
				assert.NotNil(t, returning, "restoring a member ends the inherited whole-thread action")
			})
		}
	}
}

func TestCodexLegacyTrashPagePurgeStaysPerFile(t *testing.T) {
	local, syncer, store := newCodexTrashMirror(t)
	ctx := t.Context()
	removed, err := store.DeleteSessionIfTrashed(ctx, codexTrashPage)
	require.NoError(t, err)
	assert.EqualValues(t, 1, removed)
	trash, err := store.ListTrashedSessions(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{codexTrashThread, codexTrashSibling}, sessionIDs(trash))
	_, err = local.RestoreSession(ctx, codexTrashThread)
	require.NoError(t, err)
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: codexTrashReturning, Agent: "codex", Project: "sample", Machine: "machine"}))
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	returning, err := store.GetSession(ctx, codexTrashReturning)
	require.NoError(t, err)
	assert.NotNil(t, returning)
	purged, err := store.GetSessionFull(ctx, codexTrashPage)
	require.NoError(t, err)
	assert.Nil(t, purged)
}

func TestCodexTrashScopeSchemaMigrationKeepsArchive(t *testing.T) {
	_, syncer, store := newCodexTrashMirror(t)
	ctx := t.Context()
	_, err := store.pg.ExecContext(ctx, `ALTER TABLE sessions DROP COLUMN trash_includes_codex_pages, DROP COLUMN source_trash_includes_codex_pages;
 ALTER TABLE excluded_sessions DROP COLUMN include_codex_pages;
 INSERT INTO excluded_sessions(id) VALUES('ordinary-exclusion')`)
	require.NoError(t, err)
	for range 2 {
		require.NoError(t, EnsureSchema(ctx, store.pg, syncer.schema))
	}
	trash, err := store.ListTrashedSessions(ctx)
	require.NoError(t, err)
	assert.Len(t, trash, 3)
	messages, err := store.GetMessages(ctx, codexTrashPage, 0, 10, true)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "Saved page", messages[0].Content)
	var ordinaryExcluded, scoped bool
	require.NoError(t, store.pg.QueryRowContext(ctx, `SELECT true,include_codex_pages FROM excluded_sessions WHERE id='ordinary-exclusion'`).Scan(&ordinaryExcluded, &scoped))
	assert.True(t, ordinaryExcluded)
	assert.False(t, scoped, "existing per-file exclusions must not widen")
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	removed, err := store.DeleteSessionIfTrashed(ctx, codexTrashThread)
	require.NoError(t, err)
	assert.EqualValues(t, 3, removed)
}

func TestHostedLegacyCodexTrashScope(t *testing.T) {
	for _, action := range []string{"purge", "empty", "restore_page", "restore_thread"} {
		t.Run(action, func(t *testing.T) {
			f := newHostedFixture(t, "tenant-codex-trash")
			ctx := t.Context()
			// The runtime role must be able to strengthen an existing per-file
			// tombstone when a whole-thread purge follows it.
			_, err := f.runtime.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,deleted_at,trash_includes_codex_pages,source_trash_includes_codex_pages)
   VALUES($1,'sample','machine','codex',NOW(),true,true),($2,'sample','machine','codex',NOW(),false,false),($3,'sample','machine','codex',NOW(),false,false);
   `, codexTrashThread, codexTrashPage, codexTrashSibling)
			require.NoError(t, err)
			_, err = f.runtime.ExecContext(ctx, `INSERT INTO sessions(id,project,machine,agent,provenance_kind) VALUES($1,'sample','machine','codex','raw')`, codexTrashReturning)
			require.NoError(t, err)
			_, err = f.runtime.ExecContext(ctx, `INSERT INTO excluded_sessions(id) VALUES($1)`, codexTrashThread)
			require.NoError(t, err)
			h, err := newHostedAdapter(f.runtime, f.tenant)
			require.NoError(t, err)
			wantRemoved := 3
			restoredID := ""
			if action == "restore_page" {
				restoredID = codexTrashPage
			}
			if action == "restore_thread" {
				restoredID = codexTrashThread
			}
			if restoredID != "" {
				n, err := h.RestoreSession(ctx, restoredID)
				require.NoError(t, err)
				assert.EqualValues(t, 1, n)
				wantRemoved = 2
			}
			var removed int
			if action == "purge" {
				n, err := h.DeleteSessionIfTrashed(ctx, codexTrashThread)
				require.NoError(t, err)
				removed = int(n)
			} else {
				removed, err = h.EmptyTrash(ctx)
				require.NoError(t, err)
			}
			assert.Equal(t, wantRemoved, removed)
			var rawCount int
			require.NoError(t, f.runtime.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE id=$1 AND provenance_kind='raw'`, codexTrashReturning).Scan(&rawCount))
			assert.Equal(t, 1, rawCount, "legacy deletion must not reach a raw projection")
			if restoredID != "" {
				restored, err := h.GetSession(ctx, restoredID)
				require.NoError(t, err)
				assert.NotNil(t, restored)
			}
		})
	}
}

func TestCodexTrashScopeUpgradePreservesLaterPerFileDeletion(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		missingBaseline, currentColumns, remoteTrash bool
	}{
		{"baseline_present", false, false, false},
		{"older_schema_local_trash", true, false, false},
		{"older_schema_remote_trash", true, false, true},
		{"unfinished_baseline_fast_path", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			const schema = "agentsview_codex_scope_upgrade_test"
			pgURL := testPGURL(t)
			cleanNamedPGSchema(t, pgURL, schema)
			t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
			pg, err := Open(pgURL, schema, true)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, pg.Close()) })
			require.NoError(t, EnsureSchema(ctx, pg, schema))
			store := &Store{pg: pg}

			source := testDB(t)
			sourcePath := source.Path()
			sourceFile := filepath.Join(t.TempDir(), "rollout-2026-01-01T00-00-00-11111111-1111-4111-8111-111111111111.jsonl")
			require.NoError(t, source.UpsertSession(ctx, db.Session{ID: codexTrashThread, Agent: "codex", Project: "sample", Machine: "machine", DataVersion: 129, FilePath: &sourceFile, MessageCount: 1, UserMessageCount: 1}))
			require.NoError(t, source.InsertMessages(ctx, []db.Message{{SessionID: codexTrashThread, Role: "user", Content: "Original retained message", SourceUUID: "original-message"}}))
			require.NoError(t, source.SoftDeleteSession(ctx, codexTrashThread))
			syncer := &Sync{pg: pg, local: source, machine: "machine", schema: schema, schemaDone: true}
			_, err = syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			before, err := store.GetSessionFull(ctx, codexTrashThread)
			require.NoError(t, err)
			require.NotNil(t, before)
			require.NotNil(t, before.DeletedAt)
			// Restore historical migration inputs, including an interrupted
			// upgrade whose columns exist but whose data backfills have not run.
			if !tc.currentColumns {
				_, err = pg.ExecContext(ctx, `ALTER TABLE sessions DROP COLUMN trash_includes_codex_pages, DROP COLUMN source_trash_includes_codex_pages;
					ALTER TABLE excluded_sessions DROP COLUMN include_codex_pages`)
				require.NoError(t, err)
			}
			_, err = pg.ExecContext(ctx, `DELETE FROM sync_metadata WHERE key=$1`, codexThreadScopeMetadataKey)
			require.NoError(t, err)
			if tc.missingBaseline {
				if tc.currentColumns {
					_, err = pg.ExecContext(ctx, `UPDATE sessions SET source_deleted_at=NULL`)
				} else {
					_, err = pg.ExecContext(ctx, `ALTER TABLE sessions DROP COLUMN source_deleted_at, DROP COLUMN source_display_name`)
				}
				require.NoError(t, err)
				_, err = pg.ExecContext(ctx, `DELETE FROM sync_metadata WHERE key=$1`, sourceCurationBackfillMetadataKey)
				require.NoError(t, err)
			}
			require.NoError(t, source.Close())
			raw, err := sql.Open("sqlite3", sourcePath)
			require.NoError(t, err)
			_, err = raw.ExecContext(ctx, `PRAGMA user_version=129`)
			require.NoError(t, err)
			require.NoError(t, raw.Close())
			local, err := db.OpenIsolated(ctx, filepath.Join(t.TempDir(), "rebuilt.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, local.Close()) })
			require.NoError(t, local.CopyArchiveIdentityFrom(sourcePath))
			require.NoError(t, local.CopySyncStateFrom(sourcePath))
			_, err = local.CopyTrashedDataFrom(sourcePath)
			require.NoError(t, err)
			require.NoError(t, local.UpsertSession(ctx, db.Session{ID: codexTrashPage, Agent: "codex", Project: "sample", Machine: "machine", DataVersion: db.CurrentDataVersion(), ParentSessionID: new(codexTrashThread), RelationshipType: "continuation", MessageCount: 1, UserMessageCount: 1}))
			require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: codexTrashPage, Role: "user", Content: "Retained sibling page", SourceUUID: "page-message"}}))
			require.NoError(t, local.CopySessionMetadataFrom(sourcePath))
			for _, id := range []string{codexTrashThread, codexTrashPage} {
				sess, err := local.GetSessionFull(ctx, id)
				require.NoError(t, err)
				require.NotNil(t, sess)
				require.NotNil(t, sess.DeletedAt, "pre129 upgrade inherits thread trash")
				n, err := local.RestoreSession(ctx, id)
				require.NoError(t, err)
				require.EqualValues(t, 1, n)
			}
			localHead, err := local.GetSessionFull(ctx, codexTrashThread)
			require.NoError(t, err)
			require.NotNil(t, localHead)
			require.False(t, localHead.TrashIncludesCodexPages)
			require.Nil(t, localHead.DeletedAt)
			syncer.local = local
			syncer.schemaDone = false
			for range 2 {
				_, err = syncer.Push(ctx, true, nil)
				require.NoError(t, err)
			}
			// Re-running migration entry points must not revive the old action.
			require.NoError(t, EnsureSchema(ctx, pg, schema))
			head, err := store.GetSessionFull(ctx, codexTrashThread)
			require.NoError(t, err)
			require.NotNil(t, head)
			assert.Nil(t, head.DeletedAt)
			assert.False(t, head.TrashIncludesCodexPages, "local restore must end inherited thread trash")
			page, err := store.GetSession(ctx, codexTrashPage)
			require.NoError(t, err)
			require.NotNil(t, page, "saved sibling is visible before the new per-file trash action")
			require.Nil(t, page.DeletedAt)
			if tc.remoteTrash {
				require.NoError(t, store.SoftDeleteSession(ctx, codexTrashThread))
			} else {
				require.NoError(t, local.SoftDeleteSession(ctx, codexTrashThread))
				head, err := local.GetSessionFull(ctx, codexTrashThread)
				require.NoError(t, err)
				require.False(t, head.TrashIncludesCodexPages, "new local trash is per-file")
			}
			require.NoError(t, local.UpsertSession(ctx, db.Session{ID: codexTrashReturning, Agent: "codex", Project: "sample", Machine: "machine", DataVersion: db.CurrentDataVersion()}))
			_, err = syncer.Push(ctx, true, nil)
			require.NoError(t, err)
			returning, err := store.GetSessionFull(ctx, codexTrashReturning)
			require.NoError(t, err)
			require.NotNil(t, returning)
			assert.Nil(t, returning.DeletedAt, "per-file trash must not hide another page")
			page, err = store.GetSessionFull(ctx, codexTrashPage)
			require.NoError(t, err)
			require.NotNil(t, page)
			assert.Nil(t, page.DeletedAt, "per-file trash must not hide the existing saved page")
			removed, err := store.DeleteSessionIfTrashed(ctx, codexTrashThread)
			require.NoError(t, err)
			assert.EqualValues(t, 1, removed, "per-file purge must only remove the thread")
			page, err = store.GetSessionFull(ctx, codexTrashPage)
			require.NoError(t, err)
			assert.NotNil(t, page, "per-file purge must preserve an active sibling page")
			returning, err = store.GetSessionFull(ctx, codexTrashReturning)
			require.NoError(t, err)
			assert.NotNil(t, returning, "per-file purge must preserve a returning page")
		})
	}
}
