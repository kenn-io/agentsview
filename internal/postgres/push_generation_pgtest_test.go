//go:build pgtest

package postgres

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// rebuildArchive stands in for a /resync: the replacement archive gets a new
// database ID and starts without this archive's PostgreSQL push state.
func (f resumeFixture) rebuildArchive(t *testing.T, id string) {
	t.Helper()
	require.NoError(t, f.local.SetDatabaseIDForTest(t.Context(), id))
	require.NoError(t, clearPushState(t.Context(), f.local))
}

const lostStateReason = "the local archive lost its record of earlier pushes, as happens after a rebuild"

// A rebuild keeps this archive's push marker but drops its watermark, so
// PostgreSQL can hold rows the rebuild dropped. The next push must be full.
func TestPushAfterArchiveRebuildIsFull(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_generation_test")
	_, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	res, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	require.False(t, res.Full)

	f.rebuildArchive(t, "rebuilt")
	res, err = f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.True(t, res.Full)
	assert.Equal(t, lostStateReason, res.FullReason)
}

func TestPushAfterArchiveRebuildRemovesMissingSessions(t *testing.T) {
	for _, tc := range []struct {
		name            string
		projects        []string
		excludeProjects []string
		empty           bool
		interrupt       bool
		want            []string
	}{
		{name: "unfiltered", want: []string{"foreign", "legacy", "new"}},
		{name: "empty archive", empty: true, want: []string{"foreign", "legacy"}},
		{name: "resumed push", interrupt: true, want: []string{"foreign", "legacy", "new"}},
		{name: "included projects", projects: []string{"proj"}, want: []string{"foreign", "legacy", "new", "outside"}},
		{name: "excluded projects", excludeProjects: []string{"outside"}, want: []string{"foreign", "legacy", "new", "outside"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResumeFixture(t, "agentsview_push_generation_test")
			for _, id := range []string{"foreign", "legacy", "outside"} {
				project := "proj"
				if id == "outside" {
					project = "outside"
				}
				require.NoError(t, f.local.UpsertSession(t.Context(), db.Session{
					ID: id, Project: project, Machine: "machine", Agent: "claude",
				}))
			}
			res, err := f.sync().Push(t.Context(), false, nil)
			require.NoError(t, err)
			require.Zero(t, res.Errors)
			// Establish this filter's push history before the rebuild drops it.
			s := f.sync()
			s.projects, s.excludeProjects = tc.projects, tc.excludeProjects
			s.syncState = newScopedSyncStateStore(handoffSyncState{f.local},
				pushSyncStateScope("", tc.projects, tc.excludeProjects), false)
			_, err = s.Push(t.Context(), false, nil)
			require.NoError(t, err)
			_, err = f.pg.ExecContext(t.Context(), `UPDATE sessions SET owner_marker = 'another-archive' WHERE id = 'foreign'`)
			require.NoError(t, err)
			_, err = f.pg.ExecContext(t.Context(), `UPDATE sessions SET owner_marker = '' WHERE id = 'legacy'`)
			require.NoError(t, err)

			rebuilt, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "rebuilt.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rebuilt.Close()) })
			require.NoError(t, rebuilt.CopyArchiveIdentityFrom(t.Context(), f.local.Path()))
			require.NoError(t, rebuilt.CopySyncStateFrom(t.Context(), f.local.Path()))
			require.NoError(t, f.local.CloseConnections(t.Context()))
			omitted := []string{"old", "mid", "foreign", "legacy", "outside"}
			if tc.empty {
				omitted = append(omitted, "new")
			}
			// Parser exclusions during resync omit rows without adding user deletions.
			_, err = rebuilt.CopyOrphanedDataFromExcluding(t.Context(), f.local.Path(), omitted)
			require.NoError(t, err)
			f.local = rebuilt
			s = f.sync()
			s.projects, s.excludeProjects = tc.projects, tc.excludeProjects
			s.syncState = newScopedSyncStateStore(handoffSyncState{f.local},
				pushSyncStateScope("", tc.projects, tc.excludeProjects), false)
			if tc.interrupt {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				s.afterPushBatch = cancel
				_, err = s.Push(ctx, false, nil)
				require.ErrorIs(t, err, context.Canceled)
				assert.Contains(t, f.sessionIDs(t), "old", "interrupted publication must leave cleanup for the retry")
				s.afterPushBatch = nil
			}
			res, err = s.Push(t.Context(), false, nil)
			require.NoError(t, err)
			assert.True(t, res.Full)
			assert.Zero(t, res.Errors)
			assert.Equal(t, tc.want, f.sessionIDs(t))
			var messages int
			require.NoError(t, f.pg.QueryRowContext(t.Context(),
				`SELECT count(*) FROM messages WHERE session_id IN ('old', 'mid')`).Scan(&messages))
			assert.Zero(t, messages, "removed sessions must not leave transcripts behind")
		})
	}
}

// Push state is kept per project filter, and the push marker covers the whole
// archive. The first push with a new filter has saved no state of its own, so
// it must not claim it lost any.
func TestFirstFilteredPushDoesNotClaimLostState(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_generation_test")
	_, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)

	filtered := f.sync()
	filtered.projects = []string{"proj"}
	filtered.syncState = newScopedSyncStateStore(handoffSyncState{f.local},
		pushSyncStateScope("", filtered.projects, nil), false)
	res, err := filtered.Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.NotEqual(t, lostStateReason, res.FullReason)
}

// A rebuild during a push mixes rows from two archives. The push must fail
// without saving its cutoff into the replacement, and the next push must be
// full.
func TestPushFailsWhenTheArchiveIsRebuiltDuringIt(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_generation_test")
	_, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	require.NoError(t, f.local.InsertMessages(t.Context(), []db.Message{{
		SessionID: "new", Ordinal: 1, Role: "user", Content: "again",
		ContentLength: 5, Timestamp: "2026-03-02T00:00:00Z",
	}}))

	s := f.sync()
	s.afterPushBatch = func() { f.rebuildArchive(t, "rebuilt") }
	_, err = s.Push(t.Context(), false, nil)
	require.ErrorContains(t, err, "rebuilt while this push ran")
	saved, err := f.local.GetSyncState(t.Context(), "last_push_at")
	require.NoError(t, err)
	assert.Empty(t, saved, "the push must not save its cutoff into the replacement")

	res, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.True(t, res.Full)
	assert.Equal(t, lostStateReason, res.FullReason)
}

// A --full push that fails before it records its full push reports that it
// never started, so a watch keeps the --full request.
func TestFullPushFailingBeforeItsRunIsNotStarted(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_generation_test")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, full := range []bool{false, true} {
		_, err := f.sync().PushWithOptions(ctx, storage.PushOptions{Full: full}, nil)
		require.ErrorIs(t, err, context.Canceled)
		_, notStarted := errors.AsType[storage.PushNotStartedError](err)
		assert.Equal(t, full, notStarted, "full=%t", full)
	}
}

// A failed full push saves its progress without the push's cancellation, so
// the save must still end when the archive writer never reopens, as during a
// daemon shutdown.
func TestFailedFullPushProgressSaveEnds(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_generation_test")
	s := f.sync()
	s.fullRunSaveTimeout = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.afterPushBatch = func() {
		require.NoError(t, f.local.CloseWriter())
		cancel()
	}
	_, err := s.PushWithOptions(ctx, storage.PushOptions{Full: true}, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, context.DeadlineExceeded,
		"the progress save gives up instead of waiting forever")
	require.NoError(t, f.local.ReopenWriter())
}
