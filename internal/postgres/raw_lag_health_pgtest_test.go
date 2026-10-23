//go:build pgtest

package postgres

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRawSelectionProvenanceRollout(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "rollout", "")
	lease := f.lease(t, m)
	require.NoError(t, f.sink.Project(t.Context(), lease, m, projectionOutcome("content")))
	var selected time.Time
	var version string
	var generation int64
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT selected_at,successful_processing_version,successful_projection_generation FROM raw_source_projections`).Scan(&selected, &version, &generation))
	assert.Equal(t, "parser-1", version)
	assert.Equal(t, lease.ProjectionGeneration, generation)
	next, err := f.sink.SelectSourceGeneration(t.Context(), m, "parser-2")
	require.NoError(t, err)
	var newer time.Time
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT selected_at,successful_processing_version,successful_projection_generation FROM raw_source_projections`).Scan(&newer, &version, &generation))
	assert.True(t, newer.After(selected))
	assert.Equal(t, "parser-1", version)
	assert.Less(t, generation, next)
	_, err = f.sink.SelectSourceGeneration(t.Context(), m, "parser-2")
	require.NoError(t, err)
	var unchanged time.Time
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT selected_at FROM raw_source_projections`).Scan(&unchanged))
	assert.Equal(t, newer, unchanged)

	back, err := f.sink.SelectSourceGeneration(t.Context(), m, "parser-1")
	require.NoError(t, err)
	assert.Greater(t, back, next)
	empty := projectionOutcome("unused")
	empty.Outcome.Results = nil
	empty.Outcome.ResultSetComplete = false
	require.ErrorIs(t, f.sink.Project(t.Context(), f.lease(t, m), m, empty), rawderive.ErrProjectionRetrying)
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT successful_processing_version,successful_projection_generation FROM raw_source_projections`).Scan(&version, &generation))
	assert.Equal(t, "parser-1", version)
	assert.Equal(t, lease.ProjectionGeneration, generation)
	report, err := f.jobs.RawJobHealth(t.Context(), m.Identity, rawsync.JobHealthQuery{MaxAttempts: 5, StaleAfterSeconds: 3600})
	require.NoError(t, err)
	require.NotNil(t, report.Recovery)
	assert.Equal(t, int64(1), report.Recovery.DegradedSources.ServingOlderProjection)
}

func TestRawSelectionPartialTerminalProvenance(t *testing.T) {
	f := newProjectionFixture(t)
	f.sink.options.RetryPolicy = rawderive.RetryPolicy{Base: time.Second, Maximum: time.Minute, MaxAttempts: 1}
	m, _ := f.accept(t, "device-a", "partial", "")
	lease := f.lease(t, m)
	partial := projectionOutcome("partial")
	partial.Outcome.ResultSetComplete = false
	require.ErrorIs(t, f.sink.Project(t.Context(), lease, m, partial), rawderive.ErrProjectionFailed)
	var version string
	var generation int64
	var complete bool
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT successful_processing_version,successful_projection_generation,membership_complete FROM raw_source_projections`).Scan(&version, &generation, &complete))
	assert.Equal(t, lease.ProcessingVersion, version)
	assert.Equal(t, lease.ProjectionGeneration, generation)
	assert.False(t, complete)

	report, err := f.jobs.RawJobHealth(t.Context(), m.Identity, rawsync.JobHealthQuery{MaxAttempts: 1, StaleAfterSeconds: 3600})
	require.NoError(t, err)
	require.NotNil(t, report.Recovery)
	assert.Equal(t, rawsync.DegradedSources{IncompleteMembership: 1}, report.Recovery.DegradedSources)
	require.Len(t, report.Recovery.OutcomeBuckets, 1)
	assert.Equal(t, "failed", report.Recovery.OutcomeBuckets[0].Outcome)
	assert.Equal(t, int64(1), report.Recovery.OutcomeBuckets[0].Count)
}

func TestRawSelectionLegacyProvenanceRemainsUnknown(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "legacy", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("old")))
	_, err := f.admin.ExecContext(t.Context(), `ALTER TABLE raw_source_projections DROP COLUMN selected_at, DROP COLUMN successful_processing_version, DROP COLUMN successful_projection_generation`)
	require.NoError(t, err)
	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	var unknown bool
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT selected_at IS NULL AND successful_processing_version IS NULL AND successful_projection_generation IS NULL FROM raw_source_projections`).Scan(&unknown))
	assert.True(t, unknown)
}

func TestRawLagHealthRolloutAndUnknown(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "health", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("complete")))
	_, err := f.sink.SelectSourceGeneration(t.Context(), m, "parser-2")
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_source_projections SET selected_at=now()-interval '2 hours'; UPDATE raw_ingest_jobs SET created_at=now()-interval '3 hours' WHERE state='ready'`)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO raw_upload_sessions(upload_id,device_id,provider,sha256,size_bytes,offset_bytes,created_at,updated_at,expires_at) VALUES('open','device-a','codex',repeat('a',64),100,20,now()-interval '1 hour',now(),now()+interval '1 day')`)
	require.NoError(t, err)
	report, err := f.jobs.RawJobHealth(t.Context(), m.Identity, rawsync.JobHealthQuery{MaxAttempts: 5, StaleAfterSeconds: 3600})
	require.NoError(t, err)
	body, err := json.Marshal(report)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(body, &wire))
	metrics, ok := wire["recovery"].(map[string]any)
	require.True(t, ok, "missing hosted recovery metrics")
	assert.GreaterOrEqual(t, metrics["parse_lag_seconds"], float64(7200))
	assert.GreaterOrEqual(t, metrics["oldest_ready_age_seconds"], float64(10800))
	assert.Equal(t, float64(100), metrics["queued_bytes"])
	assert.GreaterOrEqual(t, metrics["oldest_open_upload_age_seconds"], float64(3600))
	status, err := f.jobs.ReadRawSyncStatus(t.Context(), m.Identity)
	require.NoError(t, err)
	statusJSON, err := json.Marshal(status)
	require.NoError(t, err)
	var statusWire map[string]any
	require.NoError(t, json.Unmarshal(statusJSON, &statusWire))
	statusMetrics, ok := statusWire["recovery"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(100), statusMetrics["queued_bytes"])
	coverage := metrics["degraded_sources"].(map[string]any)
	assert.Equal(t, float64(1), coverage["serving_older_projection"])
	assert.Equal(t, float64(0), coverage["unknown"])
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE raw_source_projections SET selected_at=NULL,successful_processing_version=NULL,successful_projection_generation=NULL`)
	require.NoError(t, err)
	report, err = f.jobs.RawJobHealth(t.Context(), m.Identity, rawsync.JobHealthQuery{MaxAttempts: 5, StaleAfterSeconds: 3600})
	require.NoError(t, err)
	body, err = json.Marshal(report)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &wire))
	metrics = wire["recovery"].(map[string]any)
	assert.Nil(t, metrics["parse_lag_seconds"])
	coverage = metrics["degraded_sources"].(map[string]any)
	assert.Equal(t, float64(1), coverage["unknown"])
}
