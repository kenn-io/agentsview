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
	_, err = raw.ExecContext(ctx, "PRAGMA user_version=122")
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
