//go:build fts5

package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionDeletionJournalRecordsHardDeletes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"del-1", "del-2", "keep-1"} {
		require.NoError(database.UpsertSession(Session{
			ID: id, Project: "p", Machine: "m", Agent: "a",
			CreatedAt: "2026-07-01T10:00:00.000Z",
		}))
	}
	before, err := database.SessionDeletionPublicationRevision(ctx)
	require.NoError(err)

	require.NoError(database.DeleteSession("del-1"))
	_, err = database.DeleteSessions([]string{"del-2"})
	require.NoError(err)

	after, err := database.SessionDeletionPublicationRevision(ctx)
	require.NoError(err)
	require.Greater(after, before)

	delta, err := database.LoadSessionDeletionDelta(ctx, before, after, nil, nil)
	require.NoError(err)
	ids := make([]string, 0, len(delta))
	for _, d := range delta {
		assert.Equal("p", d.Project)
		ids = append(ids, d.SessionID)
	}
	assert.ElementsMatch([]string{"del-1", "del-2"}, ids)

	// Window semantics: an empty half-open window yields no tombstones.
	empty, err := database.LoadSessionDeletionDelta(ctx, after, after, nil, nil)
	require.NoError(err)
	assert.Empty(empty)

	// Project filters exclude tombstones outside their scope.
	filtered, err := database.LoadSessionDeletionDelta(ctx, before, after, []string{"other"}, nil)
	require.NoError(err)
	assert.Empty(filtered)
}

func TestSessionDeletionJournalIgnoresSoftDeleteAndClearsOnReinsert(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	require.NoError(database.UpsertSession(Session{
		ID: "sd-1", Project: "p", Machine: "m", Agent: "a",
		CreatedAt: "2026-07-01T10:00:00.000Z",
	}))
	before, err := database.SessionDeletionPublicationRevision(ctx)
	require.NoError(err)

	// Soft delete must not create a tombstone (soft-deleted sessions stay
	// live in the mirror; see deleteHardDeletedMirrorSessions semantics).
	require.NoError(database.SoftDeleteSession("sd-1"))
	after, err := database.SessionDeletionPublicationRevision(ctx)
	require.NoError(err)
	delta, err := database.LoadSessionDeletionDelta(ctx, before, after, nil, nil)
	require.NoError(err)
	assert.Empty(delta)

	// Hard delete then re-insert: the re-insert flips the journal row back
	// to deleted=0, so a delta spanning both events yields no tombstone.
	n, err := database.DeleteSessionIfTrashed("sd-1")
	require.NoError(err)
	require.Equal(int64(1), n)
	// DeleteSessionIfTrashed permanently excludes the id, which is a
	// business-level gate in UpsertSession unrelated to the deletion
	// journal under test here. Clear it directly so the re-insert below
	// exercises the journal's insert trigger.
	_, err = database.getWriter().Exec(
		"DELETE FROM excluded_sessions WHERE id = ?", "sd-1")
	require.NoError(err)
	require.NoError(database.UpsertSession(Session{
		ID: "sd-1", Project: "p", Machine: "m", Agent: "a",
		CreatedAt: "2026-07-01T11:00:00.000Z",
	}))
	final, err := database.SessionDeletionPublicationRevision(ctx)
	require.NoError(err)
	delta, err = database.LoadSessionDeletionDelta(ctx, before, final, nil, nil)
	require.NoError(err)
	assert.Empty(delta)
}
