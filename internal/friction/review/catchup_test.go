package review_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
)

func reportDates(reps []review.Report) []string {
	out := []string{}
	for _, r := range reps {
		out = append(out, r.Date)
	}
	return out
}

func TestCatchUp(t *testing.T) {
	tests := []struct {
		name     string
		seed     func(t *testing.T, d *db.DB, r *review.Runner)
		backfill int
		want     []string
	}{
		{
			name: "builds_exactly_the_missing_complete_dates_within_backfill",
			seed: func(t *testing.T, d *db.DB, _ *review.Runner) {
				t.Helper()
				seed(t, d, subject{id: "old", ended: "2026-09-01T10:00:00Z"},
					subject{id: "today", ended: "2026-09-22T09:00:00Z"})
			},
			backfill: 7,
			want:     []string{"2026-09-15", "2026-09-16", "2026-09-17", "2026-09-18", "2026-09-19", "2026-09-20", "2026-09-21"},
		},
		{
			name: "fills_gaps_before_and_after_latest_digest",
			seed: func(t *testing.T, d *db.DB, r *review.Runner) {
				t.Helper()
				seed(t, d, subject{id: "old", ended: "2026-09-01T10:00:00Z"})
				_, err := r.BuildDate(t.Context(), "2026-09-18", review.BuildOptions{})
				require.NoError(t, err)
			},
			backfill: 7,
			want:     []string{"2026-09-15", "2026-09-16", "2026-09-17", "2026-09-19", "2026-09-20", "2026-09-21"},
		},
		{
			name: "completes_window_even_when_archive_started_recently",
			seed: func(t *testing.T, d *db.DB, _ *review.Runner) {
				t.Helper()
				seed(t, d, subject{id: "new", ended: "2026-09-20T10:00:00Z"})
			},
			backfill: 7,
			want:     []string{"2026-09-15", "2026-09-16", "2026-09-17", "2026-09-18", "2026-09-19", "2026-09-20", "2026-09-21"},
		},
		{
			name:     "empty_archive_builds_nothing",
			seed:     func(*testing.T, *db.DB, *review.Runner) {},
			backfill: 7,
			want:     []string{},
		},
		{
			name: "never_today",
			seed: func(t *testing.T, d *db.DB, _ *review.Runner) {
				t.Helper()
				seed(t, d, subject{id: "today", ended: "2026-09-22T09:00:00Z"})
			},
			backfill: 7,
			want:     []string{},
		},
		{
			name: "backfill_of_one_builds_yesterday_only",
			seed: func(t *testing.T, d *db.DB, _ *review.Runner) {
				t.Helper()
				seed(t, d, subject{id: "old", ended: "2026-09-01T10:00:00Z"})
			},
			backfill: 1,
			want:     []string{"2026-09-21"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			r := newRunner(d)
			r.BackfillDays = tt.backfill
			tt.seed(t, d, r)
			reps, err := r.CatchUp(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.want, reportDates(reps))
			for _, rep := range reps {
				assert.True(t, rep.Written, rep.Date)
			}
			again, err := r.CatchUp(t.Context())
			require.NoError(t, err)
			assert.Empty(t, again, "a second catch-up is a no-op")
		})
	}
}

func TestCatchUpZoneDecidesToday(t *testing.T) {
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)
	d := dbtest.OpenTestDB(t)
	seed(t, d, subject{id: "s", ended: "2026-09-21T10:00:00Z"})
	r := newRunner(d)
	r.Loc = tokyo
	r.BackfillDays = 1
	r.Now = func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) } // 01:00 on the 22nd in Tokyo
	reps, err := r.CatchUp(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"2026-09-21"}, reportDates(reps))
	assert.Equal(t, "Asia/Tokyo", reps[0].Snapshot.Timezone)
}

func TestStaleSubjects(t *testing.T) {
	t.Run("stale_subjects_keep_waiting", func(t *testing.T) {
		d := dbtest.OpenTestDB(t)
		seed(t, d,
			subject{id: "ready", ended: "2026-09-21T10:00:00Z", findings: []db.FrictionFinding{correction("ready", "no, use the other file please")}},
			subject{id: "pending", ended: "2026-09-21T11:00:00Z", rules: "friction-v0"})
		r := newRunner(d) // now = 2026-09-22T10:00Z, 10h after the 21st ended
		r.BackfillDays = 1
		reps, err := r.CatchUp(t.Context())
		require.NoError(t, err, "a deferred date is not a job failure")
		assert.Empty(t, reps)
		got, err := d.GetFrictionDigest(t.Context(), "2026-09-21")
		require.NoError(t, err)
		assert.Nil(t, got)

		r.Now = func() time.Time { return time.Date(2026, 9, 24, 1, 0, 0, 0, time.UTC) }
		r.BackfillDays = 3
		reps, err = r.CatchUp(t.Context())
		require.NoError(t, err)
		assert.NotContains(t, reportDates(reps), "2026-09-21")
		assert.Nil(t, stored(t, d, "2026-09-21"), "pending review never expires")
	})
	t.Run("dry_run_ignores_staleness", func(t *testing.T) {
		d := dbtest.OpenTestDB(t)
		seed(t, d, subject{id: "pending", ended: "2026-09-21T11:00:00Z", rules: "friction-v0"})
		_, err := newRunner(d).BuildDate(t.Context(), "2026-09-21", review.BuildOptions{DryRun: true})
		require.NoError(t, err)
	})
}

func TestRerender(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seed(t, d, subject{id: "s1", ended: "2026-09-15T12:00:00Z", findings: []db.FrictionFinding{correction("s1", "no, use the other file please")}})
	r := newRunner(d)
	_, err := r.BuildDate(t.Context(), "2026-09-15", review.BuildOptions{})
	require.NoError(t, err)
	before := stored(t, d, "2026-09-15")

	require.NoError(t, r.Rerender(t.Context(), "2026-09-15"))
	assert.Equal(t, before.Revision, stored(t, d, "2026-09-15").Revision, "identical bytes do not bump revision")

	// Simulate bytes produced by an earlier renderer, keeping the frozen input.
	require.NoError(t, d.UpdateFrictionDigestRender(t.Context(), "2026-09-15", []byte("old renderer"), before.SummaryJSON, before.Revision+1))
	require.NoError(t, r.Rerender(t.Context(), "2026-09-15"))
	after := stored(t, d, "2026-09-15")
	assert.Equal(t, before.Revision+2, after.Revision)
	assert.Equal(t, before.Markdown, after.Markdown, "markdown comes from the frozen snapshot")
	assert.Equal(t, before.SummaryJSON, after.SummaryJSON)
	assert.Equal(t, before.SnapshotJSON, after.SnapshotJSON)

	require.ErrorIs(t, r.Rerender(t.Context(), "2020-01-01"), review.ErrDigestNotFound)
}

func TestCatchUpWaitsForReviewBackfill(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seed(t, d, subject{id: "pending", ended: "2026-09-16T12:00:00Z", rules: "friction-old"})
	r := newRunner(d)
	_, err := r.CatchUp(t.Context())
	require.NoError(t, err)
	assert.Nil(t, stored(t, d, "2026-09-16"), "old dates must wait for review too")

	seed(t, d, subject{id: "pending", ended: "2026-09-16T12:00:00Z", findings: []db.FrictionFinding{correction("pending", "use the existing helper")}})
	reps, err := r.CatchUp(t.Context())
	require.NoError(t, err)
	require.Len(t, reps, 1)
	assert.Equal(t, "2026-09-16", reps[0].Date)
	assert.Contains(t, string(stored(t, d, "2026-09-16").Markdown), "use the existing helper")
}

func TestCatchUpIncludesLateSessions(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seed(t, d, subject{id: "first", ended: "2026-09-16T12:00:00Z", findings: []db.FrictionFinding{correction("first", "use the existing helper")}})
	r := newRunner(d)
	_, err := r.CatchUp(t.Context())
	require.NoError(t, err)
	seed(t, d, subject{id: "late", ended: "2026-09-16T13:00:00Z", findings: []db.FrictionFinding{correction("late", "keep the public API")}})
	reps, err := r.CatchUp(t.Context())
	require.NoError(t, err)
	require.Len(t, reps, 1)
	assert.Equal(t, "2026-09-16", reps[0].Date)
	assert.Equal(t, 2, reps[0].Snapshot.SessionsScanned)
	got := stored(t, d, "2026-09-16")
	require.NotNil(t, got)
	assert.Contains(t, string(got.Markdown), "use the existing helper")
	assert.Contains(t, string(got.Markdown), "keep the public API")
	assert.Equal(t, 2, got.Revision)
	again, err := r.CatchUp(t.Context())
	require.NoError(t, err)
	assert.Empty(t, again)
	assert.Equal(t, 2, stored(t, d, "2026-09-16").Revision)
}

type countingReviewStore struct {
	review.Store
	subjectReads    int
	earliestReads   int
	pendingReads    int
	digestDateReads int
}

func (s *countingReviewStore) FrictionDigestDates(ctx context.Context, first, last string) ([]string, error) {
	s.digestDateReads++
	return s.Store.FrictionDigestDates(ctx, first, last)
}

func (s *countingReviewStore) EarliestSessionDate(ctx context.Context, loc *time.Location) (string, error) {
	s.earliestReads++
	return s.Store.EarliestSessionDate(ctx, loc)
}

func (s *countingReviewStore) FrictionPendingDates(ctx context.Context, first, last string, loc *time.Location) ([]string, error) {
	s.pendingReads++
	return s.Store.FrictionPendingDates(ctx, first, last, loc)
}

func (s *countingReviewStore) FrictionSubjectsForDate(ctx context.Context, date string, loc *time.Location, includeDigested bool) ([]db.FrictionSubject, error) {
	s.subjectReads++
	return s.Store.FrictionSubjectsForDate(ctx, date, loc, includeDigested)
}

func TestCatchUpOnlyLoadsChangedDates(t *testing.T) {
	for _, tc := range []struct{ days, history int }{{1, 1}, {30, 1024}} {
		t.Run(fmt.Sprintf("%d_days_%d_old_sessions", tc.days, tc.history), func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			r := newRunner(d)
			r.BackfillDays = tc.days
			for i := range tc.history {
				seed(t, d, subject{id: fmt.Sprintf("history-%d", i), ended: "2000-01-01T12:00:00Z"})
			}
			for i := 1; i <= tc.days; i++ {
				seed(t, d, subject{id: fmt.Sprintf("day-%d", i), ended: reviewNow.AddDate(0, 0, -i).Format(time.RFC3339)})
			}
			_, err := r.CatchUp(t.Context())
			require.NoError(t, err)

			// Membership survives reopening the persistent archive. Deleted and
			// trashed late arrivals must not make an unchanged day load again.
			seed(t, d, subject{id: "deleted", ended: "2026-09-21T11:00:00Z"}, subject{id: "trashed", ended: "2026-09-21T12:00:00Z"})
			require.NoError(t, d.DeleteSession(t.Context(), "deleted"))
			require.NoError(t, d.SoftDeleteSession(t.Context(), "trashed"))
			path := d.Path()
			require.NoError(t, d.Close())
			d = dbtest.OpenTestDBAt(t, path)
			counted := &countingReviewStore{Store: d}
			r.Store = counted
			again, err := r.CatchUp(t.Context())
			require.NoError(t, err)
			assert.Empty(t, again)
			assert.Zero(t, counted.subjectReads, "unchanged days must not load subjects or their usage")
			assert.Zero(t, counted.earliestReads, "persisted digests avoid scanning all historical activity")
			assert.Equal(t, 1, counted.pendingReads, "one bounded query finds all pending dates")
			assert.Equal(t, 1, counted.digestDateReads, "one bounded query finds all stored dates")

			seed(t, d, subject{id: "late", ended: "2026-09-21T13:00:00Z"})
			reps, err := r.CatchUp(t.Context())
			require.NoError(t, err)
			assert.Equal(t, []string{"2026-09-21"}, reportDates(reps))
			assert.Equal(t, 1, counted.subjectReads, "only the date with an unrecorded session needs rebuilding")
			assert.Zero(t, counted.earliestReads)
			assert.Equal(t, 2, counted.pendingReads)
			assert.Equal(t, 2, counted.digestDateReads)
		})
	}
}

func TestCatchUpBuildsDeferredDateAfterBudgetSkip(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seed(t, d, subject{id: "pending", ended: "2026-09-20T12:00:00Z", rules: "friction-old"},
		subject{id: "ready", ended: "2026-09-21T12:00:00Z"})
	r := newRunner(d)
	r.BackfillDays = 2
	first, err := r.CatchUp(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"2026-09-21"}, reportDates(first))
	assert.Nil(t, stored(t, d, "2026-09-20"))
	seed(t, d, subject{id: "pending", ended: "2026-09-20T12:00:00Z", rules: friction.SkippedRulesVersion})
	reps, err := r.CatchUp(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"2026-09-20"}, reportDates(reps))
	got := stored(t, d, "2026-09-20")
	require.NotNil(t, got)
	assert.Zero(t, got.SessionsScanned)
	assert.Equal(t, 1, stored(t, d, "2026-09-21").Revision)
}
