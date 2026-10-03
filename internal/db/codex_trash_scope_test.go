package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	legacyTrashThread      = "codex:11111111-1111-4111-8111-111111111111"
	legacyTrashPage        = legacyTrashThread + "_22222222-2222-4222-8222-222222222222"
	legacyTrashSibling     = legacyTrashThread + "_33333333-3333-4333-8333-333333333333"
	legacyTrashMissingPage = legacyTrashThread + "_44444444-4444-4444-8444-444444444444"
)

// Reproduce the rebuild order: preserve old trash, parse available pages, then
// apply the user's metadata. The absent page has never had its own archive row.
func upgradedCodexTrash(t *testing.T) *DB {
	t.Helper()
	ctx := t.Context()
	source := testDB(t)
	insertSession(t, source, legacyTrashThread, "sample", func(s *Session) { s.Agent = "codex" })
	require.NoError(t, source.SoftDeleteSession(ctx, legacyTrashThread))
	_, err := source.getWriter().Exec(ctx, "PRAGMA user_version = 122")
	require.NoError(t, err)
	path := source.Path()
	require.NoError(t, source.Close())

	d := testDB(t)
	require.NoError(t, d.CopyArchiveIdentityFrom(path))
	_, err = d.CopyTrashedDataFrom(path)
	require.NoError(t, err)
	for _, id := range []string{legacyTrashPage, legacyTrashSibling} {
		insertSession(t, d, id, "sample", func(s *Session) { s.Agent = "codex" })
		insertMessages(t, d, Message{SessionID: id, Role: "user", Content: "Saved page"})
	}
	require.NoError(t, d.CopySessionMetadataFrom(path))
	return d
}

func TestLegacyCodexTrashBlocksAbsentPageWrites(t *testing.T) {
	d := upgradedCodexTrash(t)
	ctx := t.Context()
	page := Session{ID: legacyTrashMissingPage, Agent: "codex", Project: "sample", Machine: "local"}
	assert.True(t, d.IsSessionTrashed(ctx, page.ID))
	require.ErrorIs(t, d.UpsertSession(ctx, page), ErrSessionTrashed)
	require.ErrorIs(t, d.insertSessionIfAbsent(ctx, page), ErrSessionTrashed)
	conn, err := d.getWriter().Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.ErrorIs(t, validateRecallImportPlaceholderSessionStateWithQueryer(ctx, conn, page.ID), ErrSessionTrashed)
	require.NoError(t, conn.Close())

	page.ID = "host_b~" + page.ID
	require.NoError(t, d.UpsertSession(ctx, page), "another source's same thread is independent")
}

func TestRestoreLegacyCodexTrashEndsThreadScope(t *testing.T) {
	for _, restoreID := range []string{legacyTrashThread, legacyTrashPage} {
		t.Run(restoreID, func(t *testing.T) {
			d := upgradedCodexTrash(t)
			ctx := t.Context()
			n, err := d.RestoreSession(ctx, restoreID)
			require.NoError(t, err)
			require.EqualValues(t, 1, n)
			assert.True(t, d.IsSessionTrashed(ctx, legacyTrashSibling), "other saved trash stays hidden")
			assert.False(t, d.IsSessionTrashed(ctx, legacyTrashMissingPage), "restore ends inherited scope")
			require.NoError(t, d.UpsertSession(ctx, Session{ID: restoreID, Agent: "codex", Project: "sample", Machine: "local"}))
			_, err = d.EmptyTrash(ctx)
			require.NoError(t, err)
			assert.False(t, d.IsSessionExcluded(ctx, restoreID), "emptying remaining trash must preserve the restored file")
			assert.False(t, d.IsSessionExcluded(ctx, legacyTrashMissingPage))

			path := d.Path()
			require.NoError(t, d.Close())
			rebuilt := testDB(t)
			require.NoError(t, rebuilt.CopyArchiveIdentityFrom(path))
			require.NoError(t, rebuilt.CopyExcludedSessionsFrom(path))
			_, err = rebuilt.CopyTrashedDataFrom(path)
			require.NoError(t, err)
			_, err = rebuilt.CopyOrphanedDataFrom(path)
			require.NoError(t, err)
			require.NoError(t, rebuilt.CopySessionMetadataFrom(path))
			restored, err := rebuilt.GetSession(ctx, restoreID)
			require.NoError(t, err)
			require.NotNil(t, restored)
			require.NoError(t, rebuilt.UpsertSession(ctx, Session{ID: legacyTrashMissingPage, Agent: "codex", Project: "sample", Machine: "local"}))
		})
	}
}

func TestPurgeLegacyCodexTrashRetainsThreadExclusion(t *testing.T) {
	for _, method := range []string{"delete", "delete_trashed", "delete_many", "empty_trash"} {
		t.Run(method, func(t *testing.T) {
			d := upgradedCodexTrash(t)
			ctx := t.Context()
			insertSession(t, d, "unrelated", "sample")
			switch method {
			case "delete":
				require.NoError(t, d.DeleteSession(ctx, legacyTrashThread))
			case "delete_trashed":
				n, err := d.DeleteSessionIfTrashed(ctx, legacyTrashThread)
				require.NoError(t, err)
				require.EqualValues(t, 3, n)
			case "delete_many":
				n, err := d.DeleteSessions(ctx, []string{legacyTrashThread, "absent"})
				require.NoError(t, err)
				require.Equal(t, 3, n)
			case "empty_trash":
				n, err := d.EmptyTrash(ctx)
				require.NoError(t, err)
				require.Equal(t, 3, n)
			}
			for _, id := range []string{legacyTrashThread, legacyTrashPage, legacyTrashSibling} {
				session, err := d.GetSessionFull(ctx, id)
				require.NoError(t, err)
				assert.Nil(t, session, "covered trash cannot remain restorable")
			}
			unrelated, err := d.GetSession(ctx, "unrelated")
			require.NoError(t, err)
			require.NotNil(t, unrelated)
			assert.True(t, d.IsSessionExcluded(ctx, legacyTrashMissingPage))
			assert.False(t, d.IsSessionExcluded(ctx, "absent"))

			path := d.Path()
			require.NoError(t, d.Close())
			rebuilt := testDB(t)
			require.NoError(t, rebuilt.CopyExcludedSessionsFrom(path))
			require.ErrorIs(t, rebuilt.UpsertSession(ctx, Session{ID: legacyTrashMissingPage, Agent: "codex", Project: "sample", Machine: "local"}), ErrSessionExcluded)
		})
	}
}

func TestPurgeLegacyCodexTrashPageKeepsSiblingScope(t *testing.T) {
	d := upgradedCodexTrash(t)
	ctx := t.Context()
	n, err := d.DeleteSessionIfTrashed(ctx, legacyTrashPage)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	assert.True(t, d.IsSessionExcluded(ctx, legacyTrashPage))
	assert.False(t, d.IsSessionExcluded(ctx, legacyTrashMissingPage))
	assert.True(t, d.IsSessionTrashed(ctx, legacyTrashMissingPage))
	_, err = d.RestoreSession(ctx, legacyTrashThread)
	require.NoError(t, err)
	require.NoError(t, d.UpsertSession(ctx, Session{ID: legacyTrashMissingPage, Agent: "codex", Project: "sample", Machine: "local"}))
}
