//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawderive"
	"hegel.dev/go/hegel"
)

// A reset retry budget must never make a delayed ownership token valid again.
func TestRawJobsDelayedOperationProperty(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device", "replay", "")
	hegel.Test(t, func(ht *hegel.T) {
		operation := hegel.Draw(ht, hegel.Integers(0, 4))
		_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET state='ready',attempt_count=0,lease_owner='',lease_expires_at=NULL`)
		require.NoError(ht, err)
		old := f.lease(t, m)
		require.NoError(ht, f.jobs.FailRawParseJob(t.Context(), old, "parse", "parse:internal"))
		_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET state='ready',attempt_count=0`)
		require.NoError(ht, err)
		_ = f.lease(t, m)
		switch operation {
		case 0:
			err = f.jobs.HeartbeatRawParseJob(t.Context(), old, time.Minute)
		case 1:
			err = f.jobs.FailRawParseJob(t.Context(), old, "parse", "parse:internal")
		case 2:
			err = f.jobs.RetryRawParseJob(t.Context(), old, time.Now(), "parse", "parse:internal")
		case 3:
			err = f.jobs.CompleteRawParseJob(t.Context(), old)
		case 4:
			err = f.sink.Project(t.Context(), old, m, projectionOutcome("stale"))
		}
		require.ErrorIs(ht, err, rawderive.ErrLeaseLost)
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestRawJobsRecoveryUpgrade(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device", "upgrade", "")
	lease := f.lease(t, m)
	require.NoError(t, f.jobs.FailRawParseJob(t.Context(), lease, "parse", "parse:internal"))
	_, err := f.admin.ExecContext(t.Context(), `ALTER TABLE raw_ingest_jobs DROP COLUMN IF EXISTS lease_epoch, DROP COLUMN IF EXISTS failed_at`)
	require.NoError(t, err)
	_, err = NewRawProjectionStore(f.runtime, RawProjectionOptions{Tenant: f.tenant})
	require.Error(t, err)
	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	var epoch int64
	var failedAt time.Time
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT lease_epoch,failed_at FROM raw_ingest_jobs WHERE id=$1`, lease.ID).Scan(&epoch, &failedAt))
	require.Zero(t, epoch)
	require.False(t, failedAt.IsZero())
	_, err = NewRawProjectionStore(f.runtime, RawProjectionOptions{Tenant: f.tenant})
	require.NoError(t, err)
}
