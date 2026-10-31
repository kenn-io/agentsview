package postgres

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"math"

	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

// rawReconcileHeadBatchSize keeps source-head locks short, matching rollout's
// default batch rather than tying maintenance to the tenant's total size.
const rawReconcileHeadBatchSize = 64

// RawReconcileBatch reports one bounded selection pass. Busy means another
// maintenance invocation owns this tenant; Wrapped marks the end of a sweep.
type RawReconcileBatch struct {
	Scanned, Found, Repaired, Unrecoverable int
	Wrapped, Busy                           bool
}

// ReconcileRawSources checks one keyset batch. Repair never selects the running
// binary's version. A dry run reports damage without changing selection or the
// repair cursor; custody audit is available only inside the serving process.
func (s *RawProjectionStore) ReconcileRawSources(ctx context.Context, repair bool) (RawReconcileBatch, error) {
	var result RawReconcileBatch
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	locked, err := s.lockRawMaintenanceTx(ctx, tx)
	if err != nil {
		return result, err
	}
	if !locked {
		result.Busy = true
		return result, nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_maintenance_state(check_name) VALUES('reconcile') ON CONFLICT DO NOTHING`)
	if err != nil {
		return result, err
	}
	var encoded []byte
	err = tx.QueryRowContext(ctx, `SELECT cursor FROM raw_maintenance_state WHERE tenant_id=$1 AND check_name='reconcile' FOR UPDATE`, s.options.Tenant).Scan(&encoded)
	if err != nil {
		return result, err
	}
	cursor := []string{"", "", "", ""}
	if err = json.Unmarshal(encoded, &cursor); err != nil {
		return result, err
	}
	if len(cursor) != 4 {
		return result, rawsync.ErrInvalid
	}
	rows, err := tx.QueryContext(ctx, rawRolloutHeadsSQL, s.options.Tenant, cursor[0], cursor[1], cursor[2], cursor[3], rawReconcileHeadBatchSize)
	if err != nil {
		return result, err
	}
	type head struct {
		key  [4]string
		id   string
		data []byte
	}
	var heads []head
	for rows.Next() {
		var h head
		if err = rows.Scan(&h.key[0], &h.key[1], &h.key[2], &h.key[3], &h.id, &h.data); err != nil {
			rows.Close()
			return result, err
		}
		heads = append(heads, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for _, h := range heads {
		m, err := rawsync.ParseCanonicalManifest(rawsync.AuthIdentity{TenantID: s.options.Tenant, DeviceID: h.key[0]}, h.id, h.data, rawsync.DefaultManifestLimits())
		if err != nil {
			return result, err
		}
		found, repaired, unrecoverable, err := s.recoverRawSourceTx(ctx, tx, m, repair)
		if err != nil {
			return result, err
		}
		result.Scanned++
		if found {
			result.Found++
		}
		if repaired {
			result.Repaired++
		}
		if unrecoverable {
			result.Unrecoverable++
		}
		cursor = h.key[:]
	}
	result.Wrapped = len(heads) < rawReconcileHeadBatchSize
	if result.Wrapped {
		cursor = []string{"", "", "", ""}
	}
	if !repair {
		return result, nil
	}
	encoded, err = json.Marshal(cursor)
	if err != nil {
		return result, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE raw_maintenance_state SET cursor=$2,last_run_at=clock_timestamp(),found=$3,repaired=$4,unrecoverable=$5 WHERE tenant_id=$1 AND check_name='reconcile'`, s.options.Tenant, encoded, result.Found, result.Repaired, result.Unrecoverable)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

// lockRawMaintenanceTx is shared by CLI repair and in-process audit. Scope the
// lock to both schema and tenant, including installations in the same database.
func (s *RawProjectionStore) lockRawMaintenanceTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var locked bool
	err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('raw-reconcile:'||current_schema()||':'||$1,0))`, s.options.Tenant).Scan(&locked)
	return locked, err
}

// recoverRawSourceTx rechecks observed custody while holding the head lock.
// A projection is authoritative version evidence; without one, only a unique
// version at the head's highest surviving job generation is sufficient.
func (s *RawProjectionStore) recoverRawSourceTx(ctx context.Context, tx *sql.Tx, m rawsync.CanonicalManifest, repair bool) (found, repaired, unrecoverable bool, err error) {
	if err = s.lockHead(ctx, tx, m); errors.Is(err, rawderive.ErrLeaseLost) {
		return false, false, false, nil
	}
	if err != nil {
		return
	}
	source := rawSourceID(m)
	var version string
	var agrees bool
	err = tx.QueryRowContext(ctx, `SELECT p.processing_version,
 p.projection_generation>0 AND p.selected_manifest_id=$3 AND p.device_id=$4 AND p.provider=$5 AND p.configured_root_id=$6 AND p.source_key_sha256=$7 AND EXISTS(SELECT 1 FROM raw_ingest_jobs j WHERE j.tenant_id=p.tenant_id AND j.id=p.selected_job_id AND j.manifest_id=p.selected_manifest_id AND j.stage='parse' AND j.processing_version=p.processing_version AND j.projection_generation=p.projection_generation AND j.projection_selected)
 FROM raw_source_projections p WHERE p.tenant_id=$1 AND p.source_id=$2 FOR UPDATE`, s.options.Tenant, source, m.ManifestID, m.Identity.DeviceID, string(m.Manifest.Provider), m.Manifest.ConfiguredRootID, rawIngestKeyDigest(m.Manifest.SourceKey)).Scan(&version, &agrees)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return
	}
	if agrees {
		return false, false, false, nil
	}
	found = true
	if errors.Is(err, sql.ErrNoRows) {
		var versions int
		err = tx.QueryRowContext(ctx, `SELECT count(DISTINCT processing_version),COALESCE(min(processing_version),'') FROM raw_ingest_jobs WHERE tenant_id=$1 AND manifest_id=$2 AND stage='parse' AND projection_generation=(SELECT max(projection_generation) FROM raw_ingest_jobs WHERE tenant_id=$1 AND manifest_id=$2 AND stage='parse')`, s.options.Tenant, m.ManifestID).Scan(&versions, &version)
		if err != nil {
			return
		}
		if versions != 1 {
			return true, false, true, nil
		}
	}
	if validateRawIngestProcessingVersion(version) != nil {
		return true, false, true, nil
	}
	// Include every surviving fence, even if the projection row itself was
	// restored from an older backup. Never reuse a published generation.
	var generation int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(max(g),0) FROM (
 SELECT projection_generation g FROM raw_source_projections WHERE tenant_id=$1 AND source_id=$2
 UNION ALL SELECT generation FROM raw_projection_generations WHERE tenant_id=$1 AND source_id=$2
 UNION ALL SELECT j.projection_generation FROM raw_ingest_jobs j JOIN raw_manifests m ON m.tenant_id=j.tenant_id AND m.manifest_id=j.manifest_id WHERE m.tenant_id=$1 AND m.device_id=$3 AND m.provider=$4 AND m.configured_root_id=$5 AND m.source_key_sha256=$6
 UNION ALL SELECT projection_generation FROM raw_session_branches WHERE tenant_id=$1 AND source_id=$2
 UNION ALL SELECT projection_generation FROM session_sources WHERE tenant_id=$1 AND source_id=$2
 UNION ALL SELECT c.projection_generation FROM raw_source_contributions c JOIN raw_session_branches b ON b.tenant_id=c.tenant_id AND b.branch_id=c.branch_id WHERE b.tenant_id=$1 AND b.source_id=$2
 ) fences`, s.options.Tenant, source, m.Identity.DeviceID, string(m.Manifest.Provider), m.Manifest.ConfiguredRootID, rawIngestKeyDigest(m.Manifest.SourceKey)).Scan(&generation)
	if err != nil {
		return
	}
	if generation == math.MaxInt64 {
		return true, false, true, nil
	}
	if !repair {
		return true, false, false, nil
	}
	generation++
	var job int64
	err = tx.QueryRowContext(ctx, `INSERT INTO raw_ingest_jobs(tenant_id,manifest_id,stage,processing_version,projection_generation) VALUES($1,$2,'parse',$3,$4)
 ON CONFLICT(tenant_id,manifest_id,stage,processing_version) DO UPDATE SET
 projection_generation=EXCLUDED.projection_generation,projection_selected=true,
 state=CASE WHEN raw_ingest_jobs.state='failed' THEN 'failed' ELSE 'ready' END,
 attempt_count=CASE WHEN raw_ingest_jobs.state='failed' THEN raw_ingest_jobs.attempt_count ELSE 0 END,
 lease_epoch=raw_ingest_jobs.lease_epoch+CASE WHEN raw_ingest_jobs.state='failed' THEN 0 ELSE 1 END,
 failed_at=CASE WHEN raw_ingest_jobs.state='failed' THEN raw_ingest_jobs.failed_at END,
 available_at=clock_timestamp(),lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
 RETURNING id`, s.options.Tenant, m.ManifestID, version, generation).Scan(&job)
	if err != nil {
		return
	}
	_, err = tx.ExecContext(ctx, `UPDATE raw_ingest_jobs j SET projection_selected=false,state=CASE WHEN j.state IN('ready','retrying','leased') THEN 'superseded' ELSE j.state END,lease_owner='',lease_expires_at=NULL FROM raw_manifests m WHERE j.tenant_id=$1 AND j.id<>$2 AND j.projection_selected AND m.tenant_id=j.tenant_id AND m.manifest_id=j.manifest_id AND m.device_id=$3 AND m.provider=$4 AND m.configured_root_id=$5 AND m.source_key_sha256=$6`, s.options.Tenant, job, m.Identity.DeviceID, string(m.Manifest.Provider), m.Manifest.ConfiguredRootID, rawIngestKeyDigest(m.Manifest.SourceKey))
	if err != nil {
		return
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_source_projections(source_id,device_id,provider,configured_root_id,source_key_sha256,selected_manifest_id,processing_version,projection_generation,selected_job_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
 ON CONFLICT(source_id) DO UPDATE SET device_id=EXCLUDED.device_id,provider=EXCLUDED.provider,configured_root_id=EXCLUDED.configured_root_id,source_key_sha256=EXCLUDED.source_key_sha256,selected_manifest_id=EXCLUDED.selected_manifest_id,processing_version=EXCLUDED.processing_version,projection_generation=EXCLUDED.projection_generation,selected_job_id=EXCLUDED.selected_job_id`, source, m.Identity.DeviceID, string(m.Manifest.Provider), m.Manifest.ConfiguredRootID, rawIngestKeyDigest(m.Manifest.SourceKey), m.ManifestID, version, generation, job)
	if err != nil {
		return
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_projection_generations(source_id,generation,manifest_id,processing_version) VALUES($1,$2,$3,$4)`, source, generation, m.ManifestID, version)
	if err != nil {
		return
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_corpus_state(singleton,selection_revision) VALUES(1,1) ON CONFLICT(singleton) DO UPDATE SET selection_revision=raw_corpus_state.selection_revision+1`)
	return true, err == nil, false, err
}
