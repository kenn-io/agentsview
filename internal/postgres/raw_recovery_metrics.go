package postgres

import (
	"context"
	"database/sql"
	"encoding/json/v2"

	"go.kenn.io/agentsview/internal/rawsync"
)

// Embed in the caller's snapshot so ages, counts and outcome history agree.
const rawRecoveryMetricsSQL = `
WITH sources AS (
 SELECT p.*,j.state,m.accepted_at
 FROM raw_source_heads h JOIN raw_manifests m ON m.tenant_id=h.tenant_id AND m.manifest_id=h.manifest_id
 LEFT JOIN raw_source_projections p ON p.tenant_id=h.tenant_id AND p.device_id=h.device_id
  AND p.provider=h.provider AND p.configured_root_id=h.configured_root_id AND p.source_key_sha256=h.source_key_sha256
  AND p.selected_manifest_id=h.manifest_id
 LEFT JOIN raw_ingest_jobs j ON j.tenant_id=p.tenant_id AND j.id=p.selected_job_id
  AND j.manifest_id=p.selected_manifest_id AND j.processing_version=p.processing_version
  AND j.projection_generation=p.projection_generation AND j.stage='parse'
 WHERE h.tenant_id=$1
), queue AS (
 SELECT state,created_at FROM raw_ingest_jobs WHERE tenant_id=$1 AND state IN ('ready','retrying')
), uploads AS (
 SELECT created_at,size_bytes FROM raw_upload_sessions WHERE tenant_id=$1 AND state='open'
)
SELECT jsonb_build_object(
 'parse_lag_seconds',(SELECT max(GREATEST(0,extract(epoch FROM snapshot.observed_at-selected_at))) FROM sources WHERE selected_at IS NOT NULL AND state IS DISTINCT FROM 'complete'),
 'oldest_pending_source_age_seconds',(SELECT max(GREATEST(0,extract(epoch FROM snapshot.observed_at-accepted_at))) FROM sources WHERE state IS DISTINCT FROM 'complete'),
 'oldest_ready_age_seconds',(SELECT max(GREATEST(0,extract(epoch FROM snapshot.observed_at-created_at))) FROM queue WHERE state='ready'),
 'oldest_retrying_age_seconds',(SELECT max(GREATEST(0,extract(epoch FROM snapshot.observed_at-created_at))) FROM queue WHERE state='retrying'),
 'oldest_open_upload_age_seconds',(SELECT max(GREATEST(0,extract(epoch FROM snapshot.observed_at-created_at))) FROM uploads),
 'queued_bytes',(SELECT coalesce(sum(size_bytes),0) FROM uploads),
 'degraded_sources',(SELECT jsonb_build_object(
  'incomplete_membership',count(*) FILTER(WHERE membership_complete=false),
  'failed_without_projection',count(*) FILTER(WHERE state='failed' AND successful_manifest_id IS NULL),
  'serving_older_projection',count(*) FILTER(WHERE successful_processing_version IS NOT NULL AND successful_projection_generation IS NOT NULL AND
   (successful_manifest_id,successful_processing_version,successful_projection_generation) IS DISTINCT FROM (selected_manifest_id,processing_version,projection_generation)),
  'unknown',count(*) FILTER(WHERE selected_at IS NULL OR state IS NULL OR (successful_manifest_id IS NOT NULL AND (successful_processing_version IS NULL OR successful_projection_generation IS NULL)))) FROM sources),
 'outcome_buckets',(SELECT coalesce(jsonb_agg(jsonb_build_object('bucket_start',bucket_start,'stage',stage,'outcome',outcome,'count',count) ORDER BY bucket_start,stage,outcome),'[]')
  FROM raw_job_outcome_buckets WHERE tenant_id=$1 AND bucket_start>=date_trunc('hour',snapshot.observed_at,'UTC')-interval '23 hours' AND bucket_start<=snapshot.observed_at)
)`

// Ordinary raw custody databases have no projection metadata or outcome table.
func rawRecoveryExpression(
	ctx context.Context,
	q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	},
) (string, error) {
	var hosted bool
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('hosted_tenant_binding') IS NOT NULL`).Scan(&hosted); err != nil {
		return "", err
	}
	if !hosted {
		return "NULL::text", nil
	}
	return "(" + rawRecoveryMetricsSQL + ")::text", nil
}

func decodeRawRecovery(body *string) (*rawsync.RecoveryMetrics, error) {
	if body == nil {
		return nil, nil
	}
	var metrics rawsync.RecoveryMetrics
	if err := json.Unmarshal([]byte(*body), &metrics); err != nil {
		return nil, err
	}
	for i := range metrics.OutcomeBuckets {
		metrics.OutcomeBuckets[i].BucketStart = metrics.OutcomeBuckets[i].BucketStart.UTC()
	}
	return &metrics, nil
}
