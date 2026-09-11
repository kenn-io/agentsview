package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

type RawProjectionOptions struct {
	Tenant      string
	Content     ingest.ContentOptions
	RetryPolicy rawderive.RetryPolicy
	// Now supplies the observation clock for derived signal settling.
	Now func() time.Time
}
type RawProjectionStore struct {
	curationGuard func(context.Context, *sql.Tx, string, RawIdentity) error
	db            *sql.DB
	options       RawProjectionOptions
}

var _ rawderive.ProjectionSink = (*RawProjectionStore)(nil)

func NewRawProjectionStore(database *sql.DB, options RawProjectionOptions) (*RawProjectionStore, error) {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.RetryPolicy == (rawderive.RetryPolicy{}) {
		options.RetryPolicy = rawderive.RetryPolicy{Base: time.Second, Maximum: time.Minute, MaxAttempts: 5}
	}
	if err := options.RetryPolicy.Validate(); err != nil {
		return nil, err
	}
	if database == nil {
		return nil, fmt.Errorf("raw projection requires a database")
	}
	var schema string
	if err := database.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		return nil, err
	}
	if err := CheckHostedTenant(context.Background(), database, schema, options.Tenant); err != nil {
		return nil, err
	}
	if err := CheckSchemaCompat(context.Background(), database); err != nil {
		return nil, fmt.Errorf("hosted schema requires owner provisioning: %w", err)
	}
	rows, err := database.Query(`SELECT s.provenance_kind,s.raw_group_id,s.raw_content_revision,j.projection_generation,j.projection_selected,c.recency_state FROM sessions s CROSS JOIN raw_ingest_jobs j CROSS JOIN raw_content_revisions c LIMIT 0`)
	if err != nil {
		return nil, fmt.Errorf("raw projection schema requires owner provisioning: %w", err)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	var indexes bool
	if err := database.QueryRow(`SELECT to_regclass('raw_selected_job') IS NOT NULL AND to_regclass('raw_pending_signals') IS NOT NULL AND to_regclass('raw_hosted_jobs_due') IS NOT NULL AND to_regclass('raw_hosted_jobs_expired') IS NOT NULL`).Scan(&indexes); err != nil {
		return nil, err
	}
	if !indexes {
		return nil, fmt.Errorf("raw runtime indexes require owner provisioning")
	}
	return &RawProjectionStore{db: database, options: options}, nil
}

// SelectSourceGeneration is an explicit scheduler decision made before claiming
// a job. Equal selections are idempotent, including after successful completion.
// Version strings have no ordering; projection never invokes this operation.
func (s *RawProjectionStore) SelectSourceGeneration(ctx context.Context, m rawsync.CanonicalManifest, version string) (int64, error) {
	if err := s.validateManifest(m); err != nil {
		return 0, err
	}
	if err := validateRawIngestProcessingVersion(version); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.lockHead(ctx, tx, m); err != nil {
		return 0, err
	}
	generation, err := selectRawSourceGenerationTx(ctx, tx, m, version)
	if err != nil {
		return 0, err
	}
	return generation, tx.Commit()
}

// selectRawSourceGenerationTx requires the caller to own the current source-head
// lock and to have verified the canonical manifest. Acceptance uses the same
// transaction as its receipt; explicit rollout uses the public wrapper above.
func selectRawSourceGenerationTx(ctx context.Context, tx *sql.Tx, m rawsync.CanonicalManifest, version string) (int64, error) {
	var err error
	source := rawSourceID(m)
	var generation, previousJob int64
	var manifest, selected string
	err = tx.QueryRowContext(ctx, `SELECT projection_generation,selected_manifest_id,processing_version,selected_job_id FROM raw_source_projections WHERE tenant_id=$1 AND source_id=$2`, m.Identity.TenantID, source).Scan(&generation, &manifest, &selected, &previousJob)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if manifest == m.ManifestID && selected == version {
		return generation, nil
	}
	generation++
	var jobID int64
	err = tx.QueryRowContext(ctx, `INSERT INTO raw_ingest_jobs(tenant_id,manifest_id,stage,processing_version,projection_generation) VALUES($1,$2,'parse',$3,$4)
 ON CONFLICT(tenant_id,manifest_id,stage,processing_version) DO UPDATE SET projection_generation=EXCLUDED.projection_generation,projection_selected=true,attempt_count=0,state='ready',available_at=clock_timestamp(),lease_owner='',lease_expires_at=NULL RETURNING id`, m.Identity.TenantID, m.ManifestID, version, generation).Scan(&jobID)
	if err != nil {
		return 0, err
	}

	if previousJob != 0 && previousJob != jobID {
		_, err = tx.ExecContext(ctx, `UPDATE raw_ingest_jobs SET projection_selected=false,state=CASE WHEN state IN ('leased','ready','retrying') THEN 'superseded' ELSE state END,lease_owner='',lease_expires_at=NULL WHERE tenant_id=$1 AND id=$2`, m.Identity.TenantID, previousJob)
		if err != nil {
			return 0, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_source_projections(source_id,device_id,provider,configured_root_id,source_key_sha256,selected_manifest_id,processing_version,projection_generation,selected_job_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
 ON CONFLICT(source_id) DO UPDATE SET selected_manifest_id=EXCLUDED.selected_manifest_id,processing_version=EXCLUDED.processing_version,projection_generation=EXCLUDED.projection_generation,selected_job_id=EXCLUDED.selected_job_id`, source, m.Identity.DeviceID, string(m.Manifest.Provider), m.Manifest.ConfiguredRootID, rawIngestKeyDigest(m.Manifest.SourceKey), m.ManifestID, version, generation, jobID)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_projection_generations(source_id,generation,manifest_id,processing_version) VALUES($1,$2,$3,$4)`, source, generation, m.ManifestID, version)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_corpus_state(singleton,selection_revision) VALUES(1,1) ON CONFLICT(singleton) DO UPDATE SET selection_revision=raw_corpus_state.selection_revision+1`)
	if err != nil {
		return 0, err
	}
	return generation, nil
}
func (s *RawProjectionStore) validateManifest(m rawsync.CanonicalManifest) error {
	if m.Identity.TenantID != s.options.Tenant {
		return rawderive.ErrLeaseLost
	}
	return validateRawIngestCanonicalManifest(m)
}
func (s *RawProjectionStore) lockHead(ctx context.Context, tx *sql.Tx, m rawsync.CanonicalManifest) error {
	head, err := lockRawIngestHead(ctx, tx, m)
	if errors.Is(err, sql.ErrNoRows) {
		return rawderive.ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if head.ManifestID != m.ManifestID {
		return rawderive.ErrLeaseLost
	}
	var exact bool
	err = tx.QueryRowContext(ctx, `SELECT canonical_json=$3 FROM raw_manifests WHERE tenant_id=$1 AND manifest_id=$2`, s.options.Tenant, m.ManifestID, m.CanonicalJSON).Scan(&exact)
	if err != nil {
		return err
	}
	if !exact {
		return rawderive.ErrLeaseLost
	}
	return nil
}
func (s *RawProjectionStore) lockLease(ctx context.Context, tx *sql.Tx, m rawsync.CanonicalManifest, l rawderive.JobLease) error {
	if l.Identity != m.Identity || l.ManifestID != m.ManifestID || l.ProjectionGeneration <= 0 {
		return rawderive.ErrLeaseLost
	}
	var valid bool
	err := tx.QueryRowContext(ctx, `SELECT job.state='leased' AND job.lease_owner=$3 AND job.attempt_count=$4 AND job.lease_expires_at>clock_timestamp() AND job.manifest_id=$5 AND job.processing_version=$6 AND job.projection_generation=$7 AND selected.selected_job_id=job.id AND selected.projection_generation=job.projection_generation AND selected.selected_manifest_id=job.manifest_id AND selected.processing_version=job.processing_version
 FROM raw_ingest_jobs job JOIN raw_source_projections selected ON selected.tenant_id=job.tenant_id AND selected.source_id=$8 WHERE job.tenant_id=$1 AND job.id=$2 AND job.stage='parse' FOR UPDATE OF job`, s.options.Tenant, l.ID, l.Owner, l.Attempt, l.ManifestID, l.ProcessingVersion, l.ProjectionGeneration, rawSourceID(m)).Scan(&valid)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !valid {
		return rawderive.ErrLeaseLost
	}
	return err
}
func completeProjectionJob(ctx context.Context, tx *sql.Tx, l rawderive.JobLease, complete bool, policy rawderive.RetryPolicy) (rawderive.CommittedProjectionOutcome, error) {
	state := "complete"
	var outcome rawderive.CommittedProjectionOutcome
	var delay time.Duration
	diagnostic := ""
	if !complete {
		decision := policy.Decide(l.Attempt)
		delay = decision.Delay
		diagnostic = "projection:incomplete"
		state = "retrying"
		outcome = rawderive.ProjectionRetrying
		if decision.Failed {
			state = "failed"
			outcome = rawderive.ProjectionFailed
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE raw_ingest_jobs SET state=$8,lease_owner='',lease_expires_at=NULL,available_at=clock_timestamp()+($9 * interval '1 microsecond'),last_error_class=CASE WHEN $10='' THEN '' ELSE 'projection' END,last_error=$10,updated_at=clock_timestamp() WHERE tenant_id=$1 AND id=$2 AND manifest_id=$3 AND processing_version=$4 AND projection_generation=$5 AND lease_owner=$6 AND attempt_count=$7 AND state='leased' AND lease_expires_at>clock_timestamp()`, l.Identity.TenantID, l.ID, l.ManifestID, l.ProcessingVersion, l.ProjectionGeneration, l.Owner, l.Attempt, state, delay.Microseconds(), diagnostic)
	if err != nil {
		return 0, err
	}
	return outcome, requireRawParseLeaseUpdate(result)
}
