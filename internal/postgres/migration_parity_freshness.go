package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"hash"
	"time"

	"go.kenn.io/agentsview/internal/rawderive"
)

// ValidateParityEvidence performs a complete fresh census. Page observations
// are durable staging only; the final fenced transaction is the sole operation
// that makes a validation epoch visible to report aggregation.
func (s *MigrationParityStore) ValidateParityEvidence(ctx context.Context, lease rawderive.ParityLease, baseline *sql.DB, binding rawderive.ParityBinding, pageSize int) error {
	if pageSize < 1 || pageSize > parityInventoryPageSize {
		return fmt.Errorf("invalid migration parity validation page size")
	}
	if binding.Request != lease.Request || binding.Tenant != s.tenant || binding.ObservedAt != lease.ObservedAt {
		return fmt.Errorf("binding_conflict: parity validation does not match lease")
	}
	digest, err := rawderive.DigestParityBinding(binding)
	if err != nil {
		return err
	}
	if digest != lease.BindingDigest {
		return fmt.Errorf("binding_conflict: parity validation binding changed")
	}
	baselineOptions, baselineIdentity, err := resolveParityBaselineScope(ctx, baseline)
	if err != nil {
		return err
	}
	if baselineOptions.Tenant != binding.Tenant || baselineIdentity != binding.BaselineID {
		return fmt.Errorf("binding_conflict: baseline scope does not match binding")
	}
	if err = checkMigrationParityBaseline(ctx, baseline, baselineOptions, binding.BaselineID); err != nil {
		return err
	}
	validationEpoch, err := s.beginParityValidation(ctx, lease)
	if err != nil {
		return err
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
	validationHash := sha256.New()
	_, _ = validationHash.Write([]byte("agentsview-parity-validation-v1"))
	parityHashField(validationHash, []byte(runtimeSnapshot))
	parityHashField(validationHash, []byte(baselineSnapshot))

	after := ""
	for {
		sources, readErr := readParityCohortSources(ctx, runtimeTx, binding, after, pageSize)
		if readErr != nil {
			return readErr
		}
		s.observeParityCensusPage("validation_runtime_sources", len(sources))
		for i := range sources {
			source := &sources[i]
			var historyCode string
			source.manifestRows, historyCode, err = readParityReceiptChain(ctx, runtimeTx, *source)
			if err != nil {
				return err
			}
			if source.partialCode == "" {
				source.partialCode = historyCode
			}
			runtimeDependency, dependencyCode, dependencyErr := readParityRuntimeDependency(ctx, runtimeTx, *source)
			if dependencyErr != nil {
				return dependencyErr
			}
			source.runtimeDependency = runtimeDependency
			if source.partialCode == "" {
				source.partialCode = dependencyCode
			}
			source.source.DependencyDigest = paritySourceDependency(*source)
			if err = s.stageParitySourceObservation(ctx, lease, validationEpoch, *source); err != nil {
				return err
			}
			if err = s.runParityEvidencePageHook(ctx, "validation_source"); err != nil {
				return err
			}
			parityHashField(validationHash, []byte(source.source.ID))
			parityHashField(validationHash, source.source.DependencyDigest[:])
		}
		if len(sources) < pageSize {
			break
		}
		after = sources[len(sources)-1].cursor
	}

	after = ""
	for {
		sources, readErr := readParityCohortSources(ctx, baselineTx, binding, after, pageSize)
		if readErr != nil {
			return readErr
		}
		s.observeParityCensusPage("validation_baseline_sources", len(sources))
		for _, source := range sources {
			if err = s.stageParityBaselineSourceObservation(ctx, lease, validationEpoch, source); err != nil {
				return err
			}
			if !source.hasProjection {
				continue
			}
			memberAfter := ""
			for {
				members, next, memberErr := readParityRawMemberPage(ctx, baselineTx, binding, source.source.ID, memberAfter, pageSize)
				if memberErr != nil {
					return memberErr
				}
				s.observeParityCensusPage("validation_members", len(members))
				if len(members) > 0 {
					if err = s.stageParityMemberObservations(ctx, lease, validationEpoch, source.source.ID, members); err != nil {
						return err
					}
					if err = s.runParityEvidencePageHook(ctx, "validation_member"); err != nil {
						return err
					}
					for _, member := range members {
						key, _ := encodeParityMemberKey(member.key)
						dependency := parityCapturedMemberDependency(member)
						parityHashField(validationHash, key)
						parityHashField(validationHash, dependency[:])
					}
				}
				if len(members) < pageSize {
					break
				}
				memberAfter = next
			}
		}
		if len(sources) < pageSize {
			break
		}
		after = sources[len(sources)-1].cursor
	}
	if err = s.stageLegacyParityValidation(ctx, lease, validationEpoch, baselineTx, binding, pageSize, validationHash); err != nil {
		return err
	}

	if err = runtimeTx.Commit(); err != nil {
		return err
	}
	if err = baselineTx.Commit(); err != nil {
		return err
	}
	if err = checkMigrationParityBaseline(ctx, baseline, baselineOptions, binding.BaselineID); err != nil {
		return err
	}
	return s.finishParityValidation(ctx, lease, validationEpoch, parityDigestHash(validationHash), baselineObserved, runtimeObserved)
}

func (s *MigrationParityStore) stageParityBaselineSourceObservation(ctx context.Context, lease rawderive.ParityLease, epoch int64, source parityCapturedSource) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseTx(ctx, tx, lease, true); err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.quoted+`.migration_parity_sources WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3)`, lease.RunID, lease.InitEpoch, source.source.ID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return tx.Commit()
	}
	dependency := sha256.Sum256([]byte("agentsview-parity-baseline-only-source-v1\x00" + source.source.ID))
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_generation,head_receipt,dependency_digest,verdict,code,candidate_complete,required,validation_epoch,inventory_kind)
		VALUES($1,$2,$3,'raw',$4,$5,$6,$7,$8,$9,$10,'stale','dependency_changed',TRUE,TRUE,$11,'validation_delta')`, lease.RunID, lease.InitEpoch, source.source.ID,
		source.source.Identity.DeviceID, source.provider, source.rootID, source.source.HeadManifestID, source.source.HeadGeneration, source.headReceipt, dependency[:], epoch)
	if err != nil {
		return err
	}
	key := encodeParityHeadDependency(source.source.ID, source.source.HeadManifestID, source.source.HeadGeneration, source.headReceipt)
	keyDigest := sha256.Sum256(key)
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_dependencies(run_id,init_epoch,source_id,kind,dependency_key,key_digest,expected,sequence,validation_epoch,observed)
		VALUES($1,$2,$3,'census_delta',$4,$5,$6,0,$7,$6)`, lease.RunID, lease.InitEpoch, source.source.ID, key, keyDigest[:], dependency[:], epoch)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MigrationParityStore) beginParityValidation(ctx context.Context, lease rawderive.ParityLease) (int64, error) {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseTx(ctx, tx, lease, true); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM `+s.quoted+`.migration_parity_members WHERE run_id=$1 AND init_epoch=$2 AND inventory_kind='validation_delta'`, lease.RunID, lease.InitEpoch); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM `+s.quoted+`.migration_parity_sources WHERE run_id=$1 AND init_epoch=$2 AND inventory_kind='validation_delta'`, lease.RunID, lease.InitEpoch); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM `+s.quoted+`.migration_parity_dependencies WHERE run_id=$1 AND init_epoch=$2 AND kind='census_delta'`, lease.RunID, lease.InitEpoch); err != nil {
		return 0, err
	}
	var epoch int64
	err = tx.QueryRowContext(ctx, `UPDATE `+s.quoted+`.migration_parity_runs SET validation_epoch=validation_epoch+1,validation_state='validating',state='validating',validation_digest=NULL,last_code='pending'
		WHERE run_id=$1 AND init_epoch=$2 AND request_generation=$3 AND lease_token=$4 AND binding_digest=$5 AND lease_owner=$6 AND lease_expires_at>clock_timestamp()
		RETURNING validation_epoch`, lease.RunID, lease.InitEpoch, lease.RequestGeneration, lease.Token, lease.BindingDigest[:], lease.Owner).Scan(&epoch)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("binding_conflict: parity validation lost lease")
	}
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_dependencies SET observed=NULL,validation_epoch=$3
		WHERE run_id=$1 AND init_epoch=$2 AND kind IN ('head','member')`, lease.RunID, lease.InitEpoch, epoch); err != nil {
		return 0, err
	}
	return epoch, tx.Commit()
}

func (s *MigrationParityStore) stageParitySourceObservation(ctx context.Context, lease rawderive.ParityLease, epoch int64, source parityCapturedSource) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseTx(ctx, tx, lease, true); err != nil {
		return err
	}
	var expected []byte
	err = tx.QueryRowContext(ctx, `SELECT dependency_digest FROM `+s.quoted+`.migration_parity_sources WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND source_kind='raw'`, lease.RunID, lease.InitEpoch, source.source.ID).Scan(&expected)
	if err == sql.ErrNoRows {
		_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_generation,head_receipt,dependency_digest,verdict,code,candidate_complete,required,validation_epoch,inventory_kind)
			VALUES($1,$2,$3,'raw',$4,$5,$6,$7,$8,$9,$10,'stale','dependency_changed',TRUE,TRUE,$11,'validation_delta')`, lease.RunID, lease.InitEpoch, source.source.ID,
			source.source.Identity.DeviceID, string(lease.Request.Cohort.Provider), source.rootID, source.source.HeadManifestID, source.source.HeadGeneration, source.headReceipt, source.source.DependencyDigest[:], epoch)
		if err != nil {
			return err
		}
		key := encodeParityHeadDependency(source.source.ID, source.source.HeadManifestID, source.source.HeadGeneration, source.headReceipt)
		keyDigest := sha256.Sum256(key)
		_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_dependencies(run_id,init_epoch,source_id,kind,dependency_key,key_digest,expected,sequence,validation_epoch,observed)
			VALUES($1,$2,$3,'census_delta',$4,$5,$6,0,$7,$6)`, lease.RunID, lease.InitEpoch, source.source.ID, key, keyDigest[:], source.source.DependencyDigest[:], epoch)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_dependencies SET observed=$4,validation_epoch=$5
		WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND kind='head'`, lease.RunID, lease.InitEpoch, source.source.ID, source.source.DependencyDigest[:], epoch)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MigrationParityStore) stageParityMemberObservations(ctx context.Context, lease rawderive.ParityLease, epoch int64, sourceID string, members []parityCapturedMember) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseTx(ctx, tx, lease, true); err != nil {
		return err
	}
	for _, member := range members {
		key, keyDigest := encodeParityMemberKey(member.key)
		observed := parityCapturedMemberDependency(member)
		result, updateErr := tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_dependencies SET observed=$5,validation_epoch=$6
			WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND kind='member' AND key_digest=$4 AND dependency_key=$7`,
			lease.RunID, lease.InitEpoch, sourceID, keyDigest[:], observed[:], epoch, key)
		if updateErr != nil {
			return updateErr
		}
		affected, _ := result.RowsAffected()
		if affected == 1 {
			continue
		}
		var existingKey []byte
		var inventoryKind string
		lookupErr := tx.QueryRowContext(ctx, `SELECT member_key,inventory_kind FROM `+s.quoted+`.migration_parity_members
			WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND member_digest=$4`, lease.RunID, lease.InitEpoch, sourceID, keyDigest[:]).Scan(&existingKey, &inventoryKind)
		if lookupErr == nil {
			if string(existingKey) != string(key) {
				return fmt.Errorf("binding_conflict: parity member digest collision")
			}
			if inventoryKind != "candidate_only" {
				return fmt.Errorf("invalid migration parity member inventory")
			}
			_, updateErr = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_dependencies(run_id,init_epoch,source_id,kind,dependency_key,key_digest,expected,validation_epoch,observed)
				VALUES($1,$2,$3,'census_delta',$4,$5,$6,$7,$6)
				ON CONFLICT(tenant_id,run_id,init_epoch,source_id,kind,key_digest) DO UPDATE SET dependency_key=EXCLUDED.dependency_key,expected=EXCLUDED.expected,observed=EXCLUDED.observed,validation_epoch=EXCLUDED.validation_epoch`,
				lease.RunID, lease.InitEpoch, sourceID, key, keyDigest[:], observed[:], epoch)
			if updateErr != nil {
				return updateErr
			}
			continue
		}
		if lookupErr != sql.ErrNoRows {
			return lookupErr
		}
		_, updateErr = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_members(run_id,init_epoch,source_id,member_key,member_digest,kind,required,mapping_state,physical_fingerprint,verdict,validation_epoch,inventory_kind)
			VALUES($1,$2,$3,$4,$5,$6,TRUE,$7,$8,'stale',$9,'validation_delta')`, lease.RunID, lease.InitEpoch, sourceID, key, keyDigest[:], member.key.Kind,
			member.mappingState, parityMemberPhysicalValue(member), epoch)
		if updateErr != nil {
			return updateErr
		}
	}
	return tx.Commit()
}

func parityMemberPhysicalValue(member parityCapturedMember) any {
	if member.physical == nil {
		return nil
	}
	return member.physical[:]
}

func (s *MigrationParityStore) stageLegacyParityValidation(ctx context.Context, lease rawderive.ParityLease, epoch int64, baselineTx *sql.Tx, binding rawderive.ParityBinding, pageSize int, validationHash hash.Hash) error {
	// Legacy rows are provider-wide and intentionally unresolved. Reusing their
	// sealed member keys prevents them from being assigned to every raw source;
	// absence remains visible because their member dependencies stay unobserved.
	after := ""
	for {
		rows, err := baselineTx.QueryContext(ctx, `SELECT id,COALESCE(NULLIF(source_session_id,''),id),
			EXISTS(SELECT 1 FROM raw_session_public_aliases WHERE alias_id IN (sessions.id,sessions.source_session_id))
			FROM sessions WHERE provenance_kind='legacy' AND agent=$1 AND id>$2 ORDER BY id LIMIT $3`, string(binding.Request.Cohort.Provider), after, pageSize)
		if err != nil {
			return err
		}
		var members []parityCapturedMember
		for rows.Next() {
			var physicalID, logical string
			var collision bool
			if err = rows.Scan(&physicalID, &logical, &collision); err != nil {
				rows.Close()
				return err
			}
			sourceDigest := sha256.Sum256([]byte("agentsview-parity-legacy-source-v1\x00" + physicalID))
			sourceID := fmt.Sprintf("legacy:%x", sourceDigest[:])
			mapping := "legacy_only"
			if collision {
				mapping = "ambiguous"
			}
			members = append(members, parityCapturedMember{key: rawderive.ParityMemberKey{SourceID: sourceID, LogicalKey: logical, Kind: "legacy"}, physicalID: physicalID, mappingState: mapping})
			after = physicalID
		}
		scanErr := rows.Err()
		rows.Close()
		if scanErr != nil {
			return scanErr
		}
		for i := range members {
			if err = s.stageParityLegacySourceObservation(ctx, lease, epoch, members[i], string(binding.Request.Cohort.Provider)); err != nil {
				return err
			}
			physical, readErr := readParityPhysicalMember(ctx, baselineTx, binding, members[i].physicalID, members[i].key)
			if readErr != nil {
				return readErr
			}
			if physical.InvalidCode == "" {
				members[i].physical = &physical.Physical
			}
			key, _ := encodeParityMemberKey(members[i].key)
			dependency := parityCapturedMemberDependency(members[i])
			parityHashField(validationHash, key)
			parityHashField(validationHash, dependency[:])
			if err = s.stageParityMemberObservations(ctx, lease, epoch, members[i].key.SourceID, []parityCapturedMember{members[i]}); err != nil {
				return err
			}
		}
		if len(members) < pageSize {
			return nil
		}
	}
}

func (s *MigrationParityStore) stageParityLegacySourceObservation(ctx context.Context, lease rawderive.ParityLease, epoch int64, member parityCapturedMember, provider string) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseTx(ctx, tx, lease, true); err != nil {
		return err
	}
	dependency := sha256.Sum256([]byte(member.physicalID))
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_sources(run_id,init_epoch,source_id,source_kind,device_id,provider,root_id,head_manifest,head_generation,head_receipt,dependency_digest,verdict,code,candidate_complete,required,validation_epoch,inventory_kind)
		VALUES($1,$2,$3,'legacy','',$4,'','',0,'',$5,'stale','dependency_changed',TRUE,TRUE,$6,'validation_delta')
		ON CONFLICT(tenant_id,run_id,init_epoch,source_id) DO NOTHING`, lease.RunID, lease.InitEpoch, member.key.SourceID, provider, dependency[:], epoch)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *MigrationParityStore) finishParityValidation(ctx context.Context, lease rawderive.ParityLease, epoch int64, digest rawderive.ParityDigest, baselineObserved, runtimeObserved time.Time) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseTx(ctx, tx, lease, true); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_members m SET verdict='stale',different=0,validation_epoch=$3
		WHERE m.run_id=$1 AND m.init_epoch=$2 AND m.inventory_kind='baseline' AND EXISTS(
		 SELECT 1 FROM `+s.quoted+`.migration_parity_dependencies d WHERE d.run_id=m.run_id AND d.init_epoch=m.init_epoch
		 AND d.source_id=m.source_id AND d.kind='member' AND d.key_digest=m.member_digest AND d.validation_epoch=$3
		 AND (d.observed IS NULL OR d.observed<>d.expected))`, lease.RunID, lease.InitEpoch, epoch)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_members m SET verdict='stale',different=0,validation_epoch=$3
		WHERE m.run_id=$1 AND m.init_epoch=$2 AND m.inventory_kind='candidate_only' AND EXISTS(
		 SELECT 1 FROM `+s.quoted+`.migration_parity_dependencies d WHERE d.run_id=m.run_id AND d.init_epoch=m.init_epoch
		 AND d.source_id=m.source_id AND d.kind='census_delta' AND d.key_digest=m.member_digest AND d.validation_epoch=$3
		 AND d.dependency_key=m.member_key)`, lease.RunID, lease.InitEpoch, epoch)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_sources src SET verdict='stale',code='dependency_changed'
		WHERE src.run_id=$1 AND src.init_epoch=$2 AND (
		 EXISTS(SELECT 1 FROM `+s.quoted+`.migration_parity_dependencies d WHERE d.run_id=src.run_id AND d.init_epoch=src.init_epoch AND d.source_id=src.source_id
		  AND d.validation_epoch=$3 AND ((d.kind IN ('head','member') AND (d.observed IS NULL OR d.observed<>d.expected)) OR d.kind='census_delta'))
		 OR EXISTS(SELECT 1 FROM `+s.quoted+`.migration_parity_members m WHERE m.run_id=src.run_id AND m.init_epoch=src.init_epoch AND m.source_id=src.source_id AND m.inventory_kind='validation_delta' AND m.validation_epoch=$3))`, lease.RunID, lease.InitEpoch, epoch)
	if err != nil {
		return err
	}
	var stale bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.quoted+`.migration_parity_sources WHERE run_id=$1 AND init_epoch=$2 AND required AND verdict='stale')`, lease.RunID, lease.InitEpoch).Scan(&stale); err != nil {
		return err
	}
	state := "checked"
	if stale {
		state = "stale"
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_runs SET validation_state=$1,validation_digest=$2,baseline_observed_at=$3,runtime_observed_at=$4,state='running',last_code=$5
		WHERE run_id=$6 AND init_epoch=$7 AND validation_epoch=$8 AND request_generation=$9 AND lease_token=$10 AND lease_owner=$11 AND binding_digest=$12 AND lease_expires_at>clock_timestamp()`,
		state, digest[:], baselineObserved, runtimeObserved, map[bool]string{true: "dependency_changed", false: "pending"}[stale], lease.RunID, lease.InitEpoch, epoch,
		lease.RequestGeneration, lease.Token, lease.Owner, lease.BindingDigest[:])
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return fmt.Errorf("binding_conflict: parity validation lost lease")
	}
	return tx.Commit()
}
