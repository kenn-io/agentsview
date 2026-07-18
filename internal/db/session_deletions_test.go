//go:build fts5

package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionDeletionJournalRecordsHardDeletes(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	for _, id := range []string{"del-1", "del-2", "keep-1"} {
		require.NoError(t, database.UpsertSession(Session{
			ID: id, Project: "p", Machine: "m", Agent: "a",
			CreatedAt: "2026-07-01T10:00:00.000Z"}))
	}
	before, err := database.SessionDeletionPublicationRevision(ctx)
	require.NoError(t, err)

	require.NoError(t, database.DeleteSession("del-1"))
	_, err = database.DeleteSessions([]string{"del-2"})
	require.NoError(t, err)

	after, err := database.SessionDeletionPublicationRevision(ctx)
	require.NoError(t, err)
	require.Greater(t, after, before)

	delta, err := database.LoadSessionDeletionDelta(ctx, before, after, nil, nil)
	require.NoError(t, err)
	ids := make([]string, 0, len(delta))
	for _, d := range delta {
		assert.Equal(t, "p", d.Project)
		ids = append(ids, d.SessionID)
	}
	assert.ElementsMatch(t, []string{"del-1", "del-2"}, ids)

	// Window semantics: an empty half-open window yields no tombstones.
	empty, err := database.LoadSessionDeletionDelta(ctx, after, after, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)

	// Project filters exclude tombstones outside their scope.
	filtered, err := database.LoadSessionDeletionDelta(ctx, before, after, []string{"other"}, nil)
	require.NoError(t, err)
	assert.Empty(t, filtered)
}

func TestSessionDeletionJournalIgnoresSoftDeleteAndClearsOnReinsert(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	require.NoError(t, database.UpsertSession(Session{
		ID: "sd-1", Project: "p", Machine: "m", Agent: "a",
		CreatedAt: "2026-07-01T10:00:00.000Z"}))
	before, err := database.SessionDeletionPublicationRevision(ctx)
	require.NoError(t, err)

	// Soft delete must not create a tombstone (soft-deleted sessions stay
	// live in the mirror; see deleteHardDeletedMirrorSessions semantics).
	require.NoError(t, database.SoftDeleteSession("sd-1"))
	after, err := database.SessionDeletionPublicationRevision(ctx)
	require.NoError(t, err)
	delta, err := database.LoadSessionDeletionDelta(ctx, before, after, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, delta)

	// Hard delete then re-insert: the re-insert flips the journal row back
	// to deleted=0, so a delta spanning both events yields no tombstone.
	n, err := database.DeleteSessionIfTrashed("sd-1")
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	// DeleteSessionIfTrashed permanently excludes the id, which is a
	// business-level gate in UpsertSession unrelated to the deletion
	// journal under test here. Clear it directly so the re-insert below
	// exercises the journal's insert trigger.
	_, err = database.getWriter().Exec(
		"DELETE FROM excluded_sessions WHERE id = ?", "sd-1")
	require.NoError(t, err)
	require.NoError(t, database.UpsertSession(Session{
		ID: "sd-1", Project: "p", Machine: "m", Agent: "a",
		CreatedAt: "2026-07-01T11:00:00.000Z"}))
	final, err := database.SessionDeletionPublicationRevision(ctx)
	require.NoError(t, err)
	delta, err = database.LoadSessionDeletionDelta(ctx, before, final, nil, nil)
	require.NoError(t, err)
	assert.Empty(t, delta)
}
