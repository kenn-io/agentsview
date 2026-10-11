//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestCodexFilteredPageRestoreSyncsExcludedAnchorScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts storage.PusherOptions
	}{
		{"include_page", storage.PusherOptions{Projects: []string{"selected"}}},
		{"exclude_anchor", storage.PusherOptions{ExcludeProjects: []string{"sample"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			local, initial, store := newCodexTrashMirror(t)
			n, err := local.RestoreSession(ctx, codexTrashPage)
			require.NoError(t, err)
			require.EqualValues(t, 1, n)
			_, err = local.AssignSessionProject(ctx, codexTrashPage, "selected")
			require.NoError(t, err)
			require.NoError(t, local.InsertMessages(ctx, []db.Message{{SessionID: codexTrashThread, Role: "user", Content: "Excluded anchor transcript"}}))
			localHead, err := local.GetSessionFull(ctx, codexTrashThread)
			require.NoError(t, err)
			require.NotNil(t, localHead)
			require.False(t, localHead.TrashIncludesCodexPages)
			before, err := store.GetSessionFull(ctx, codexTrashThread)
			require.NoError(t, err)
			require.NotNil(t, before)
			require.True(t, before.TrashIncludesCodexPages)

			filtered, err := New(testPGURL(t), initial.schema, local, "machine", true, tc.opts)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, filtered.Close()) })
			pushed, err := filtered.Push(ctx, false, nil)
			require.NoError(t, err)
			require.Zero(t, pushed.Errors)
			assert.Equal(t, 1, pushed.SessionsPushed)
			page, err := store.GetSessionFull(ctx, codexTrashPage)
			require.NoError(t, err)
			require.NotNil(t, page)
			assert.Nil(t, page.DeletedAt, "filtered publication must preserve the local restore")
			head, err := store.GetSessionFull(ctx, codexTrashThread)
			require.NoError(t, err)
			require.NotNil(t, head)
			assert.False(t, head.TrashIncludesCodexPages)
			assert.Equal(t, before.DeletedAt, head.DeletedAt, "clearing scope must not restore the anchor")
			assert.Equal(t, before.Project, head.Project)
			messages, err := store.GetMessages(ctx, codexTrashThread, 0, 10, true)
			require.NoError(t, err)
			assert.Empty(t, messages, "the excluded project's transcript must not be published")

			// A later unfiltered push must not mistake inherited trash from the
			// filtered push for an independent PostgreSQL decision.
			_, err = initial.Push(ctx, true, nil)
			require.NoError(t, err)
			page, err = store.GetSession(ctx, codexTrashPage)
			require.NoError(t, err)
			require.NotNil(t, page)
			assert.Nil(t, page.DeletedAt)
			_, err = store.EmptyTrash(ctx)
			require.NoError(t, err)
			page, err = store.GetSessionFull(ctx, codexTrashPage)
			require.NoError(t, err)
			assert.NotNil(t, page, "purging the remaining trash must retain the restored page")
		})
	}
}

func TestCodexFilteredPageRestorePreservesRemoteThreadTrash(t *testing.T) {
	ctx := t.Context()
	local, initial, store := newCodexTrashMirror(t)
	_, err := store.RestoreSession(ctx, codexTrashThread)
	require.NoError(t, err)
	require.NoError(t, store.SoftDeleteSession(ctx, codexTrashThread))
	// Recreate the pre-split schema after an actual PostgreSQL-only trash
	// action, then let the production upgrade assign its whole-thread scope.
	_, err = store.pg.ExecContext(ctx, `ALTER TABLE sessions DROP COLUMN trash_includes_codex_pages, DROP COLUMN source_trash_includes_codex_pages;
		ALTER TABLE excluded_sessions DROP COLUMN include_codex_pages`)
	require.NoError(t, err)
	_, err = store.pg.ExecContext(ctx, `DELETE FROM sync_metadata WHERE key=$1`, codexThreadScopeMetadataKey)
	require.NoError(t, err)
	require.NoError(t, EnsureSchema(ctx, store.pg, initial.schema))
	var remoteOverride bool
	require.NoError(t, store.pg.QueryRowContext(ctx, `SELECT deleted_at IS DISTINCT FROM source_deleted_at FROM sessions WHERE id=$1`, codexTrashThread).Scan(&remoteOverride))
	require.True(t, remoteOverride)
	_, err = local.RestoreSession(ctx, codexTrashPage)
	require.NoError(t, err)
	_, err = local.AssignSessionProject(ctx, codexTrashPage, "selected")
	require.NoError(t, err)
	filtered, err := New(testPGURL(t), initial.schema, local, "machine", true, storage.PusherOptions{Projects: []string{"selected"}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filtered.Close()) })
	result, err := filtered.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	head, err := store.GetSessionFull(ctx, codexTrashThread)
	require.NoError(t, err)
	require.NotNil(t, head)
	assert.True(t, head.TrashIncludesCodexPages, "the source restore must preserve the independent remote trash action")
	var sourceScope bool
	require.NoError(t, store.pg.QueryRowContext(ctx, `SELECT source_trash_includes_codex_pages FROM sessions WHERE id=$1`, codexTrashThread).Scan(&sourceScope))
	assert.False(t, sourceScope, "advance the source baseline even while keeping the remote override")
	// The retained remote scope must also cover a later, previously absent page.
	require.NoError(t, local.UpsertSession(ctx, db.Session{ID: codexTrashReturning, Agent: "codex", Machine: "machine", Project: "selected"}))
	result, err = filtered.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	page, err := store.GetSessionFull(ctx, codexTrashReturning)
	require.NoError(t, err)
	require.NotNil(t, page)
	assert.NotNil(t, page.DeletedAt)
}

func TestCodexFilteredScopeSyncRespectsArchiveOwnership(t *testing.T) {
	ctx := t.Context()
	_, initial, store := newCodexTrashMirror(t)
	other := testDB(t)
	for _, sess := range []db.Session{
		{ID: codexTrashThread, Agent: "codex", Machine: "machine", Project: "sample"},
		{ID: codexTrashPage, Agent: "codex", Machine: "machine", Project: "selected"},
	} {
		require.NoError(t, other.UpsertSession(ctx, sess))
	}
	filtered, err := New(testPGURL(t), initial.schema, other, "machine", true, storage.PusherOptions{Projects: []string{"selected"}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filtered.Close()) })
	result, err := filtered.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	assert.Equal(t, 1, result.SkippedConflicts)
	head, err := store.GetSessionFull(ctx, codexTrashThread)
	require.NoError(t, err)
	require.NotNil(t, head)
	assert.True(t, head.TrashIncludesCodexPages, "another archive must not clear the owning archive's scope")
}
