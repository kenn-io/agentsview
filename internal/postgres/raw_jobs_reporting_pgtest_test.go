//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawJobsReporting(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device", "reporting", "")
	lease := f.lease(t, m)
	require.NoError(t, f.jobs.FailRawParseJob(t.Context(), lease, "parse", "parse:internal"))
	oldest := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET failed_at=$1`, oldest)
	require.NoError(t, err)
	counts, err := f.sink.ListRawJobs(t.Context())
	require.NoError(t, err)
	require.Len(t, counts, 1)
	require.Equal(t, RawJobCount{State: "failed", ErrorClass: "parse", Count: 1, OldestFailedAt: &oldest}, counts[0])
	report, err := rawJobHealth(t.Context(), f.runtime, f.sink.options.Tenant, rawsync.JobHealthQuery{MaxAttempts: 5, StaleAfterSeconds: 60})
	require.NoError(t, err)
	require.Equal(t, int64(1), report.DeadLetters.Count)
	require.Equal(t, &oldest, report.DeadLetters.OldestFailedAt)
	_, err = f.sink.RetryFailedRawJobs(t.Context(), RawJobRetryFilter{All: true})
	require.NoError(t, err)
	report, err = rawJobHealth(t.Context(), f.runtime, f.sink.options.Tenant, rawsync.JobHealthQuery{MaxAttempts: 5, StaleAfterSeconds: 60})
	require.NoError(t, err)
	require.Zero(t, report.DeadLetters.Count)
	require.Nil(t, report.DeadLetters.OldestFailedAt)
}
