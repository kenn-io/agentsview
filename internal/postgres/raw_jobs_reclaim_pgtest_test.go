//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Named regression for the shrunk crash-budget counterexample (maximum one).
func TestRawJobsCrashBudgetOneAttempt(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device", "crash", "")
	leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "same-owner", 1, time.Minute, 1)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	require.Equal(t, 1, leases[0].Attempt)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE manifest_id=$1`, m.ManifestID)
	require.NoError(t, err)
	leases, err = f.jobs.ClaimRawParseJobs(t.Context(), "same-owner", 1, time.Minute, 1)
	require.NoError(t, err)
	require.Empty(t, leases)
	var class string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT last_error_class FROM raw_ingest_jobs WHERE manifest_id=$1 AND failed_at IS NOT NULL`, m.ManifestID).Scan(&class))
	require.Equal(t, "lease:expired", class)
}

func TestRawJobsLoweredLimitAndBoundedReclaim(t *testing.T) {
	f := newProjectionFixture(t)
	for _, device := range []string{"a", "b", "c"} {
		f.accept(t, device, "bounded", "")
	}
	_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET state='retrying',attempt_count=10`)
	require.NoError(t, err)
	for want := 1; want <= 3; want++ {
		leases, err := f.jobs.ClaimRawParseJobs(t.Context(), "worker", 1, time.Minute, 5)
		require.NoError(t, err)
		require.Empty(t, leases)
		var count int
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM raw_ingest_jobs WHERE state='failed' AND last_error_class='attempts:exhausted' AND failed_at IS NOT NULL`).Scan(&count))
		require.Equal(t, want, count)
	}
}
