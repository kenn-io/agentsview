//go:build pgtest

package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawderive"
)

func TestParityQueueClaimReclaimsOnlyAbandonedUnsealedEpoch(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	store, err := NewMigrationParityStore(t.Context(), f.runtime, MigrationParityOptions{
		Schema: f.schema, Tenant: f.tenant,
	})
	require.NoError(t, err)
	_, err = store.CreateOrResumeParity(t.Context(), parityTestRequest(), 2)
	require.NoError(t, err)

	first, err := store.ClaimParity(t.Context(), "owner-first", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, int64(1), first.InitEpoch)
	assert.Equal(t, 2, first.BatchSize)
	assert.False(t, first.Token == "")
	binding := parityPhysicalBinding(f.tenant)
	binding.Request = first.Request
	binding.ObservedAt = time.Date(2026, 9, 12, 10, 11, 12, 345678000, time.UTC)
	*first, err = store.BindParity(t.Context(), *first, binding)
	require.NoError(t, err)

	insertParityQueueSource(t, f, *first, "abandoned-source")
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1`, first.RunID)
	require.NoError(t, err)

	reclaimed, err := store.ClaimParity(t.Context(), "owner-second", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reclaimed)
	assert.Equal(t, int64(2), reclaimed.InitEpoch)
	assert.NotEqual(t, first.Token, reclaimed.Token)
	assert.Equal(t, first.BindingDigest, reclaimed.BindingDigest)
	assert.Equal(t, first.ObservedAt, reclaimed.ObservedAt)
	_, err = store.BindParity(t.Context(), *reclaimed, binding)
	require.NoError(t, err)
	changed := binding
	changed.Versions.ParserBuild[0]++
	_, err = store.BindParity(t.Context(), *reclaimed, changed)
	require.ErrorContains(t, err, "binding_conflict")
	var abandoned int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_sources WHERE run_id=$1`, first.RunID).Scan(&abandoned))
	assert.Zero(t, abandoned)
	require.ErrorContains(t, store.HeartbeatParity(t.Context(), *first, time.Minute), "binding_conflict")
	require.ErrorContains(t, store.FinishParityRequest(t.Context(), *first, "pending"), "binding_conflict")

	insertParityQueueSource(t, f, *reclaimed, "sealed-source")
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE migration_parity_runs SET baseline_sealed=TRUE,lease_expires_at=clock_timestamp()-interval '1 second' WHERE run_id=$1`, reclaimed.RunID)
	require.NoError(t, err)
	sealedResume, err := store.ClaimParity(t.Context(), "owner-third", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, sealedResume)
	assert.Equal(t, reclaimed.InitEpoch, sealedResume.InitEpoch)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM migration_parity_sources WHERE run_id=$1`, first.RunID).Scan(&abandoned))
	assert.Equal(t, 1, abandoned)
}

func TestParityQueueFinishAndExplicitResumeAdvanceOneGeneration(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	store, err := NewMigrationParityStore(t.Context(), f.runtime, MigrationParityOptions{
		Schema: f.schema, Tenant: f.tenant,
	})
	require.NoError(t, err)
	firstReport, err := store.CreateOrResumeParity(t.Context(), parityTestRequest(), 1)
	require.NoError(t, err)
	activeRepeat, err := store.CreateOrResumeParity(t.Context(), parityTestRequest(), 1)
	require.NoError(t, err)
	assert.Equal(t, firstReport.RequestGeneration, activeRepeat.RequestGeneration)

	lease, err := store.ClaimParity(t.Context(), "owner", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.NoError(t, store.FinishParityRequest(t.Context(), *lease, "pending"))
	finished, err := store.ReadParityRequestReport(t.Context(), lease.RunID, lease.RequestGeneration)
	require.NoError(t, err)
	assert.Equal(t, "complete", finished.State)
	assert.Equal(t, lease.RequestGeneration, finished.CompletedGeneration)

	resumed, err := store.CreateOrResumeParity(t.Context(), parityTestRequest(), 1)
	require.NoError(t, err)
	assert.Equal(t, lease.RequestGeneration+1, resumed.RequestGeneration)
	assert.Equal(t, "unchecked", resumed.Freshness)
	assert.Equal(t, "requested", resumed.State)
}

func TestParityQueueActiveLeaseCannotBeClaimedTwice(t *testing.T) {
	f := newHostedFixture(t, "tenant-a")
	store, err := NewMigrationParityStore(t.Context(), f.runtime, MigrationParityOptions{
		Schema: f.schema, Tenant: f.tenant,
	})
	require.NoError(t, err)
	_, err = store.CreateOrResumeParity(t.Context(), parityTestRequest(), 1)
	require.NoError(t, err)
	lease, err := store.ClaimParity(t.Context(), "owner", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)

	none, err := store.ClaimParity(t.Context(), "other-owner", time.Minute)
	require.NoError(t, err)
	assert.Nil(t, none)
	require.NoError(t, store.HeartbeatParity(t.Context(), *lease, time.Minute))
}

func insertParityQueueSource(t *testing.T, f hostedFixture, lease rawderive.ParityLease, sourceID string) {
	t.Helper()
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_receipt,head_generation,source_key_sha256,dependency_digest,code,required)
		VALUES($1,$2,$3,'raw','device-a','claude','root-a','manifest','receipt',1,'source-key',decode(repeat('01',32),'hex'),'pending',TRUE)`, lease.RunID, lease.InitEpoch, sourceID)
	require.NoError(t, err)
}
