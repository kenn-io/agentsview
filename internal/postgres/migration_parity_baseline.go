package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

const parityInventoryPageSize = 128

type parityCapturedSource struct {
	source               rawderive.ParitySource
	provider             string
	rootID               string
	headReceipt          string
	sourceKey            string
	keySHA256            string
	hasProjection        bool
	selectedManifest     string
	processingVersion    string
	projectionGeneration int64
	selectedJobID        int64
	successfulManifest   sql.NullString
	lastAttemptManifest  sql.NullString
	membershipComplete   bool
	diagnostics          string
	manifestRows         []rawderive.ParityHistoryEntry
	partialCode          string
	runtimeDependency    rawderive.ParityDigest
	cursor               string
}

type parityCapturedMember struct {
	key                 rawderive.ParityMemberKey
	physicalID          string
	mappingState        string
	fingerprint         *rawderive.ParityFingerprint
	physical            *rawderive.ParityDigest
	verdict             rawderive.ParityVerdict
	canRead             bool
	links               []rawderive.ParityLink
	overlay             rawderive.ParityOverlay
	relationshipLookups [][]byte
	relationshipValues  [][]byte
	relationshipBytes   int64
}

func parityHashField(h hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = h.Write(value)
}

func parityDigestHash(h hash.Hash) rawderive.ParityDigest {
	var digest rawderive.ParityDigest
	copy(digest[:], h.Sum(nil))
	return digest
}

func paritySourceDependency(source parityCapturedSource) rawderive.ParityDigest {
	h := sha256.New()
	_, _ = h.Write([]byte("agentsview-parity-source-dependency-v1"))
	for _, value := range []string{
		source.source.ID, source.source.Identity.TenantID, source.source.Identity.DeviceID,
		source.provider, source.rootID, source.source.HeadManifestID, source.headReceipt, source.sourceKey, source.keySHA256,
		source.selectedManifest, source.processingVersion, source.successfulManifest.String,
		source.lastAttemptManifest.String, source.diagnostics,
	} {
		parityHashField(h, []byte(value))
	}
	var generation [8]byte
	binary.BigEndian.PutUint64(generation[:], uint64(source.source.HeadGeneration))
	parityHashField(h, generation[:])
	binary.BigEndian.PutUint64(generation[:], uint64(source.projectionGeneration))
	parityHashField(h, generation[:])
	binary.BigEndian.PutUint64(generation[:], uint64(source.selectedJobID))
	parityHashField(h, generation[:])
	parityHashField(h, source.runtimeDependency[:])
	for _, value := range []bool{source.successfulManifest.Valid, source.lastAttemptManifest.Valid, source.membershipComplete} {
		if value {
			parityHashField(h, []byte{1})
		} else {
			parityHashField(h, []byte{0})
		}
	}
	for _, entry := range source.manifestRows {
		key := encodeParityHistoryEntry(entry)
		parityHashField(h, key)
	}
	parityHashField(h, []byte(source.partialCode))
	return parityDigestHash(h)
}

func readParityRuntimeDependency(ctx context.Context, tx *sql.Tx, source parityCapturedSource) (rawderive.ParityDigest, string, error) {
	rawRows, rawBytes, err := parityRawBindingSize(ctx, tx, source)
	if err != nil {
		return rawderive.ParityDigest{}, "", err
	}
	if rawRows > parityPhysicalMaxRows || rawBytes > parityPhysicalMaxBytes {
		return rawderive.ParityDigest{}, "limit_exceeded", nil
	}
	rows, bytes, err := parityPhysicalBindingSize(ctx, tx, "", source.source.ID, false)
	if err != nil {
		return rawderive.ParityDigest{}, "", err
	}
	if rawRows+rows > parityPhysicalMaxRows || rawBytes+bytes > parityPhysicalMaxBytes {
		return rawderive.ParityDigest{}, "limit_exceeded", nil
	}
	h := sha256.New()
	_, _ = h.Write([]byte("agentsview-parity-runtime-dependency-v1"))
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT jsonb_build_array(source_id)::text AS parity_page_key,source_id,device_id,provider,configured_root_id,source_key_sha256,selected_manifest_id,processing_version,projection_generation::text,selected_job_id::text,successful_manifest_id,last_attempt_manifest_id,membership_complete::text,diagnostics FROM raw_source_projections WHERE source_id=$1 AND jsonb_build_array(source_id)::text>@after ORDER BY jsonb_build_array(source_id)::text LIMIT @limit`, []any{source.source.ID}},
		{`SELECT jsonb_build_array(generation)::text AS parity_page_key,generation::text,manifest_id,processing_version FROM raw_projection_generations WHERE source_id=$1 AND jsonb_build_array(generation)::text>@after ORDER BY jsonb_build_array(generation)::text LIMIT @limit`, []any{source.source.ID}},
		{`SELECT jsonb_build_array(group_id)::text AS parity_page_key,group_id,provider,logical_key,base_alias FROM raw_session_groups WHERE group_id IN (SELECT group_id FROM raw_session_branches WHERE source_id=$1) AND jsonb_build_array(group_id)::text>@after ORDER BY jsonb_build_array(group_id)::text LIMIT @limit`, []any{source.source.ID}},
		{`SELECT jsonb_build_array(branch_id)::text AS parity_page_key,branch_id,group_id,source_id,session_id,physical_session_id,manifest_id,content_revision,processing_version,projection_generation::text FROM session_sources WHERE source_id=$1 AND jsonb_build_array(branch_id)::text>@after ORDER BY jsonb_build_array(branch_id)::text LIMIT @limit`, []any{source.source.ID}},
		{`SELECT jsonb_build_array(l.branch_id,l.kind,l.ordinal,l.call_index,l.event_index)::text AS parity_page_key,l.branch_id,l.kind,l.ordinal::text,l.call_index::text,l.event_index::text,l.target_alias FROM raw_session_links l JOIN raw_session_branches b ON b.branch_id=l.branch_id WHERE b.source_id=$1 AND jsonb_build_array(l.branch_id,l.kind,l.ordinal,l.call_index,l.event_index)::text>@after ORDER BY jsonb_build_array(l.branch_id,l.kind,l.ordinal,l.call_index,l.event_index)::text LIMIT @limit`, []any{source.source.ID}},
		{`SELECT jsonb_build_array(a.alias_id,a.group_id)::text AS parity_page_key,a.alias_id,a.group_id,a.anchor_branch FROM raw_session_public_aliases a WHERE a.group_id IN (SELECT group_id FROM raw_session_branches WHERE source_id=$1) AND jsonb_build_array(a.alias_id,a.group_id)::text>@after ORDER BY jsonb_build_array(a.alias_id,a.group_id)::text LIMIT @limit`, []any{source.source.ID}},
		{`SELECT jsonb_build_array(c.group_id,c.branch_id,c.field)::text AS parity_page_key,c.group_id,c.branch_id,c.field,c.value::text FROM raw_curation c WHERE c.group_id IN (SELECT group_id FROM raw_session_branches WHERE source_id=$1) AND jsonb_build_array(c.group_id,c.branch_id,c.field)::text>@after ORDER BY jsonb_build_array(c.group_id,c.branch_id,c.field)::text LIMIT @limit`, []any{source.source.ID}},
		{`SELECT jsonb_build_array(p.group_id,p.branch_id,p.message_key)::text AS parity_page_key,p.group_id,p.branch_id,p.message_key,p.ordinal::text,p.content_revision,p.pinned::text,p.note FROM raw_pins p WHERE p.group_id IN (SELECT group_id FROM raw_session_branches WHERE source_id=$1) AND jsonb_build_array(p.group_id,p.branch_id,p.message_key)::text>@after ORDER BY jsonb_build_array(p.group_id,p.branch_id,p.message_key)::text LIMIT @limit`, []any{source.source.ID}},
		{`SELECT jsonb_build_array(h.source_key_sha256)::text AS parity_page_key,h.device_id,h.provider,h.configured_root_id,h.source_key,h.source_key_sha256,h.manifest_id,h.receipt,h.generation::text FROM raw_source_heads h WHERE h.device_id=$1 AND h.provider=$2 AND h.configured_root_id=$3 AND h.source_key_sha256=$4 AND jsonb_build_array(h.source_key_sha256)::text>@after ORDER BY jsonb_build_array(h.source_key_sha256)::text LIMIT @limit`, []any{source.source.Identity.DeviceID, source.provider, source.rootID, source.keySHA256}},
		{`SELECT jsonb_build_array(m.manifest_id)::text AS parity_page_key,m.manifest_id,m.device_id,m.provider,m.configured_root_id,m.source_key,m.source_key_sha256,m.capture_id,m.parent_receipt,m.receipt,m.generation::text,m.kind,m.canonical_json FROM raw_manifests m WHERE m.device_id=$1 AND m.provider=$2 AND m.configured_root_id=$3 AND m.source_key_sha256=$4 AND jsonb_build_array(m.manifest_id)::text>@after ORDER BY jsonb_build_array(m.manifest_id)::text LIMIT @limit`, []any{source.source.Identity.DeviceID, source.provider, source.rootID, source.keySHA256}},
		{`SELECT jsonb_build_array(e.manifest_id,e.entry_index)::text AS parity_page_key,e.manifest_id,e.entry_index::text,e.path,e.path_sha256,e.entry_type,e.size_bytes::text FROM raw_manifest_entries e JOIN raw_manifests m USING(manifest_id) WHERE m.device_id=$1 AND m.provider=$2 AND m.configured_root_id=$3 AND m.source_key_sha256=$4 AND jsonb_build_array(e.manifest_id,e.entry_index)::text>@after ORDER BY jsonb_build_array(e.manifest_id,e.entry_index)::text LIMIT @limit`, []any{source.source.Identity.DeviceID, source.provider, source.rootID, source.keySHA256}},
		{`SELECT jsonb_build_array(o.manifest_id,o.entry_index,o.object_index)::text AS parity_page_key,o.manifest_id,o.entry_index::text,o.object_index::text,o.sha256,o.size_bytes::text FROM raw_manifest_objects o JOIN raw_manifests m USING(manifest_id) WHERE m.device_id=$1 AND m.provider=$2 AND m.configured_root_id=$3 AND m.source_key_sha256=$4 AND jsonb_build_array(o.manifest_id,o.entry_index,o.object_index)::text>@after ORDER BY jsonb_build_array(o.manifest_id,o.entry_index,o.object_index)::text LIMIT @limit`, []any{source.source.Identity.DeviceID, source.provider, source.rootID, source.keySHA256}},
		{`SELECT jsonb_build_array(o.sha256,o.size_bytes)::text AS parity_page_key,o.sha256,o.size_bytes::text FROM raw_objects o WHERE EXISTS(SELECT 1 FROM raw_manifest_objects mo JOIN raw_manifests m USING(manifest_id) WHERE m.device_id=$1 AND m.provider=$2 AND m.configured_root_id=$3 AND m.source_key_sha256=$4 AND mo.sha256=o.sha256 AND mo.size_bytes=o.size_bytes) AND jsonb_build_array(o.sha256,o.size_bytes)::text>@after ORDER BY jsonb_build_array(o.sha256,o.size_bytes)::text LIMIT @limit`, []any{source.source.Identity.DeviceID, source.provider, source.rootID, source.keySHA256}},
	}
	if err := hashParityRawPayloadRows(ctx, tx, "", source.source.ID, h); err != nil {
		if errors.Is(err, errParityPhysicalLimit) {
			return rawderive.ParityDigest{}, "limit_exceeded", nil
		}
		return rawderive.ParityDigest{}, "", err
	}
	for _, query := range queries {
		if err := hashParityTextPages(ctx, tx, query.sql, query.args, h); err != nil {
			if errors.Is(err, errParityPhysicalLimit) {
				return rawderive.ParityDigest{}, "limit_exceeded", nil
			}
			return rawderive.ParityDigest{}, "", err
		}
	}
	return parityDigestHash(h), "", nil
}

// parityRawBindingSize rejects an oversized source before any canonical JSON
// or object row crosses the database driver boundary. It is keyed by the
// immutable head coordinate, so an unprojected source receives the same bound
// as a projected source.
func parityRawBindingSize(ctx context.Context, q hostedQuerier, source parityCapturedSource) (int64, int64, error) {
	var rows, bytes int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE(sum(n),0),COALESCE(sum(bytes),0) FROM (
	 SELECT count(*) n,COALESCE(sum(octet_length(h.device_id)+octet_length(h.provider)+octet_length(h.configured_root_id)+octet_length(h.source_key)+octet_length(h.source_key_sha256)+COALESCE(octet_length(h.manifest_id),0)+COALESCE(octet_length(h.receipt),0)+octet_length(h.generation::text)),0) bytes FROM raw_source_heads h WHERE h.device_id=$1 AND h.provider=$2 AND h.configured_root_id=$3 AND h.source_key_sha256=$4
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(m.manifest_id)+octet_length(m.device_id)+octet_length(m.provider)+octet_length(m.configured_root_id)+octet_length(m.source_key)+octet_length(m.source_key_sha256)+octet_length(m.capture_id)+octet_length(m.parent_receipt)+octet_length(m.receipt)+octet_length(m.generation::text)+octet_length(m.kind)+octet_length(m.canonical_json)),0) FROM raw_manifests m WHERE m.device_id=$1 AND m.provider=$2 AND m.configured_root_id=$3 AND m.source_key_sha256=$4
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(e.manifest_id)+octet_length(e.entry_index::text)+octet_length(e.path)+octet_length(e.path_sha256)+octet_length(e.entry_type)+octet_length(e.size_bytes::text)),0) FROM raw_manifest_entries e JOIN raw_manifests m USING(manifest_id) WHERE m.device_id=$1 AND m.provider=$2 AND m.configured_root_id=$3 AND m.source_key_sha256=$4
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(o.manifest_id)+octet_length(o.entry_index::text)+octet_length(o.object_index::text)+octet_length(o.sha256)+octet_length(o.size_bytes::text)),0) FROM raw_manifest_objects o JOIN raw_manifests m USING(manifest_id) WHERE m.device_id=$1 AND m.provider=$2 AND m.configured_root_id=$3 AND m.source_key_sha256=$4
	 UNION ALL SELECT count(*),COALESCE(sum(octet_length(o.sha256)+octet_length(o.size_bytes::text)),0) FROM raw_objects o WHERE EXISTS(SELECT 1 FROM raw_manifest_objects mo JOIN raw_manifests m USING(manifest_id) WHERE m.device_id=$1 AND m.provider=$2 AND m.configured_root_id=$3 AND m.source_key_sha256=$4 AND mo.sha256=o.sha256 AND mo.size_bytes=o.size_bytes)
	) sizes`, source.source.Identity.DeviceID, source.provider, source.rootID, source.keySHA256).Scan(&rows, &bytes)
	return rows, bytes, err
}

func resolveParityBaselineScope(ctx context.Context, baseline *sql.DB) (MigrationParityOptions, string, error) {
	if baseline == nil {
		return MigrationParityOptions{}, "", fmt.Errorf("baseline_unprovisioned: baseline database is required")
	}
	var schema, tenant, identity string
	err := baseline.QueryRowContext(ctx, `SELECT current_schema(),current_setting('agentsview.tenant_id')`).Scan(&schema, &tenant)
	if err != nil {
		return MigrationParityOptions{}, "", err
	}
	err = baseline.QueryRowContext(ctx, `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return MigrationParityOptions{}, "", fmt.Errorf("baseline_unprovisioned: target identity is missing")
	}
	if err != nil {
		return MigrationParityOptions{}, "", err
	}
	if err = validateHostedBinding(schema, tenant); err != nil {
		return MigrationParityOptions{}, "", fmt.Errorf("baseline_unprovisioned: %w", err)
	}
	return MigrationParityOptions{Schema: schema, Tenant: tenant}, identity, nil
}

// CaptureParityBaseline streams one immutable inventory from two pinned
// read-only snapshots and seals it only after both censuses complete.
func (s *MigrationParityStore) CaptureParityBaseline(ctx context.Context, lease rawderive.ParityLease, baseline *sql.DB, binding rawderive.ParityBinding) error {
	if binding.Request != lease.Request || binding.Tenant != s.tenant || binding.ObservedAt.IsZero() || binding.ObservedAt != lease.ObservedAt {
		return fmt.Errorf("binding_conflict: parity capture does not match lease")
	}
	bindingDigest, err := rawderive.DigestParityBinding(binding)
	if err != nil {
		return err
	}
	if bindingDigest != lease.BindingDigest {
		return fmt.Errorf("binding_conflict: parity capture binding changed")
	}
	baselineOptions, baselineIdentity, err := resolveParityBaselineScope(ctx, baseline)
	if err != nil {
		return err
	}
	if baselineOptions.Tenant != binding.Tenant || baselineIdentity != binding.BaselineID {
		return fmt.Errorf("binding_conflict: baseline scope does not match binding")
	}
	if err = checkMigrationParityBaseline(ctx, baseline, baselineOptions, binding.BaselineID); err != nil {
		return fmt.Errorf("checking baseline scope: %w", err)
	}

	runtimeTx, err := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = runtimeTx.Rollback() }()
	baselineTx, err := baseline.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = baselineTx.Rollback() }()
	var runtimeObserved, baselineObserved time.Time
	var runtimeSnapshot, baselineSnapshot string
	if err = runtimeTx.QueryRowContext(ctx, `SELECT transaction_timestamp(),pg_current_snapshot()::text`).Scan(&runtimeObserved, &runtimeSnapshot); err != nil {
		return err
	}
	if err = baselineTx.QueryRowContext(ctx, `SELECT transaction_timestamp(),pg_current_snapshot()::text`).Scan(&baselineObserved, &baselineSnapshot); err != nil {
		return err
	}
	_ = runtimeSnapshot
	_ = baselineSnapshot

	if err = s.ensureEmptyParityEpoch(ctx, lease); err != nil {
		return fmt.Errorf("initializing evidence epoch: %w", err)
	}
	inventoryHash := sha256.New()
	_, _ = inventoryHash.Write([]byte("agentsview-parity-inventory-v1"))
	baselineHash := sha256.New()
	_, _ = baselineHash.Write([]byte("agentsview-parity-baseline-v1"))

	afterSource := ""
	for {
		sources, scanErr := readParityCohortSources(ctx, runtimeTx, binding, afterSource, parityInventoryPageSize)
		if scanErr != nil {
			return fmt.Errorf("reading runtime source census: %w", scanErr)
		}
		s.observeParityCensusPage("capture_runtime_sources", len(sources))
		for i := range sources {
			source := &sources[i]
			var historyCode string
			source.manifestRows, historyCode, err = readParityReceiptChain(ctx, runtimeTx, *source)
			if err != nil {
				return fmt.Errorf("reading source history: %w", err)
			}
			if source.partialCode == "" {
				source.partialCode = historyCode
			}
			runtimeDependency, dependencyCode, dependencyErr := readParityRuntimeDependency(ctx, runtimeTx, *source)
			if dependencyErr != nil {
				return fmt.Errorf("reading runtime source dependency: %w", dependencyErr)
			}
			source.runtimeDependency = runtimeDependency
			if source.partialCode == "" || dependencyCode == "limit_exceeded" {
				source.partialCode = dependencyCode
			}
			source.source.DependencyDigest = paritySourceDependency(*source)
			if err = s.storeCapturedParitySource(ctx, lease, *source); err != nil {
				return fmt.Errorf("writing source evidence: %w", err)
			}
			if err = s.runParityEvidencePageHook(ctx, "baseline_source"); err != nil {
				return err
			}
			parityHashField(inventoryHash, []byte(source.source.ID))
			parityHashField(inventoryHash, source.source.DependencyDigest[:])
		}
		if len(sources) < parityInventoryPageSize {
			break
		}
		afterSource = sources[len(sources)-1].cursor
	}
	afterSource = ""
	for {
		baselineSources, scanErr := readParityCohortSources(ctx, baselineTx, binding, afterSource, parityInventoryPageSize)
		if scanErr != nil {
			return fmt.Errorf("reading baseline source census: %w", scanErr)
		}
		s.observeParityCensusPage("capture_baseline_sources", len(baselineSources))
		for _, source := range baselineSources {
			var added bool
			var dependency rawderive.ParityDigest
			added, dependency, err = s.ensureCapturedBaselineSource(ctx, lease, source)
			if err != nil {
				return err
			}
			if added {
				parityHashField(inventoryHash, []byte(source.source.ID))
				parityHashField(inventoryHash, dependency[:])
			}
			if !source.hasProjection {
				continue
			}
			if err = s.captureRawBaselineMembers(ctx, lease, baselineTx, binding, source.source, inventoryHash, baselineHash); err != nil {
				return fmt.Errorf("reading baseline member census: %w", err)
			}
		}
		if len(baselineSources) < parityInventoryPageSize {
			break
		}
		afterSource = baselineSources[len(baselineSources)-1].cursor
	}
	if err = s.captureLegacyBaselineMembers(ctx, lease, baselineTx, binding, inventoryHash, baselineHash); err != nil {
		return fmt.Errorf("reading baseline legacy census: %w", err)
	}

	if err = runtimeTx.Commit(); err != nil {
		return fmt.Errorf("closing runtime snapshot: %w", err)
	}
	if err = baselineTx.Commit(); err != nil {
		return fmt.Errorf("closing baseline snapshot: %w", err)
	}
	return s.sealParityBaseline(ctx, lease, parityDigestHash(inventoryHash), parityDigestHash(baselineHash), baselineObserved, runtimeObserved)
}

func (s *MigrationParityStore) ensureEmptyParityEpoch(ctx context.Context, lease rawderive.ParityLease) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseTx(ctx, tx, lease, false); err != nil {
		return err
	}
	var sealed bool
	if err = tx.QueryRowContext(ctx, `SELECT baseline_sealed FROM `+s.quoted+`.migration_parity_runs WHERE run_id=$1 AND init_epoch=$2 FOR UPDATE`, lease.RunID, lease.InitEpoch).Scan(&sealed); err != nil {
		return err
	}
	if sealed {
		return fmt.Errorf("binding_conflict: parity baseline is already sealed")
	}
	// A failed capture can leave only evidence rows in this unsealed epoch.
	// Reclaim them under the run lock before starting the next complete census.
	for _, table := range []string{"migration_parity_members", "migration_parity_sources", "migration_parity_dependencies"} {
		if _, err = tx.ExecContext(ctx, `DELETE FROM `+s.quoted+`.`+table+` WHERE run_id=$1 AND init_epoch=$2`, lease.RunID, lease.InitEpoch); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_runs SET state='initializing',last_code='pending' WHERE run_id=$1 AND init_epoch=$2`, lease.RunID, lease.InitEpoch)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func readParityCohortSources(ctx context.Context, tx *sql.Tx, binding rawderive.ParityBinding, after string, limit int) ([]parityCapturedSource, error) {
	rows, err := tx.QueryContext(ctx, `SELECT h.device_id,h.provider,h.configured_root_id,h.source_key,h.source_key_sha256,
		COALESCE(h.manifest_id,''),h.generation,COALESCE(h.receipt,''),p.source_id,p.selected_manifest_id,p.processing_version,
		p.projection_generation,p.selected_job_id,p.successful_manifest_id,p.last_attempt_manifest_id,p.membership_complete,p.diagnostics
		FROM raw_source_heads h LEFT JOIN raw_source_projections p
		ON p.device_id=h.device_id AND p.provider=h.provider AND p.configured_root_id=h.configured_root_id AND p.source_key_sha256=h.source_key_sha256
		WHERE h.device_id=$1 AND h.provider=$2 AND h.configured_root_id=$3 AND h.generation>0 AND h.source_key_sha256>$4
		ORDER BY h.source_key_sha256 LIMIT $5`, binding.Request.Cohort.DeviceID, string(binding.Request.Cohort.Provider), binding.Request.Cohort.RootID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []parityCapturedSource
	for rows.Next() {
		source, scanErr := scanParityCapturedSource(rows, binding.Tenant)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, source)
	}
	return result, rows.Err()
}

type parityRowScanner interface {
	Scan(...any) error
}

func scanParityCapturedSource(row parityRowScanner, tenant string) (parityCapturedSource, error) {
	var source parityCapturedSource
	var projectionID, selected, version, diagnostics sql.NullString
	var projectionGeneration, selectedJob sql.NullInt64
	var membership sql.NullBool
	if err := row.Scan(&source.source.Identity.DeviceID, &source.provider, &source.rootID, &source.sourceKey, &source.keySHA256,
		&source.source.HeadManifestID, &source.source.HeadGeneration, &source.headReceipt, &projectionID, &selected, &version,
		&projectionGeneration, &selectedJob, &source.successfulManifest, &source.lastAttemptManifest, &membership, &diagnostics); err != nil {
		return parityCapturedSource{}, err
	}
	source.source.Identity.TenantID = tenant
	manifest := rawsync.CanonicalManifest{Identity: source.source.Identity, Manifest: rawsync.Manifest{
		Provider: parser.AgentType(source.provider), ConfiguredRootID: source.rootID, SourceKey: source.sourceKey,
	}}
	source.source.ID = rawderive.SourceID(manifest)
	source.cursor = source.keySHA256
	source.hasProjection = projectionID.Valid
	if source.hasProjection {
		source.selectedManifest, source.processingVersion = selected.String, version.String
		source.projectionGeneration, source.selectedJobID = projectionGeneration.Int64, selectedJob.Int64
		source.membershipComplete, source.diagnostics = membership.Bool, diagnostics.String
		if projectionID.String != source.source.ID {
			source.partialCode = "invalid"
		}
	} else {
		source.partialCode = "missing_object"
	}
	source.source.Required = true
	return source, nil
}

func (s *MigrationParityStore) storeCapturedParitySource(ctx context.Context, lease rawderive.ParityLease, source parityCapturedSource) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseTx(ctx, tx, lease, false); err != nil {
		return err
	}
	var verdict any
	code := "pending"
	if source.partialCode != "" {
		verdict = string(rawderive.ParityPartial)
		code = source.partialCode
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_generation,head_receipt,source_key_sha256,dependency_digest,captured_verdict,captured_code,verdict,code,required)
		VALUES($1,$2,$3,'raw',$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$12,$13,TRUE)`, lease.RunID, lease.InitEpoch, source.source.ID,
		source.source.Identity.DeviceID, string(lease.Request.Cohort.Provider), source.rootID, source.source.HeadManifestID,
		source.source.HeadGeneration, source.headReceipt, source.keySHA256, source.source.DependencyDigest[:], verdict, code)
	if err != nil {
		return err
	}
	for _, entry := range source.manifestRows {
		key := encodeParityHistoryEntry(entry)
		keyDigest := sha256.Sum256(key)
		expected := sha256.Sum256(append([]byte("agentsview-parity-history-v1"), key...))
		_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_dependencies(run_id,init_epoch,source_id,kind,dependency_key,key_digest,expected,sequence)
			VALUES($1,$2,$3,'manifest',$4,$5,$6,$7)`, lease.RunID, lease.InitEpoch, source.source.ID, key, keyDigest[:], expected[:], entry.Generation)
		if err != nil {
			return err
		}
	}
	headKey := encodeParityHeadDependency(source.source.ID, source.source.HeadManifestID, source.source.HeadGeneration, source.headReceipt)
	headDigest := sha256.Sum256(headKey)
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_dependencies(run_id,init_epoch,source_id,kind,dependency_key,key_digest,expected)
		VALUES($1,$2,$3,'head',$4,$5,$6)`, lease.RunID, lease.InitEpoch, source.source.ID, headKey, headDigest[:], source.source.DependencyDigest[:])
	if err != nil {
		return err
	}
	projectionKey := []byte("agentsview-parity-projection-v1\x00" + source.source.ID)
	projectionDigest := sha256.Sum256(projectionKey)
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_dependencies(run_id,init_epoch,source_id,kind,dependency_key,key_digest,expected)
		VALUES($1,$2,$3,'projection',$4,$5,$6)`, lease.RunID, lease.InitEpoch, source.source.ID, projectionKey, projectionDigest[:], source.source.DependencyDigest[:])
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MigrationParityStore) ensureCapturedBaselineSource(ctx context.Context, lease rawderive.ParityLease, source parityCapturedSource) (bool, rawderive.ParityDigest, error) {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return false, rawderive.ParityDigest{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseTx(ctx, tx, lease, false); err != nil {
		return false, rawderive.ParityDigest{}, err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.quoted+`.migration_parity_sources WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3)`, lease.RunID, lease.InitEpoch, source.source.ID).Scan(&exists); err != nil {
		return false, rawderive.ParityDigest{}, err
	}
	if exists {
		return false, rawderive.ParityDigest{}, tx.Commit()
	}
	dependency := sha256.Sum256([]byte("agentsview-parity-baseline-only-source-v1\x00" + source.source.ID))
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_generation,head_receipt,source_key_sha256,dependency_digest,captured_verdict,captured_code,verdict,code,candidate_complete,required)
		VALUES($1,$2,$3,'raw',$4,$5,$6,$7,$8,$9,$10,$11,'missing','missing_object','missing','missing_object',TRUE,TRUE)`, lease.RunID, lease.InitEpoch, source.source.ID,
		source.source.Identity.DeviceID, source.provider, source.rootID, source.source.HeadManifestID, source.source.HeadGeneration, source.headReceipt, source.keySHA256, dependency[:])
	if err != nil {
		return false, rawderive.ParityDigest{}, err
	}
	if err = tx.Commit(); err != nil {
		return false, rawderive.ParityDigest{}, err
	}
	return true, rawderive.ParityDigest(dependency), nil
}

func readParityReceiptChain(ctx context.Context, tx *sql.Tx, source parityCapturedSource) ([]rawderive.ParityHistoryEntry, string, error) {
	if source.source.HeadGeneration < 1 || source.headReceipt == "" {
		return nil, "missing_history", nil
	}
	current := source.headReceipt
	seen := make(map[string]struct{}, 16)
	chain := make([]rawderive.ParityHistoryEntry, 0, min(int(source.source.HeadGeneration), 1024))
	for current != "" && len(chain) < 1024 {
		if _, duplicate := seen[current]; duplicate {
			return chain, "missing_history", nil
		}
		seen[current] = struct{}{}
		var entry rawderive.ParityHistoryEntry
		var device, provider, root, keySHA string
		err := tx.QueryRowContext(ctx, `SELECT manifest_id,receipt,parent_receipt,generation,device_id,provider,configured_root_id,source_key_sha256
			FROM raw_manifests WHERE receipt=$1`, current).Scan(&entry.ManifestID, &entry.Receipt, &entry.ParentReceipt, &entry.Generation, &device, &provider, &root, &keySHA)
		if err == sql.ErrNoRows {
			return chain, "missing_history", nil
		}
		if err != nil {
			return nil, "", err
		}
		if device != source.source.Identity.DeviceID || provider != source.provider || root != source.rootID || keySHA != source.keySHA256 ||
			entry.Generation != source.source.HeadGeneration-int64(len(chain)) || len(chain) == 0 && entry.ManifestID != source.source.HeadManifestID {
			return chain, "missing_history", nil
		}
		if entry.Generation == 1 && entry.ParentReceipt != "" || entry.Generation > 1 && entry.ParentReceipt == "" {
			return chain, "missing_history", nil
		}
		chain = append(chain, entry)
		current = entry.ParentReceipt
	}
	if current != "" || len(chain) == 0 || chain[len(chain)-1].Generation != 1 {
		return chain, "missing_history", nil
	}
	for left, right := 0, len(chain)-1; left < right; left, right = left+1, right-1 {
		chain[left], chain[right] = chain[right], chain[left]
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT c.manifest_id FROM raw_source_contributions c JOIN raw_session_branches b ON b.branch_id=c.branch_id WHERE b.source_id=$1`, source.source.ID)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	manifestSet := make(map[string]struct{}, len(chain))
	for _, entry := range chain {
		manifestSet[entry.ManifestID] = struct{}{}
	}
	for rows.Next() {
		var manifest string
		if err = rows.Scan(&manifest); err != nil {
			return nil, "", err
		}
		if _, ok := manifestSet[manifest]; !ok {
			return chain, "missing_history", nil
		}
	}
	return chain, "", rows.Err()
}

func (s *MigrationParityStore) captureRawBaselineMembers(ctx context.Context, lease rawderive.ParityLease, baselineTx *sql.Tx, binding rawderive.ParityBinding, source rawderive.ParitySource, inventoryHash, baselineHash hash.Hash) error {
	after := ""
	for {
		page, next, err := readParityRawMemberPage(ctx, baselineTx, binding, source.ID, after, parityInventoryPageSize)
		if err != nil {
			return err
		}
		s.observeParityCensusPage("capture_members", len(page))
		if len(page) > 0 {
			if err = s.storeCapturedParityMembers(ctx, lease, source.ID, page, inventoryHash, baselineHash); err != nil {
				return err
			}
			if err = s.runParityEvidencePageHook(ctx, "baseline_member"); err != nil {
				return err
			}
		}
		if len(page) < parityInventoryPageSize {
			return nil
		}
		after = next
	}
}

func (s *MigrationParityStore) runParityEvidencePageHook(ctx context.Context, phase string) error {
	if s.parityEvidencePageHook == nil {
		return nil
	}
	return s.parityEvidencePageHook(ctx, phase)
}

func readParityRawMemberPage(ctx context.Context, baselineTx *sql.Tx, binding rawderive.ParityBinding, sourceID, after string, limit int) ([]parityCapturedMember, string, error) {
	rows, err := baselineTx.QueryContext(ctx, `SELECT b.group_id,g.logical_key,b.branch_id,
			COALESCE(ss.physical_session_id,''),
			(ss.branch_id IS NOT NULL AND ss.group_id=b.group_id AND ss.source_id=b.source_id AND ss.session_id=b.session_id
			 AND ss.manifest_id=b.manifest_id AND ss.content_revision=b.content_revision AND ss.processing_version=b.processing_version
			 AND ss.projection_generation=b.projection_generation AND ss.physical_session_id=b.session_id
			 AND materialized.id IS NOT NULL AND materialized.provenance_kind='raw' AND materialized.raw_group_id=b.group_id
			 AND materialized.raw_content_revision=b.content_revision) AS proof_exact
			FROM raw_source_projections p JOIN raw_session_branches b ON b.source_id=p.source_id AND b.active
			JOIN raw_session_groups g ON g.group_id=b.group_id
			LEFT JOIN session_sources ss ON ss.branch_id=b.branch_id
			LEFT JOIN sessions materialized ON materialized.id=ss.physical_session_id
			WHERE b.source_id=$1 AND p.device_id=$4 AND p.provider=$5 AND p.configured_root_id=$6
			AND b.group_id>$2 ORDER BY b.group_id LIMIT $3`, sourceID, after, limit,
		binding.Request.Cohort.DeviceID, string(binding.Request.Cohort.Provider), binding.Request.Cohort.RootID)
	if err != nil {
		return nil, "", err
	}
	var page []parityCapturedMember
	next := after
	for rows.Next() {
		var groupID, branchID string
		var exact bool
		var member parityCapturedMember
		if err = rows.Scan(&groupID, &member.key.LogicalKey, &branchID, &member.physicalID, &exact); err != nil {
			rows.Close()
			return nil, "", err
		}
		member.key.SourceID = sourceID
		member.key.Kind = "session"
		member.mappingState = "exact"
		if !exact || member.physicalID == "" {
			member.mappingState = "ambiguous"
			member.verdict = rawderive.ParityAmbiguous
			member.physicalID = ""
		} else {
			member.canRead = true
		}
		page = append(page, member)
		next = groupID
	}
	scanErr := rows.Err()
	rows.Close()
	if scanErr != nil {
		return nil, "", scanErr
	}
	// PostgreSQL permits only one active result stream on this transaction's
	// connection. Buffer the bounded key page, close it, then hydrate each
	// member from the same pinned snapshot.
	for i := range page {
		member := &page[i]
		if !member.canRead {
			continue
		}
		physical, readErr := readParityPhysicalMember(ctx, baselineTx, binding, member.physicalID, member.key)
		if readErr != nil {
			return nil, "", readErr
		}
		if physical.InvalidCode != "" {
			member.verdict = rawderive.ParityPartial
			continue
		}
		member.overlay = physical.Graph.Overlay
		for linkIndex := range physical.Graph.Links {
			original := physical.Graph.Links[linkIndex]
			resolved, resolveErr := readParityLinkTarget(ctx, baselineTx, sourceID, original)
			if resolveErr != nil {
				return nil, "", resolveErr
			}
			lookup, value := encodeParityRelationshipDependency(member.key, original, resolved)
			if len(lookup)+len(value) > parityPhysicalMaxBytes || member.relationshipBytes+int64(len(lookup)+len(value)) > parityPhysicalMaxBytes {
				member.verdict = rawderive.ParityPartial
				member.fingerprint = nil
				break
			}
			member.relationshipBytes += int64(len(lookup) + len(value))
			member.relationshipLookups = append(member.relationshipLookups, lookup)
			member.relationshipValues = append(member.relationshipValues, value)
			physical.Graph.Links[linkIndex] = resolved
			if resolved.Unresolved != "" {
				member.verdict = rawderive.ParityAmbiguous
			}
		}
		if member.verdict == rawderive.ParityPartial {
			continue
		}
		member.links = append([]rawderive.ParityLink(nil), physical.Graph.Links...)
		fingerprint, fingerprintErr := rawderive.FingerprintParity(ctx, binding, physical.Graph)
		if fingerprintErr != nil {
			return nil, "", fingerprintErr
		}
		member.fingerprint = &fingerprint
		member.physical = &physical.Physical
	}
	return page, next, nil
}

func (s *MigrationParityStore) storeCapturedParityMembers(ctx context.Context, lease rawderive.ParityLease, sourceID string, members []parityCapturedMember, inventoryHash, baselineHash hash.Hash) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseTx(ctx, tx, lease, false); err != nil {
		return err
	}
	for _, member := range members {
		key, memberDigest := encodeParityMemberKey(member.key)
		var baselineFingerprint, physicalFingerprint any
		if member.fingerprint != nil {
			baselineFingerprint = encodeParityFingerprint(*member.fingerprint)
		}
		if member.physical != nil {
			physicalFingerprint = member.physical[:]
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_members(run_id,init_epoch,source_id,member_key,member_digest,kind,baseline_ref,required,mapping_state,baseline_fingerprint,physical_fingerprint,captured_verdict,verdict,inventory_kind)
			VALUES($1,$2,$3,$4,$5,$6,$7,TRUE,$8,$9,$10,$11,$11,'baseline')`, lease.RunID, lease.InitEpoch, sourceID, key, memberDigest[:], member.key.Kind,
			nullableStringValue(member.physicalID), member.mappingState, baselineFingerprint, physicalFingerprint, nullableParityVerdict(member.verdict))
		if err != nil {
			return err
		}
		memberDependency := parityCapturedMemberDependency(member)
		_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_dependencies(run_id,init_epoch,source_id,kind,dependency_key,key_digest,expected)
			VALUES($1,$2,$3,'member',$4,$5,$6)`, lease.RunID, lease.InitEpoch, sourceID, key, memberDigest[:], memberDependency[:])
		if err != nil {
			return err
		}
		for index, lookup := range member.relationshipLookups {
			keyDigest := sha256.Sum256(lookup)
			expected := sha256.Sum256(member.relationshipValues[index])
			_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_dependencies(run_id,init_epoch,source_id,kind,dependency_key,key_digest,expected)
				VALUES($1,$2,$3,'relationship',$4,$5,$6)`, lease.RunID, lease.InitEpoch, sourceID, member.relationshipValues[index], keyDigest[:], expected[:])
			if err != nil {
				return err
			}
		}
		overlayLookup, overlayValue := encodeParityOverlayDependency(member.key, member.overlay)
		overlayKeyDigest := sha256.Sum256(overlayLookup)
		overlayExpected := sha256.Sum256(overlayValue)
		_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_dependencies(run_id,init_epoch,source_id,kind,dependency_key,key_digest,expected)
			VALUES($1,$2,$3,'overlay',$4,$5,$6)`, lease.RunID, lease.InitEpoch, sourceID, overlayValue, overlayKeyDigest[:], overlayExpected[:])
		if err != nil {
			return err
		}
		parityHashField(inventoryHash, key)
		if member.physical != nil {
			parityHashField(inventoryHash, member.physical[:])
		}
		if member.fingerprint != nil {
			parityHashField(baselineHash, encodeParityFingerprint(*member.fingerprint))
		}
	}
	if err = s.reduceCapturedSourceBlockers(ctx, tx, lease, sourceID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MigrationParityStore) reduceCapturedSourceBlockers(ctx context.Context, tx *sql.Tx, lease rawderive.ParityLease, sourceID string) error {
	var sourceVerdict sql.NullString
	var code string
	if err := tx.QueryRowContext(ctx, `SELECT captured_verdict,captured_code FROM `+s.quoted+`.migration_parity_sources WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 FOR UPDATE`, lease.RunID, lease.InitEpoch, sourceID).Scan(&sourceVerdict, &code); err != nil {
		return err
	}
	verdict := rawderive.ParityVerdict("")
	if sourceVerdict.Valid {
		verdict = rawderive.ParityVerdict(sourceVerdict.String)
	}
	rows, err := tx.QueryContext(ctx, `SELECT captured_verdict FROM `+s.quoted+`.migration_parity_members WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND captured_verdict IS NOT NULL`, lease.RunID, lease.InitEpoch, sourceID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var member rawderive.ParityVerdict
		if err = rows.Scan(&member); err != nil {
			rows.Close()
			return err
		}
		verdict = strongerParityVerdict(verdict, member)
		if member == rawderive.ParityPartial && code == "pending" {
			code = "limit_exceeded"
		}
	}
	if err = rows.Close(); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_sources SET captured_verdict=$4,captured_code=$5,verdict=$4,code=$5 WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3`, lease.RunID, lease.InitEpoch, sourceID, nullableParityVerdict(verdict), code)
	return err
}

func parityCapturedMemberDependency(member parityCapturedMember) rawderive.ParityDigest {
	h := sha256.New()
	_, _ = h.Write([]byte("agentsview-parity-member-dependency-v1"))
	key, _ := encodeParityMemberKey(member.key)
	parityHashField(h, key)
	parityHashField(h, []byte(member.mappingState))
	parityHashField(h, []byte(member.physicalID))
	if member.physical != nil {
		parityHashField(h, member.physical[:])
	}
	for _, value := range member.relationshipValues {
		parityHashField(h, value)
	}
	_, overlay := encodeParityOverlayDependency(member.key, member.overlay)
	parityHashField(h, overlay)
	return parityDigestHash(h)
}

func nullableStringValue(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s *MigrationParityStore) captureLegacyBaselineMembers(ctx context.Context, lease rawderive.ParityLease, baselineTx *sql.Tx, binding rawderive.ParityBinding, inventoryHash, baselineHash hash.Hash) error {
	after := ""
	for {
		rows, err := baselineTx.QueryContext(ctx, `SELECT id,COALESCE(NULLIF(source_session_id,''),id),
			EXISTS(SELECT 1 FROM raw_session_public_aliases WHERE alias_id IN (sessions.id,sessions.source_session_id))
			FROM sessions WHERE provenance_kind='legacy' AND agent=$1 AND id>$2 ORDER BY id LIMIT $3`, string(binding.Request.Cohort.Provider), after, parityInventoryPageSize)
		if err != nil {
			return err
		}
		var page []parityCapturedMember
		for rows.Next() {
			var physicalID, logical string
			var collision bool
			if err = rows.Scan(&physicalID, &logical, &collision); err != nil {
				rows.Close()
				return err
			}
			sourceDigest := sha256.Sum256([]byte("agentsview-parity-legacy-source-v1\x00" + physicalID))
			sourceID := fmt.Sprintf("legacy:%x", sourceDigest[:])
			dependency := sha256.Sum256([]byte(physicalID))
			captured := parityCapturedSource{source: rawderive.ParitySource{ID: sourceID, Identity: rawsync.AuthIdentity{TenantID: binding.Tenant}, Required: true, DependencyDigest: rawderive.ParityDigest(dependency)}}
			if err = s.storeCapturedLegacySource(ctx, lease, captured, collision); err != nil {
				rows.Close()
				return err
			}
			verdict := rawderive.ParityLegacyOnly
			mapping := "legacy_only"
			if collision {
				verdict = rawderive.ParityAmbiguous
				mapping = "ambiguous"
			}
			page = append(page, parityCapturedMember{key: rawderive.ParityMemberKey{SourceID: sourceID, LogicalKey: logical, Kind: "legacy"}, physicalID: physicalID, mappingState: mapping, verdict: verdict})
			after = physicalID
		}
		scanErr := rows.Err()
		rows.Close()
		if scanErr != nil {
			return scanErr
		}
		for i := range page {
			member := &page[i]
			physical, readErr := readParityPhysicalMember(ctx, baselineTx, binding, member.physicalID, member.key)
			if readErr != nil {
				return readErr
			}
			if physical.InvalidCode == "" {
				fingerprint, fingerprintErr := rawderive.FingerprintParity(ctx, binding, physical.Graph)
				if fingerprintErr != nil {
					return fingerprintErr
				}
				member.fingerprint = &fingerprint
				member.physical = &physical.Physical
			} else {
				member.verdict = rawderive.ParityPartial
			}
			if err = s.storeCapturedParityMembers(ctx, lease, member.key.SourceID, []parityCapturedMember{*member}, inventoryHash, baselineHash); err != nil {
				return err
			}
		}
		if len(page) < parityInventoryPageSize {
			return nil
		}
	}
}

func (s *MigrationParityStore) storeCapturedLegacySource(ctx context.Context, lease rawderive.ParityLease, source parityCapturedSource, collision bool) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseTx(ctx, tx, lease, false); err != nil {
		return err
	}
	verdict := rawderive.ParityLegacyOnly
	if collision {
		verdict = rawderive.ParityAmbiguous
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_generation,head_receipt,dependency_digest,captured_verdict,captured_code,verdict,code,candidate_complete,required)
		VALUES($1,$2,$3,'legacy','',$4,'','',0,'',$5,$6,'pending',$6,'pending',TRUE,TRUE)`, lease.RunID, lease.InitEpoch, source.source.ID, string(lease.Request.Cohort.Provider), source.source.DependencyDigest[:], verdict)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MigrationParityStore) sealParityBaseline(ctx context.Context, lease rawderive.ParityLease, inventory, baseline rawderive.ParityDigest, baselineObserved, runtimeObserved time.Time) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_runs
		SET baseline_sealed=true,inventory_digest=$1,baseline_digest=$2,state='running',baseline_observed_at=$8,runtime_observed_at=$9
		WHERE tenant_id=$3 AND run_id=$4 AND init_epoch=$5
		  AND lease_token=$6 AND binding_digest=$7 AND baseline_sealed=false
		  AND request_generation=$10 AND lease_owner=$11 AND lease_expires_at>clock_timestamp()`, inventory[:], baseline[:], s.tenant, lease.RunID, lease.InitEpoch,
		lease.Token, lease.BindingDigest[:], baselineObserved, runtimeObserved, lease.RequestGeneration, lease.Owner)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("binding_conflict: parity baseline seal lost lease")
	}
	return tx.Commit()
}
