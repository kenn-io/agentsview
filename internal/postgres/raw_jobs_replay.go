package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"go.kenn.io/agentsview/internal/rawsync"
)

// rawJobReplayBatch bounds source-head locks to keep replay transactions short.
const rawJobReplayBatch = 64

// RawJobRetryFilter requires exactly one selector for a replay batch.
type RawJobRetryFilter struct {
	ErrorClass, ManifestID string
	All                    bool
}

// RawJobReplayResult counts failures replayed or retired as obsolete.
type RawJobReplayResult struct{ Replayed, Superseded int64 }

// RetryFailedRawJobs replays one bounded batch against durable selection, never
// the running binary's processing version. Head locks use rollout's source order.
func (s *RawProjectionStore) RetryFailedRawJobs(ctx context.Context, filter RawJobRetryFilter) (RawJobReplayResult, error) {
	filters := 0
	for _, set := range []bool{filter.All, filter.ErrorClass != "", filter.ManifestID != ""} {
		if set {
			filters++
		}
	}
	if filters != 1 {
		return RawJobReplayResult{}, rawsync.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RawJobReplayResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT j.id,j.lease_epoch,h.device_id,h.provider,h.configured_root_id,h.source_key_sha256,coalesce(h.manifest_id,'')
 FROM raw_ingest_jobs j JOIN raw_manifests m ON m.tenant_id=j.tenant_id AND m.manifest_id=j.manifest_id
 JOIN raw_source_heads h ON h.tenant_id=m.tenant_id AND h.device_id=m.device_id AND h.provider=m.provider AND h.configured_root_id=m.configured_root_id AND h.source_key_sha256=m.source_key_sha256
 WHERE j.tenant_id=$1 AND j.stage='parse' AND j.state='failed' AND ($2='' OR j.last_error_class=$2) AND ($3='' OR j.manifest_id=$3)
 ORDER BY h.device_id,h.provider,h.configured_root_id,h.source_key_sha256,j.id LIMIT $4 FOR UPDATE OF h`, s.options.Tenant, filter.ErrorClass, filter.ManifestID, rawJobReplayBatch)
	if err != nil {
		return RawJobReplayResult{}, err
	}
	defer rows.Close()
	type candidate struct {
		id, epoch                            int64
		device, provider, root, digest, head string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.epoch, &c.device, &c.provider, &c.root, &c.digest, &c.head); err != nil {
			return RawJobReplayResult{}, err
		}
		candidates = append(candidates, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return RawJobReplayResult{}, err
	}
	var result RawJobReplayResult
	for _, c := range candidates {
		var selected, generation int64
		var manifest, version string
		err = tx.QueryRowContext(ctx, `SELECT selected_job_id,selected_manifest_id,processing_version,projection_generation FROM raw_source_projections WHERE tenant_id=$1 AND device_id=$2 AND provider=$3 AND configured_root_id=$4 AND source_key_sha256=$5 FOR UPDATE`, s.options.Tenant, c.device, c.provider, c.root, c.digest).Scan(&selected, &manifest, &version, &generation)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return RawJobReplayResult{}, err
		}
		var state string
		err = tx.QueryRowContext(ctx, `WITH decision AS (
 SELECT id, projection_selected AND id=$4 AND manifest_id=$5 AND manifest_id=$6 AND processing_version=$7 AND projection_generation=$8 AS replay
 FROM raw_ingest_jobs WHERE tenant_id=$1 AND id=$2)
 UPDATE raw_ingest_jobs j SET state=CASE WHEN d.replay THEN 'ready' ELSE 'superseded' END,
 attempt_count=CASE WHEN d.replay THEN 0 ELSE j.attempt_count END,
 available_at=CASE WHEN d.replay THEN clock_timestamp() ELSE j.available_at END,
 lease_epoch=j.lease_epoch+CASE WHEN d.replay THEN 1 ELSE 0 END,
 failed_at=CASE WHEN d.replay THEN NULL ELSE j.failed_at END,
 lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
 FROM decision d WHERE j.tenant_id=$1 AND j.id=d.id AND j.lease_epoch=$3 AND j.state='failed' RETURNING j.state`, s.options.Tenant, c.id, c.epoch, selected, manifest, c.head, version, generation).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return RawJobReplayResult{}, err
		}
		if state == "ready" {
			result.Replayed++
		} else {
			result.Superseded++
		}
	}
	return result, tx.Commit()
}

// RawJobCount groups parse jobs without exposing raw error messages.
type RawJobCount struct {
	State, ErrorClass string
	Count             int64
	OldestFailedAt    *time.Time
}

// ListRawJobs reports tenant-scoped counts and dead-letter ages in stable order.
func (s *RawProjectionStore) ListRawJobs(ctx context.Context) ([]RawJobCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state,last_error_class,count(*),min(failed_at) FROM raw_ingest_jobs WHERE tenant_id=$1 AND stage='parse' GROUP BY state,last_error_class ORDER BY state,last_error_class`, s.options.Tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make([]RawJobCount, 0)
	for rows.Next() {
		var row RawJobCount
		if err = rows.Scan(&row.State, &row.ErrorClass, &row.Count, &row.OldestFailedAt); err != nil {
			return nil, err
		}
		if stamp := row.OldestFailedAt; stamp != nil {
			*stamp = stamp.UTC()
		}
		counts = append(counts, row)
	}
	return counts, rows.Err()
}
