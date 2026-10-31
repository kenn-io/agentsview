//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawderive"
)

// Named regression for the shrunk ABA heartbeat counterexample, plus sibling mutations.
func TestRawJobsDelayedHeartbeatAfterReplay(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device", "replay", "")
	for operation := 0; operation <= 4; operation++ {
		_, err := f.runtime.ExecContext(t.Context(), `UPDATE raw_ingest_jobs SET state='ready',attempt_count=0,failed_at=NULL,lease_owner='',lease_expires_at=NULL`)
		require.NoError(t, err)
		old := f.lease(t, m)
		require.NoError(t, f.jobs.FailRawParseJob(t.Context(), old, "parse", "parse:internal"))
		result, err := f.sink.RetryFailedRawJobs(t.Context(), RawJobRetryFilter{ManifestID: m.ManifestID})
		require.NoError(t, err)
		require.Equal(t, int64(1), result.Replayed)
		fresh := f.lease(t, m)
		require.Equal(t, old.Attempt, fresh.Attempt)
		require.Equal(t, old.Owner, fresh.Owner)
		require.Greater(t, fresh.LeaseEpoch, old.LeaseEpoch)
		require.ErrorIs(t, recoveryOperation(t.Context(), f, m, old, operation), rawderive.ErrLeaseLost)
		var state string
		var epoch int64
		var published int
		require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state,lease_epoch,(SELECT count(*) FROM sessions)+(SELECT count(*) FROM raw_embedding_outbox) FROM raw_ingest_jobs WHERE id=$1`, fresh.ID).Scan(&state, &epoch, &published))
		require.Equal(t, "leased", state)
		require.Equal(t, fresh.LeaseEpoch, epoch)
		require.Zero(t, published)
	}
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
