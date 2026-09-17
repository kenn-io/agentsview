package postgres

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/rawsync"
)

const (
	rawJobHealthMaxRows         = 50
	rawJobHealthMaxErrorClasses = 20
)

// rawJobHealthSQL reads all hosted raw health indicators from one PostgreSQL
// statement and one statement timestamp.
const rawJobHealthSQL = `
WITH snapshot AS MATERIALIZED (
	SELECT statement_timestamp() AS observed_at
), orphaned AS (
	SELECT
		head.device_id,
		manifest.manifest_id,
		manifest.provider,
		manifest.configured_root_id,
		manifest.source_key_sha256,
		manifest.generation,
		manifest.kind,
		manifest.accepted_at
	FROM raw_source_heads AS head
	JOIN raw_manifests AS manifest
		ON manifest.tenant_id = head.tenant_id
		AND manifest.device_id = head.device_id
		AND manifest.provider = head.provider
		AND manifest.configured_root_id = head.configured_root_id
		AND manifest.source_key_sha256 = head.source_key_sha256
		AND manifest.manifest_id = head.manifest_id
	WHERE head.tenant_id = $1
		AND head.generation > 0
		AND head.manifest_id IS NOT NULL
		AND NOT EXISTS (
			SELECT 1
			FROM raw_ingest_jobs AS job
			WHERE job.tenant_id = manifest.tenant_id
				AND job.manifest_id = manifest.manifest_id
				AND job.stage = 'parse'
		)
), orphaned_capped AS (
	SELECT *
	FROM orphaned
	ORDER BY accepted_at DESC, manifest_id, device_id,
		provider, configured_root_id, source_key_sha256
	LIMIT $4
), expired AS (
	SELECT
		job.id AS job_id,
		manifest.device_id,
		job.manifest_id,
		job.processing_version,
		manifest.provider,
		manifest.configured_root_id,
		manifest.source_key_sha256,
		job.attempt_count,
		job.lease_expires_at,
		job.updated_at
	FROM raw_ingest_jobs AS job
	JOIN raw_manifests AS manifest
		ON manifest.tenant_id = job.tenant_id
		AND manifest.manifest_id = job.manifest_id
	CROSS JOIN snapshot
	WHERE job.tenant_id = $1
		AND job.stage = 'parse'
		AND job.state = 'leased'
		AND job.lease_expires_at IS NOT NULL
		AND job.lease_expires_at <= snapshot.observed_at
), expired_capped AS (
	SELECT *
	FROM expired
	ORDER BY lease_expires_at, job_id
	LIMIT $4
), failed AS (
	SELECT
		job.last_error_class AS error_class,
		count(*)::bigint AS job_count,
		max(job.updated_at) AS latest_failure_at
	FROM raw_ingest_jobs AS job
	WHERE job.tenant_id = $1
		AND job.stage = 'parse'
		AND job.state = 'failed'
	GROUP BY job.last_error_class
), failed_capped AS (
	SELECT *
	FROM failed
	ORDER BY job_count DESC, error_class
	LIMIT $5
), retrying AS (
	SELECT
		job.id AS job_id,
		manifest.device_id,
		job.manifest_id,
		job.processing_version,
		manifest.provider,
		manifest.configured_root_id,
		manifest.source_key_sha256,
		job.attempt_count,
		job.available_at,
		job.updated_at,
		job.last_error_class
	FROM raw_ingest_jobs AS job
	JOIN raw_manifests AS manifest
		ON manifest.tenant_id = job.tenant_id
		AND manifest.manifest_id = job.manifest_id
	WHERE job.tenant_id = $1
		AND job.stage = 'parse'
		AND job.state = 'retrying'
		AND job.attempt_count >= GREATEST(1, $2::integer - 1)
), retrying_capped AS (
	SELECT *
	FROM retrying
	ORDER BY attempt_count DESC, job_id
	LIMIT $4
), stale AS (
	SELECT
		head.device_id,
		head.provider,
		head.configured_root_id,
		head.source_key_sha256,
		head.manifest_id,
		head.generation,
		manifest.kind,
		head.updated_at
	FROM raw_source_heads AS head
	JOIN raw_manifests AS manifest
		ON manifest.tenant_id = head.tenant_id
		AND manifest.device_id = head.device_id
		AND manifest.provider = head.provider
		AND manifest.configured_root_id = head.configured_root_id
		AND manifest.source_key_sha256 = head.source_key_sha256
		AND manifest.manifest_id = head.manifest_id
	CROSS JOIN snapshot
	WHERE head.tenant_id = $1
		AND head.generation > 0
		AND head.updated_at <= snapshot.observed_at -
			($3::bigint * interval '1 second')
), stale_capped AS (
	SELECT *
	FROM stale
	ORDER BY updated_at, device_id, provider, configured_root_id,
		source_key_sha256, manifest_id
	LIMIT $4
	)
SELECT
	snapshot.observed_at,
	(SELECT count(*)::bigint FROM orphaned),
	COALESCE((
		SELECT jsonb_agg(jsonb_build_object(
			'manifest_id', manifest_id,
			'device_id', device_id,
			'provider', provider,
			'configured_root_id', configured_root_id,
			'source_key_sha256', source_key_sha256,
			'generation', generation,
			'kind', kind,
			'accepted_at', accepted_at
		) ORDER BY accepted_at DESC, manifest_id, device_id,
			provider, configured_root_id, source_key_sha256)::text
		FROM orphaned_capped
	), '[]'),
	(SELECT count(*)::bigint FROM expired),
	COALESCE((
		SELECT jsonb_agg(jsonb_build_object(
			'job_id', job_id,
			'device_id', device_id,
			'manifest_id', manifest_id,
			'processing_version', processing_version,
			'provider', provider,
			'configured_root_id', configured_root_id,
			'source_key_sha256', source_key_sha256,
			'attempt_count', attempt_count,
			'lease_expires_at', lease_expires_at,
			'updated_at', updated_at
		) ORDER BY lease_expires_at, job_id)::text
		FROM expired_capped
	), '[]'),
	(SELECT coalesce(sum(job_count), 0)::bigint FROM failed),
	COALESCE((
		SELECT jsonb_agg(jsonb_build_object(
			'error_class', error_class,
			'job_count', job_count,
			'latest_failure_at', latest_failure_at
		) ORDER BY job_count DESC, error_class)::text
		FROM failed_capped
	), '[]'),
	(SELECT count(*)::bigint FROM retrying),
	COALESCE((
		SELECT jsonb_agg(jsonb_build_object(
			'job_id', job_id,
			'device_id', device_id,
			'manifest_id', manifest_id,
			'processing_version', processing_version,
			'provider', provider,
			'configured_root_id', configured_root_id,
			'source_key_sha256', source_key_sha256,
			'attempt_count', attempt_count,
			'available_at', available_at,
			'updated_at', updated_at,
			'last_error_class', last_error_class
		) ORDER BY attempt_count DESC, job_id)::text
		FROM retrying_capped
	), '[]'),
	(SELECT count(*)::bigint FROM stale),
	COALESCE((
		SELECT jsonb_agg(jsonb_build_object(
			'device_id', device_id,
			'provider', provider,
			'configured_root_id', configured_root_id,
			'source_key_sha256', source_key_sha256,
			'manifest_id', manifest_id,
			'generation', generation,
			'kind', kind,
			'updated_at', updated_at
		) ORDER BY updated_at, device_id, provider, configured_root_id,
			source_key_sha256, manifest_id)::text
		FROM stale_capped
	), '[]')
FROM snapshot`

// RawJobHealth returns one tenant-scoped, read-only raw-sync observation.
func (s *RawIngestStore) RawJobHealth(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	query rawsync.JobHealthQuery,
) (rawsync.JobHealthReport, error) {
	if err := validateRawIngestIdentity(identity); err != nil {
		return rawsync.JobHealthReport{}, err
	}
	if err := query.Validate(); err != nil {
		return rawsync.JobHealthReport{}, err
	}

	var (
		observedAt             time.Time
		orphanedManifestCount  int64
		orphanedManifestsJSON  string
		expiredLeaseCount      int64
		expiredLeasesJSON      string
		failedJobCount         int64
		failedJobsByClassJSON  string
		retryingNearLimitCount int64
		retryingNearLimitJSON  string
		staleSourceHeadCount   int64
		staleSourceHeadsJSON   string
	)
	if err := s.db.QueryRowContext(
		ctx, rawJobHealthSQL, identity.TenantID,
		query.MaxAttempts, query.StaleAfterSeconds,
		rawJobHealthMaxRows, rawJobHealthMaxErrorClasses,
	).Scan(
		&observedAt,
		&orphanedManifestCount, &orphanedManifestsJSON,
		&expiredLeaseCount, &expiredLeasesJSON,
		&failedJobCount, &failedJobsByClassJSON,
		&retryingNearLimitCount, &retryingNearLimitJSON,
		&staleSourceHeadCount, &staleSourceHeadsJSON,
	); err != nil {
		return rawsync.JobHealthReport{}, fmt.Errorf("reading raw job health: %w", err)
	}

	report := rawsync.JobHealthReport{
		ObservedAt:             observedAt,
		MaxAttempts:            query.MaxAttempts,
		StaleAfterSeconds:      query.StaleAfterSeconds,
		OrphanedManifests:      make([]rawsync.OrphanedManifest, 0),
		OrphanedManifestCount:  orphanedManifestCount,
		ExpiredLeases:          make([]rawsync.ExpiredLease, 0),
		ExpiredLeaseCount:      expiredLeaseCount,
		FailedJobsByErrorClass: make([]rawsync.JobFailureClass, 0),
		FailedJobCount:         failedJobCount,
		RetryingNearLimit:      make([]rawsync.JobAttemptWarning, 0),
		RetryingNearLimitCount: retryingNearLimitCount,
		StaleSourceHeads:       make([]rawsync.StaleSourceHead, 0),
		StaleSourceHeadCount:   staleSourceHeadCount,
	}
	if err := json.Unmarshal([]byte(orphanedManifestsJSON), &report.OrphanedManifests); err != nil {
		return rawsync.JobHealthReport{}, fmt.Errorf("decoding orphaned raw manifests: %w", err)
	}
	if err := json.Unmarshal([]byte(expiredLeasesJSON), &report.ExpiredLeases); err != nil {
		return rawsync.JobHealthReport{}, fmt.Errorf("decoding expired raw leases: %w", err)
	}
	if err := json.Unmarshal([]byte(failedJobsByClassJSON), &report.FailedJobsByErrorClass); err != nil {
		return rawsync.JobHealthReport{}, fmt.Errorf("decoding failed raw job classes: %w", err)
	}
	if err := json.Unmarshal([]byte(retryingNearLimitJSON), &report.RetryingNearLimit); err != nil {
		return rawsync.JobHealthReport{}, fmt.Errorf("decoding retrying raw jobs: %w", err)
	}
	if err := json.Unmarshal([]byte(staleSourceHeadsJSON), &report.StaleSourceHeads); err != nil {
		return rawsync.JobHealthReport{}, fmt.Errorf("decoding stale raw heads: %w", err)
	}
	return report, nil
}
