package vector

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

type restoredSessionSource struct {
	*db.DB
	restore func()
}

func (s *restoredSessionSource) LoadSessionDeletionChanges(ctx context.Context, after, through int64) ([]string, error) {
	delta, err := s.DB.LoadSessionDeletionChanges(ctx, after, through)
	if err == nil && len(delta) > 0 && s.restore != nil {
		s.restore()
		s.restore = nil
	}
	return delta, err
}

// rewoundJournalSource reports a journal revision below the one the mirror
// stored, as after a journal reset or a restored backup.
type rewoundJournalSource struct {
	*db.DB
	revision int64
}

func (s *rewoundJournalSource) SessionDeletionPublicationRevision(context.Context) (int64, error) {
	return s.revision, nil
}

func seedEndedSession(t *testing.T, archive *db.DB, id, content, ended string) {
	t.Helper()
	dbtest.SeedSessionWithMessages(t, archive, id, "project",
		[]db.Message{dbtest.UserMsg(id, 0, content)},
		func(s *db.Session) { s.EndedAt = new(ended) })
}

func mirrorSessionIDs(t *testing.T, ix *Index) []string {
	t.Helper()
	rows, err := ix.db.QueryContext(t.Context(), `SELECT DISTINCT session_id FROM vector_messages ORDER BY session_id`)
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func TestBuildIncrementalDropsPermanentlyDeletedSession(t *testing.T) {
	ctx := t.Context()
	archive := dbtest.OpenTestDB(t)
	seedEndedSession(t, archive, "kept", "beta", "2024-01-02T00:00:00Z")
	seedEndedSession(t, archive, "gone", "alpha", "2024-01-01T00:00:00Z")
	ix := openTestIndex(t)
	enc := fakeBuildEncoder()
	gen := fakeGeneration("fake-model")

	_, err := ix.Build(ctx, archive, enc, gen, BuildOptions{})
	require.NoError(t, err)
	trashed, err := archive.SoftDeleteSessions(ctx, []string{"gone"})
	require.NoError(t, err)
	require.Equal(t, 1, trashed)
	deleted, err := archive.DeleteSessionIfTrashed(ctx, "gone")
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)

	result, err := ix.Build(ctx, archive, enc, gen, BuildOptions{})
	require.NoError(t, err)
	assert.Equal(t, 0, result.Fill.Documents, "the surviving session keeps its embedding")

	hits, err := ix.Search(ctx, enc, "alpha", 10)
	require.NoError(t, err)
	for _, hit := range hits {
		assert.NotEqual(t, "gone", hit.SessionID, "a permanently deleted session must not be returned")
	}
	infos, err := ix.Generations(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.EqualValues(t, 1, infos[0].Embedded)
}

func TestRefreshRunsFullReconciliationWhenJournalMovesBackwards(t *testing.T) {
	ctx := t.Context()
	archive := dbtest.OpenTestDB(t)
	seedEndedSession(t, archive, "kept", "kept content", "2024-01-03T00:00:00Z")
	seedEndedSession(t, archive, "earlier", "earlier content", "2024-01-02T00:00:00Z")
	seedEndedSession(t, archive, "gone", "gone content", "2024-01-01T00:00:00Z")
	ix := openTestIndex(t)
	_, err := ix.Refresh(ctx, archive, true, false)
	require.NoError(t, err)

	// Advance the stored cursor past zero through an ordinary deletion.
	removed, err := archive.DeleteParserExcludedSessions(ctx, []string{"earlier"})
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	_, err = ix.Refresh(ctx, archive, false, false)
	require.NoError(t, err)
	require.Equal(t, []string{"gone", "kept"}, mirrorSessionIDs(t, ix))

	removed, err = archive.DeleteParserExcludedSessions(ctx, []string{"gone"})
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	_, err = ix.Refresh(ctx, &rewoundJournalSource{DB: archive, revision: 0}, false, false)
	require.NoError(t, err)

	assert.Equal(t, []string{"kept"}, mirrorSessionIDs(t, ix))
}

func TestRefreshRunsFullReconciliationForNewArchiveIdentity(t *testing.T) {
	ctx := t.Context()
	original := dbtest.OpenTestDB(t)
	require.NoError(t, original.SetDatabaseIDForTest(ctx, "original-archive"))
	seedEndedSession(t, original, "kept", "kept content", "2024-01-03T00:00:00Z")
	seedEndedSession(t, original, "gone", "gone content", "2024-01-01T00:00:00Z")
	ix := openTestIndex(t)
	_, err := ix.Refresh(ctx, original, true, false)
	require.NoError(t, err)
	stored, err := original.SessionDeletionPublicationRevision(ctx)
	require.NoError(t, err)

	// A resync replaces the archive: its journal is not behind the stored
	// cursor and never records that "gone" was dropped.
	replacement := dbtest.OpenTestDB(t)
	require.NoError(t, replacement.SetDatabaseIDForTest(ctx, "replacement-archive"))
	seedEndedSession(t, replacement, "kept", "kept content", "2024-01-03T00:00:00Z")
	revision, err := replacement.SessionDeletionPublicationRevision(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, revision, stored)

	_, err = ix.Refresh(ctx, replacement, false, false)
	require.NoError(t, err)

	assert.Equal(t, []string{"kept"}, mirrorSessionIDs(t, ix))
}

func TestRefreshDeletionSnapshotCannotRemoveRestoredSession(t *testing.T) {
	archive := dbtest.OpenTestDB(t)
	seed := func(id, content, ended string) {
		dbtest.SeedSessionWithMessages(t, archive, id, "project", []db.Message{dbtest.UserMsg(id, 0, content)}, func(s *db.Session) { s.EndedAt = new(ended) })
	}
	seed("restored", "old content", "2024-01-01T00:00:00Z")
	seed("retained", "retained content", "2024-01-03T00:00:00Z")
	ix := openTestIndex(t)
	_, err := ix.Refresh(t.Context(), archive, true, false)
	require.NoError(t, err)
	removed, err := archive.DeleteParserExcludedSessions(t.Context(), []string{"restored"})
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	source := &restoredSessionSource{DB: archive, restore: func() { seed("restored", "restored content", "2024-01-01T00:00:00Z") }}
	_, err = ix.Refresh(t.Context(), source, false, false)
	require.NoError(t, err)
	assert.Nil(t, source.restore, "the restore must occur after capturing the real journal tombstone")
	var content string
	err = ix.db.QueryRowContext(t.Context(), `SELECT content FROM vector_messages WHERE session_id = 'restored'`).Scan(&content)
	require.NoError(t, err)
	assert.Equal(t, "restored content", content)
	_, err = ix.Refresh(t.Context(), source, false, false)
	require.NoError(t, err)
	var count int
	require.NoError(t, ix.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM vector_messages`).Scan(&count))
	assert.Equal(t, 2, count, "retain both restored and unchanged archive content")
}

func TestRefreshReconcilesStaleDocumentsWhenRestoreRacesIncrementalScan(t *testing.T) {
	archive := dbtest.OpenTestDB(t)
	seed := func(id string, messages []db.Message, ended string) {
		dbtest.SeedSessionWithMessages(t, archive, id, "project", messages, func(s *db.Session) { s.EndedAt = new(ended) })
	}
	seed("restored", []db.Message{
		dbtest.UserMsg("restored", 0, "original content"),
		dbtest.UserMsg("restored", 1, "obsolete content"),
	}, "2024-01-01T00:00:00Z")
	seed("retained", []db.Message{dbtest.UserMsg("retained", 0, "retained content")}, "2024-01-03T00:00:00Z")

	ix := openTestIndex(t)
	_, err := ix.Refresh(t.Context(), archive, true, false)
	require.NoError(t, err)
	removed, err := archive.DeleteParserExcludedSessions(t.Context(), []string{"restored"})
	require.NoError(t, err)
	require.Equal(t, 1, removed)

	// Restore after Refresh captures the deletion revision, with a new
	// timestamp that makes the restored row appear in the incremental scan.
	source := &restoredSessionSource{DB: archive, restore: func() {
		seed("restored", []db.Message{dbtest.UserMsg("restored", 0, "restored content")}, "2024-01-04T00:00:00Z")
	}}
	_, err = ix.Refresh(t.Context(), source, false, false)
	require.NoError(t, err)
	assert.Nil(t, source.restore, "the restore must occur after capturing the deletion revision")

	var count int
	err = ix.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM vector_messages WHERE session_id = ?`, "restored").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "obsolete mirror documents from the restored session must be removed")
	var content string
	err = ix.db.QueryRowContext(t.Context(), `SELECT content FROM vector_messages WHERE session_id = ?`, "restored").Scan(&content)
	require.NoError(t, err)
	assert.Equal(t, "restored content", content)
}

func TestRefreshRescansRestoredSessionBehindWatermark(t *testing.T) {
	archive := dbtest.OpenTestDB(t)
	seed := func(id, content, ended string) {
		dbtest.SeedSessionWithMessages(t, archive, id, "project", []db.Message{dbtest.UserMsg(id, 0, content)}, func(s *db.Session) { s.EndedAt = new(ended) })
	}
	seed("restored", "original content", "2024-01-01T00:00:00Z")
	seed("retained", "retained content", "2024-01-03T00:00:00Z")

	ix := openTestIndex(t)
	_, err := ix.Refresh(t.Context(), archive, true, false)
	require.NoError(t, err)
	watermark, err := ix.refreshWatermark(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "2024-01-03T00:00:00Z", watermark)

	removed, err := archive.DeleteParserExcludedSessions(t.Context(), []string{"restored"})
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	_, err = ix.Refresh(t.Context(), archive, false, false)
	require.NoError(t, err)

	// Re-insertion publishes a deleted=0 journal entry, but its old ended_at
	// remains behind the watermark established by the retained session.
	seed("restored", "restored content", "2024-01-01T00:00:00Z")
	_, err = ix.Refresh(t.Context(), archive, false, false)
	require.NoError(t, err)

	var content string
	err = ix.db.QueryRowContext(t.Context(), `SELECT content FROM vector_messages WHERE session_id = 'restored'`).Scan(&content)
	require.NoError(t, err, "the restored session must be republished to the vector mirror")
	assert.Equal(t, "restored content", content)
}

func TestRefreshReconcilesRestoredSessionBeforeDeletionIsConsumed(t *testing.T) {
	archive := dbtest.OpenTestDB(t)
	seed := func(id string, messages []db.Message, ended string) {
		dbtest.SeedSessionWithMessages(t, archive, id, "project", messages, func(s *db.Session) { s.EndedAt = new(ended) })
	}
	seed("restored", []db.Message{
		dbtest.UserMsg("restored", 0, "original content"),
		dbtest.UserMsg("restored", 1, "obsolete content"),
	}, "2024-01-01T00:00:00Z")
	seed("retained", []db.Message{dbtest.UserMsg("retained", 0, "retained content")}, "2024-01-03T00:00:00Z")

	ix := openTestIndex(t)
	_, err := ix.Refresh(t.Context(), archive, true, false)
	require.NoError(t, err)
	removed, err := archive.DeleteParserExcludedSessions(t.Context(), []string{"restored"})
	require.NoError(t, err)
	require.Equal(t, 1, removed)

	// Re-insertion overwrites the journal row with deleted=0 before the
	// incremental refresh consumes the earlier deletion revision.
	seed("restored", []db.Message{dbtest.UserMsg("restored", 0, "restored content")}, "2024-01-01T00:00:00Z")
	_, err = ix.Refresh(t.Context(), archive, false, false)
	require.NoError(t, err)

	var count int
	err = ix.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM vector_messages WHERE session_id = ?`, "restored").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "the restored session must replace stale mirror documents")
	var content string
	err = ix.db.QueryRowContext(t.Context(), `SELECT content FROM vector_messages WHERE session_id = ?`, "restored").Scan(&content)
	require.NoError(t, err)
	assert.Equal(t, "restored content", content)
}
