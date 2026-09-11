package postgres

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/signals"
)

const pendingRawSignalsSQL = `SELECT id,raw_group_id FROM sessions WHERE tenant_id=$1 AND provenance_kind='raw' AND signals_pending_since IS NOT NULL AND ended_at<=$2 ORDER BY ended_at,id LIMIT $3`

// SettlePendingSignals examines only a capped indexed set of due, currently
// physical raw revisions. Group locks serialize removal, replacement and
// curation; it never acquires a source-head lock after a group lock.
func (s *RawProjectionStore) SettlePendingSignals(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 256 {
		return 0, rawsync.ErrInvalid
	}
	now := s.options.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, pendingRawSignalsSQL, s.options.Tenant, now.Add(-signals.RecencyWindow), limit)
	if err != nil {
		return 0, err
	}
	type candidate struct{ id, group string }
	var candidates []candidate
	var groups []string
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.id, &c.group); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, c)
		groups = append(groups, c.group)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	slices.Sort(groups)
	groups = slices.Compact(groups)
	for _, group := range groups {
		var locked string
		if err = tx.QueryRowContext(ctx, `SELECT group_id FROM raw_session_groups WHERE group_id=$1 FOR UPDATE`, group).Scan(&locked); err != nil {
			return 0, err
		}
	}
	changed := 0
	for _, c := range candidates {
		var eligible bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s WHERE s.id=$1 AND s.raw_group_id=$2 AND s.provenance_kind='raw' AND s.signals_pending_since IS NOT NULL AND s.ended_at<=$3 AND EXISTS(SELECT 1 FROM raw_session_branches b WHERE b.group_id=$2 AND b.session_id=s.id AND b.active AND NOT COALESCE((SELECT value='true'::jsonb FROM raw_curation c WHERE c.group_id=b.group_id AND c.branch_id=b.branch_id AND c.field='excluded'),(SELECT value='true'::jsonb FROM raw_curation c WHERE c.group_id=b.group_id AND c.branch_id='' AND c.field='excluded'),false)))`, c.id, c.group, now.Add(-signals.RecencyWindow)).Scan(&eligible)
		if err != nil {
			return 0, err
		}
		if !eligible {
			continue
		}
		p, err := loadRawPayload(ctx, tx, c.id)
		if err != nil {
			return 0, err
		}
		updated, err := publishRawRecency(ctx, tx, c.id, ingest.RefreshSignalRecencyAt(p.Session, p.Messages, p.Signals, now))
		if err != nil {
			return 0, err
		}
		if updated {
			changed++
		}
	}
	if changed > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE raw_corpus_state SET corpus_revision=corpus_revision+1 WHERE singleton=1`); err != nil {
			return 0, err
		}
	}
	return changed, tx.Commit()
}

type RawRolloutBatch struct {
	Selected int
	Done     bool
}

const rawRolloutHeadsSQL = `SELECT h.device_id,h.provider,h.configured_root_id,h.source_key_sha256,m.manifest_id,m.canonical_json FROM raw_source_heads h JOIN raw_manifests m ON m.tenant_id=h.tenant_id AND m.manifest_id=h.manifest_id WHERE h.tenant_id=$1 AND (h.device_id,h.provider,h.configured_root_id,h.source_key_sha256)>($2,$3,$4,$5) ORDER BY h.device_id,h.provider,h.configured_root_id,h.source_key_sha256 LIMIT $6 FOR UPDATE OF h`

// ScheduleCurrentHeads is an explicit, resumable version rollout, never an idle
// worker poll. Each call commits at most limit heads and its keyset checkpoint.
// Reusing a run ID requires the same version; equal selections stay completed.
func (s *RawProjectionStore) ScheduleCurrentHeads(ctx context.Context, runID, version string, limit int) (RawRolloutBatch, error) {
	if strings.TrimSpace(runID) == "" || len(runID) > 128 || limit < 1 || limit > 256 {
		return RawRolloutBatch{}, rawsync.ErrInvalid
	}
	if err := validateRawIngestProcessingVersion(version); err != nil {
		return RawRolloutBatch{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RawRolloutBatch{}, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO raw_projection_rollouts(run_id,processing_version) VALUES($1,$2) ON CONFLICT(run_id) DO NOTHING`, runID, version)
	if err != nil {
		return RawRolloutBatch{}, err
	}
	var saved, device, provider, root, digest string
	var done bool
	err = tx.QueryRowContext(ctx, `SELECT processing_version,device_id,provider,configured_root_id,source_key_sha256,complete FROM raw_projection_rollouts WHERE run_id=$1 FOR UPDATE`, runID).Scan(&saved, &device, &provider, &root, &digest, &done)
	if err != nil {
		return RawRolloutBatch{}, err
	}
	if saved != version {
		return RawRolloutBatch{}, rawsync.ErrConflict
	}
	if done {
		return RawRolloutBatch{Done: true}, nil
	}
	rows, err := tx.QueryContext(ctx, rawRolloutHeadsSQL, s.options.Tenant, device, provider, root, digest, limit)
	if err != nil {
		return RawRolloutBatch{}, err
	}
	type head struct {
		device, provider, root, digest, id string
		data                               []byte
	}
	var heads []head
	for rows.Next() {
		var h head
		if err = rows.Scan(&h.device, &h.provider, &h.root, &h.digest, &h.id, &h.data); err != nil {
			rows.Close()
			return RawRolloutBatch{}, err
		}
		heads = append(heads, h)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return RawRolloutBatch{}, err
	}
	for _, h := range heads {
		m, err := rawsync.ParseCanonicalManifest(rawsync.AuthIdentity{TenantID: s.options.Tenant, DeviceID: h.device}, h.id, h.data, rawsync.DefaultManifestLimits())
		if err != nil {
			return RawRolloutBatch{}, err
		}
		if _, err = selectRawSourceGenerationTx(ctx, tx, m, version); err != nil {
			return RawRolloutBatch{}, err
		}
		device, provider, root, digest = h.device, h.provider, h.root, h.digest
	}
	done = len(heads) < limit
	_, err = tx.ExecContext(ctx, `UPDATE raw_projection_rollouts SET device_id=$2,provider=$3,configured_root_id=$4,source_key_sha256=$5,complete=$6 WHERE run_id=$1`, runID, device, provider, root, digest, done)
	if err != nil {
		return RawRolloutBatch{}, err
	}
	return RawRolloutBatch{Selected: len(heads), Done: done}, tx.Commit()
}

var ErrHostedProjectionOwned = errors.New("pg push cannot mutate a hosted-owned projection")

// RejectHostedPush guards the database itself, even when pg push used an older
// target configuration that did not declare raw_derivation.
func RejectHostedPush(ctx context.Context, database *sql.DB) error {
	var binding sql.NullString
	if err := database.QueryRowContext(ctx, `SELECT to_regclass('hosted_tenant_binding')::text`).Scan(&binding); err != nil {
		return err
	}
	if binding.Valid {
		return ErrHostedProjectionOwned
	}
	return nil
}
