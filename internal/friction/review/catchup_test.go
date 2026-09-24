package review_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
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
			name: "starts_after_latest_digest",
			seed: func(t *testing.T, d *db.DB, r *review.Runner) {
				t.Helper()
				seed(t, d, subject{id: "old", ended: "2026-09-01T10:00:00Z"})
				_, err := r.BuildDate(t.Context(), "2026-09-18", review.BuildOptions{})
				require.NoError(t, err)
			},
			backfill: 7,
			want:     []string{"2026-09-19", "2026-09-20", "2026-09-21"},
		},
		{
			name: "starts_at_earliest_session",
			seed: func(t *testing.T, d *db.DB, _ *review.Runner) {
				t.Helper()
				seed(t, d, subject{id: "new", ended: "2026-09-20T10:00:00Z"})
			},
			backfill: 7,
			want:     []string{"2026-09-20", "2026-09-21"},
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
	r.Now = func() time.Time { return time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC) } // 01:00 on the 22nd in Tokyo
	reps, err := r.CatchUp(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"2026-09-21"}, reportDates(reps))
	assert.Equal(t, "Asia/Tokyo", reps[0].Snapshot.Timezone)
}

func TestStaleSubjects(t *testing.T) {
	t.Run("stale_subjects_defer_then_build_after_grace", func(t *testing.T) {
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
		require.NotEmpty(t, reps)
		assert.Equal(t, "2026-09-21", reps[0].Date)
		assert.Equal(t, 1, reps[0].Snapshot.SessionsScanned, "the stale session is left out after the grace period")
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

	r.PublicURL = "https://av.example"
	require.NoError(t, r.Rerender(t.Context(), "2026-09-15"))
	after := stored(t, d, "2026-09-15")
	assert.Equal(t, before.Revision+1, after.Revision)
	assert.Equal(t, before.Markdown, after.Markdown, "markdown comes from the frozen snapshot")
	assert.Contains(t, string(after.SummaryJSON), "https://av.example/friction/2026-09-15")
	assert.Equal(t, before.SnapshotJSON, after.SnapshotJSON)

	require.ErrorIs(t, r.Rerender(t.Context(), "2020-01-01"), review.ErrDigestNotFound)
}
