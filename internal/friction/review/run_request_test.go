package review

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestRunnerRun(t *testing.T) {
	now := time.Date(2026, 9, 16, 2, 30, 0, 0, time.UTC)
	newRunner := func(t *testing.T) *Runner {
		t.Helper()
		return &Runner{
			Store: dbtest.OpenTestDB(t), Loc: time.UTC,
			Now: func() time.Time { return now }, BackfillDays: 7,
		}
	}
	t.Run("latest_complete_date_is_yesterday_in_zone", func(t *testing.T) {
		r := newRunner(t)
		assert.Equal(t, "2026-09-15", r.LatestCompleteDate())
		tokyo, err := time.LoadLocation("Asia/Tokyo")
		require.NoError(t, err)
		r.Loc = tokyo
		assert.Equal(t, "2026-09-15", r.LatestCompleteDate())
		r.Now = func() time.Time { return time.Date(2026, 9, 16, 16, 0, 0, 0, time.UTC) }
		assert.Equal(t, "2026-09-16", r.LatestCompleteDate())
		r.Loc = nil
		assert.Equal(t, "2026-09-15", r.LatestCompleteDate())
	})
	t.Run("latest_complete_date_after_a_skipped_midnight", func(t *testing.T) {
		// Chile skipped 2026-09-06 00:00; that day is complete half an hour
		// into September 7.
		santiago, err := time.LoadLocation("America/Santiago")
		require.NoError(t, err)
		r := newRunner(t)
		r.Loc = santiago
		r.Now = func() time.Time { return time.Date(2026, 9, 7, 0, 30, 0, 0, santiago) }
		assert.Equal(t, "2026-09-06", r.LatestCompleteDate())
	})
	invalid := []struct {
		name string
		req  RunRequest
	}{
		{"malformed", RunRequest{Date: "2026-9-1"}},
		{"today", RunRequest{Date: "2026-09-16"}},
		{"future_dry_run", RunRequest{Date: "2026-10-01", DryRun: true}},
	}
	for _, tt := range invalid {
		t.Run("rejects_"+tt.name, func(t *testing.T) {
			err := newRunner(t).Run(t.Context(), tt.req, nil, nil)
			require.ErrorIs(t, err, ErrInvalidRunDate)
		})
	}
	t.Run("dry_run_without_date_targets_yesterday_and_writes_nothing", func(t *testing.T) {
		r := newRunner(t)
		reports, err := runReports(t, r, RunRequest{DryRun: true})
		require.NoError(t, err)
		require.Len(t, reports, 1)
		assert.Equal(t, "2026-09-15", reports[0].Date)
		assert.False(t, reports[0].Written)
		d, err := r.Store.GetFrictionDigest(t.Context(), "2026-09-15")
		require.NoError(t, err)
		assert.Nil(t, d)
	})
	t.Run("explicit_date_builds_once", func(t *testing.T) {
		r := newRunner(t)
		reports, err := runReports(t, r, RunRequest{Date: "2026-09-14"})
		require.NoError(t, err)
		require.Len(t, reports, 1)
		assert.True(t, reports[0].Written)
		again, err := runReports(t, r, RunRequest{Date: "2026-09-14"})
		require.NoError(t, err)
		require.Len(t, again, 1)
		assert.False(t, again[0].Written)
	})
}

func runReports(t *testing.T, r *Runner, req RunRequest) ([]Report, error) {
	t.Helper()
	var reports []Report
	err := r.Run(t.Context(), req, nil, func(rep Report) { reports = append(reports, rep) })
	return reports, err
}
