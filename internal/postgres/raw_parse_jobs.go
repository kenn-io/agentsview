package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

const (
	// maxRawParseSupersedeBatch bounds how many obsolete jobs one short claim
	// may retire, so per-call mutation work stays bounded while a backlog
	// drains across successive claims.
	maxRawParseSupersedeBatch = 4 * rawderive.MaxClaimBatchSize
)

// ClaimRawParseJobs leases available current-head parse jobs without waiting on
// jobs claimed concurrently by other workers. A claim that fills its whole
// batch from current heads skips the obsolete-job cleanup scan entirely, so
// draining a healthy backlog pays no head probes; only a claim that cannot
// fill its batch runs the bounded cleanup.
func (s *RawIngestStore) ClaimRawParseJobs(
	ctx context.Context,
	owner string,
	limit int,
	leaseDuration time.Duration,
) ([]rawderive.JobLease, error) {
	if err := validateRawParseLeaseRequest(owner, limit, leaseDuration); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning raw parse job claim: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	query, args := s.rawParseClaimStatement(owner, limit, leaseDuration)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("claiming raw parse jobs: %w", err)
	}
	leasing := make([]rawderive.JobLease, 0, limit)
	for rows.Next() {
		var lease rawderive.JobLease
		if err := rows.Scan(
			&lease.ID,
			&lease.Identity.TenantID,
			&lease.Identity.DeviceID,
			&lease.ManifestID,
			&lease.ProcessingVersion,
			&lease.ProjectionGeneration,
			&lease.Attempt,
			&lease.Owner,
			&lease.ExpiresAt,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scanning raw parse job lease: %w", err)
		}
		leasing = append(leasing, lease)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("closing raw parse job leases: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading raw parse job leases: %w", err)
	}
	// The claim predicate above already refuses every obsolete row, so
	// supersession only reclaims rows no claim could ever lease. Running it
	// after a short claim keeps cleanup bounded without charging the common
	// full-batch path for an O(backlog) head-probe scan.
	if len(leasing) < limit && s.hostedVersion == "" {
		if err := supersedeTenantObsoleteRawParseJobs(
			ctx, tx, maxRawParseSupersedeBatch, s.tenant,
		); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing raw parse job claim: %w", err)
	}
	committed = true
	return leasing, nil
}

// supersedeObsoleteRawParseJobs retires at most limit jobs whose manifest is
// no longer the current source head. Each call performs bounded mutation
// work: candidates are locked with FOR UPDATE OF obsolete SKIP LOCKED in a
// deterministic id order — only the job rows being retired, never joined
// manifest rows — so concurrent claims cooperate instead of piling up lock
// traffic, and a large obsolete backlog drains across successive calls.
func supersedeObsoleteRawParseJobs(
	ctx context.Context,
	tx *sql.Tx,
	limit int,
) error {
	if _, err := tx.ExecContext(ctx, rawParseSupersedeSQL,
		limit,
	); err != nil {
		return fmt.Errorf("superseding obsolete raw parse jobs: %w", err)
	}
	return nil
}

// Future retries stay outside the idle cleanup scan until they become due.
const rawParseSupersedePrefix = `
		UPDATE raw_ingest_jobs AS job
		SET state = 'superseded', lease_owner = '', lease_expires_at = NULL,
			updated_at = now()
		WHERE job.id IN (
			SELECT obsolete.id
			FROM raw_ingest_jobs AS obsolete
			JOIN raw_manifests AS manifest
				ON manifest.tenant_id = obsolete.tenant_id
				AND manifest.manifest_id = obsolete.manifest_id
			WHERE (
					(obsolete.state IN ('ready', 'retrying') AND obsolete.available_at <= now())
					OR (obsolete.state = 'leased' AND obsolete.lease_expires_at <= now())
			)
				AND obsolete.stage = 'parse'`
const rawParseSupersedeSuffix = `
				AND NOT EXISTS (
					SELECT 1
					FROM raw_source_heads AS head
					WHERE head.tenant_id = manifest.tenant_id
						AND head.device_id = manifest.device_id
						AND head.provider = manifest.provider
						AND head.configured_root_id = manifest.configured_root_id
						AND head.source_key_sha256 = manifest.source_key_sha256
						AND head.manifest_id = manifest.manifest_id
				)
			ORDER BY obsolete.id
			LIMIT $1
			FOR UPDATE OF obsolete SKIP LOCKED
		)`

const rawParseSupersedeSQL = rawParseSupersedePrefix + rawParseSupersedeSuffix
const rawParseSupersedeTenantSQL = rawParseSupersedePrefix + ` AND obsolete.tenant_id = $2 ` + rawParseSupersedeSuffix + ` AND job.tenant_id = $2 `

func supersedeTenantObsoleteRawParseJobs(ctx context.Context, tx *sql.Tx, limit int, tenant string) error {
	if tenant == "" {
		return supersedeObsoleteRawParseJobs(ctx, tx, limit)
	}
	_, err := tx.ExecContext(ctx, rawParseSupersedeTenantSQL, limit, tenant)
	if err != nil {
		return fmt.Errorf("superseding tenant raw parse jobs: %w", err)
	}
	return nil
}

// HeartbeatRawParseJob extends an active lease held by the current source head.
func (s *RawIngestStore) HeartbeatRawParseJob(
	ctx context.Context,
	lease rawderive.JobLease,
	leaseDuration time.Duration,
) error {
	if s.tenant != "" && lease.Identity.TenantID != s.tenant {
		return rawderive.ErrLeaseLost
	}
	if err := validateRawParseLease(lease); err != nil {
		return err
	}
	if err := validateRawParseLeaseDuration(leaseDuration); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE raw_ingest_jobs AS job
		SET lease_expires_at = now() + ($4 * interval '1 microsecond'),
			updated_at = now()
		WHERE job.id = $1 AND job.lease_owner = $2 AND job.attempt_count = $3
			AND ($5 = '' OR (job.tenant_id = $5 AND job.manifest_id = $6 AND job.processing_version = $7))
			AND job.state = 'leased' AND job.lease_expires_at > now()
			AND EXISTS (
				SELECT 1
				FROM raw_manifests AS manifest
				JOIN raw_source_heads AS head
					ON head.tenant_id = manifest.tenant_id
					AND head.device_id = manifest.device_id
					AND head.provider = manifest.provider
					AND head.configured_root_id = manifest.configured_root_id
					AND head.source_key_sha256 = manifest.source_key_sha256
					AND head.manifest_id = manifest.manifest_id
				WHERE manifest.tenant_id = job.tenant_id
					AND manifest.manifest_id = job.manifest_id
			)`,
		lease.ID, lease.Owner, lease.Attempt, leaseDuration.Microseconds(), s.tenant, lease.ManifestID, lease.ProcessingVersion,
	)
	if err != nil {
		return fmt.Errorf("heartbeating raw parse job: %w", err)
	}
	return requireRawParseLeaseUpdate(result)
}

// CompleteRawParseJob marks an active current-head lease complete.
func (s *RawIngestStore) CompleteRawParseJob(
	ctx context.Context,
	lease rawderive.JobLease,
) error {
	if s.tenant != "" && lease.Identity.TenantID != s.tenant {
		return rawderive.ErrLeaseLost
	}
	if err := validateRawParseLease(lease); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE raw_ingest_jobs AS job
		SET state = 'complete', lease_owner = '', lease_expires_at = NULL,
			last_error_class = '', last_error = '', updated_at = now()
		WHERE job.id = $1 AND job.lease_owner = $2 AND job.attempt_count = $3
			AND ($4 = '' OR (job.tenant_id = $4 AND job.manifest_id = $5 AND job.processing_version = $6))
			AND job.state = 'leased' AND job.lease_expires_at > now()
			AND EXISTS (
				SELECT 1
				FROM raw_manifests AS manifest
				JOIN raw_source_heads AS head
					ON head.tenant_id = manifest.tenant_id
					AND head.device_id = manifest.device_id
					AND head.provider = manifest.provider
					AND head.configured_root_id = manifest.configured_root_id
					AND head.source_key_sha256 = manifest.source_key_sha256
					AND head.manifest_id = manifest.manifest_id
				WHERE manifest.tenant_id = job.tenant_id
					AND manifest.manifest_id = job.manifest_id
			)`,
		lease.ID, lease.Owner, lease.Attempt, s.tenant, lease.ManifestID, lease.ProcessingVersion,
	)
	if err != nil {
		return fmt.Errorf("completing raw parse job: %w", err)
	}
	return requireRawParseLeaseUpdate(result)
}

// RetryRawParseJob releases an active lease for a later attempt.
func (s *RawIngestStore) RetryRawParseJob(
	ctx context.Context,
	lease rawderive.JobLease,
	availableAt time.Time,
	errorClass string,
	message string,
) error {
	if s.tenant != "" && lease.Identity.TenantID != s.tenant {
		return rawderive.ErrLeaseLost
	}
	if err := validateRawParseLease(lease); err != nil {
		return err
	}
	if availableAt.IsZero() {
		return fmt.Errorf("%w: retry time is required", rawsync.ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE raw_ingest_jobs AS job
		SET state = 'retrying', available_at = $4, lease_owner = '',
			lease_expires_at = NULL, last_error_class = $5, last_error = $6,
			updated_at = now()
		WHERE job.id = $1 AND job.lease_owner = $2 AND job.attempt_count = $3
			AND ($7 = '' OR (job.tenant_id = $7 AND job.manifest_id = $8 AND job.processing_version = $9))
			AND job.state = 'leased' AND job.lease_expires_at > now()
			AND EXISTS (
				SELECT 1
				FROM raw_manifests AS manifest
				JOIN raw_source_heads AS head
					ON head.tenant_id = manifest.tenant_id
					AND head.device_id = manifest.device_id
					AND head.provider = manifest.provider
					AND head.configured_root_id = manifest.configured_root_id
					AND head.source_key_sha256 = manifest.source_key_sha256
					AND head.manifest_id = manifest.manifest_id
				WHERE manifest.tenant_id = job.tenant_id
					AND manifest.manifest_id = job.manifest_id
			)`,
		lease.ID, lease.Owner, lease.Attempt, availableAt.UTC(), errorClass, message, s.tenant, lease.ManifestID, lease.ProcessingVersion,
	)
	if err != nil {
		return fmt.Errorf("retrying raw parse job: %w", err)
	}
	return requireRawParseLeaseUpdate(result)
}

// FailRawParseJob moves an active current-head lease into durable terminal
// failure for operator inspection.
func (s *RawIngestStore) FailRawParseJob(
	ctx context.Context,
	lease rawderive.JobLease,
	errorClass string,
	message string,
) error {
	if s.tenant != "" && lease.Identity.TenantID != s.tenant {
		return rawderive.ErrLeaseLost
	}
	if err := validateRawParseLease(lease); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE raw_ingest_jobs AS job
		SET state = 'failed', lease_owner = '', lease_expires_at = NULL,
			last_error_class = $4, last_error = $5, updated_at = now()
		WHERE job.id = $1 AND job.lease_owner = $2 AND job.attempt_count = $3
			AND ($6 = '' OR (job.tenant_id = $6 AND job.manifest_id = $7 AND job.processing_version = $8))
			AND job.state = 'leased' AND job.lease_expires_at > now()
			AND EXISTS (
				SELECT 1
				FROM raw_manifests AS manifest
				JOIN raw_source_heads AS head
					ON head.tenant_id = manifest.tenant_id
					AND head.device_id = manifest.device_id
					AND head.provider = manifest.provider
					AND head.configured_root_id = manifest.configured_root_id
					AND head.source_key_sha256 = manifest.source_key_sha256
					AND head.manifest_id = manifest.manifest_id
				WHERE manifest.tenant_id = job.tenant_id
					AND manifest.manifest_id = job.manifest_id
			)`,
		lease.ID, lease.Owner, lease.Attempt, errorClass, message, s.tenant, lease.ManifestID, lease.ProcessingVersion,
	)
	if err != nil {
		return fmt.Errorf("failing raw parse job: %w", err)
	}
	return requireRawParseLeaseUpdate(result)
}

func validateRawParseLeaseRequest(owner string, limit int, duration time.Duration) error {
	if !rawderive.ValidLeaseOwner(owner) {
		return fmt.Errorf("%w: parse lease owner is invalid", rawsync.ErrInvalid)
	}
	if limit <= 0 || limit > rawderive.MaxClaimBatchSize {
		return fmt.Errorf("%w: parse claim limit must be between 1 and %d",
			rawsync.ErrInvalid, rawderive.MaxClaimBatchSize)
	}
	return validateRawParseLeaseDuration(duration)
}

func validateRawParseLeaseDuration(duration time.Duration) error {
	if duration <= 0 || duration.Microseconds() <= 0 {
		return fmt.Errorf("%w: lease duration must be positive", rawsync.ErrInvalid)
	}
	return nil
}

func validateRawParseLease(lease rawderive.JobLease) error {
	if lease.ID <= 0 || lease.Attempt <= 0 || strings.TrimSpace(lease.Owner) == "" {
		return fmt.Errorf("%w: parse job lease is invalid", rawsync.ErrInvalid)
	}
	return nil
}

func requireRawParseLeaseUpdate(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking raw parse job lease update: %w", err)
	}
	if affected != 1 {
		return rawderive.ErrLeaseLost
	}
	return nil
}

func (s *RawIngestStore) rawParseClaimStatement(owner string, limit int, leaseDuration time.Duration) (string, []any) {
	query := `
		WITH candidates AS (
			SELECT job.id
			FROM raw_ingest_jobs AS job
			JOIN raw_manifests AS manifest
				ON manifest.tenant_id = job.tenant_id
				AND manifest.manifest_id = job.manifest_id
			WHERE (
				(job.state IN ('ready', 'retrying') AND job.available_at <= now())
				OR (job.state = 'leased' AND job.lease_expires_at <= now())
			)
				AND job.stage = 'parse' AND job.projection_selected
 AND ($4 = '' OR job.tenant_id = $4)
 {{hosted}}
				AND EXISTS (
				SELECT 1
				FROM raw_source_heads AS head
				WHERE head.tenant_id = manifest.tenant_id
					AND head.device_id = manifest.device_id
					AND head.provider = manifest.provider
					AND head.configured_root_id = manifest.configured_root_id
					AND head.source_key_sha256 = manifest.source_key_sha256
					AND head.manifest_id = manifest.manifest_id
				)
			ORDER BY job.available_at, job.id
			LIMIT $1
			FOR UPDATE OF job SKIP LOCKED
		), claimed AS (
			UPDATE raw_ingest_jobs AS job
			SET state = 'leased', attempt_count = job.attempt_count + 1,
				lease_owner = $2,
				lease_expires_at = now() + ($3 * interval '1 microsecond'),
				last_error_class = '', last_error = '', updated_at = now()
			FROM candidates
			WHERE job.id = candidates.id AND ($4 = '' OR job.tenant_id = $4)
			RETURNING job.id, job.tenant_id, job.manifest_id,
				job.processing_version, job.projection_generation, job.attempt_count, job.lease_owner,
				job.lease_expires_at
		)
		SELECT claimed.id, claimed.tenant_id, manifest.device_id,
			claimed.manifest_id, claimed.processing_version, claimed.projection_generation,
			claimed.attempt_count, claimed.lease_owner, claimed.lease_expires_at
		FROM claimed
		JOIN raw_manifests AS manifest
			ON manifest.tenant_id = claimed.tenant_id
			AND manifest.manifest_id = claimed.manifest_id
		ORDER BY claimed.id`
	args := []any{limit, owner, leaseDuration.Microseconds(), s.tenant}
	predicate := ""
	if s.hostedVersion != "" {
		predicate = `AND job.processing_version=$5 AND job.projection_generation>0 AND EXISTS (SELECT 1 FROM raw_source_projections selected WHERE selected.tenant_id=job.tenant_id AND selected.selected_job_id=job.id AND selected.selected_manifest_id=job.manifest_id AND selected.processing_version=job.processing_version AND selected.projection_generation=job.projection_generation)`
		args = append(args, s.hostedVersion)
		query = strings.ReplaceAll(query, "($4 = '' OR job.tenant_id = $4)", "job.tenant_id = $4")
	}
	return strings.Replace(query, "{{hosted}}", predicate, 1), args
}
