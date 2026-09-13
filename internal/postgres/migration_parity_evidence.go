package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/agentsview/internal/rawderive"
)

const parityFingerprintSize = 9 * sha256.Size

func putParityOpaqueString(buf *bytes.Buffer, value string) {
	_ = binary.Write(buf, binary.BigEndian, uint32(len(value)))
	_, _ = buf.WriteString(value)
}

func readParityOpaqueString(reader *bytes.Reader) (string, error) {
	var size uint32
	if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
		return "", err
	}
	if uint64(size) > uint64(reader.Len()) {
		return "", io.ErrUnexpectedEOF
	}
	value := make([]byte, int(size))
	_, err := io.ReadFull(reader, value)
	return string(value), err
}

func encodeParityMemberKey(key rawderive.ParityMemberKey) ([]byte, rawderive.ParityDigest) {
	var buf bytes.Buffer
	_, _ = buf.WriteString("agentsview-parity-member-key-v1\x00")
	putParityOpaqueString(&buf, key.SourceID)
	putParityOpaqueString(&buf, key.LogicalKey)
	putParityOpaqueString(&buf, key.Kind)
	encoded := buf.Bytes()
	return encoded, rawderive.ParityDigest(sha256.Sum256(encoded))
}

func encodeParityFingerprint(fingerprint rawderive.ParityFingerprint) []byte {
	encoded := make([]byte, 0, parityFingerprintSize)
	for _, digest := range []rawderive.ParityDigest{
		fingerprint.Session, fingerprint.Messages, fingerprint.Tools,
		fingerprint.Usage, fingerprint.Signals, fingerprint.Findings,
		fingerprint.Links, fingerprint.Exclusions, fingerprint.Semantic,
	} {
		encoded = append(encoded, digest[:]...)
	}
	return encoded
}

func decodeParityFingerprint(encoded []byte) (rawderive.ParityFingerprint, error) {
	if len(encoded) != parityFingerprintSize {
		return rawderive.ParityFingerprint{}, fmt.Errorf("invalid parity fingerprint")
	}
	var values [9]rawderive.ParityDigest
	for i := range values {
		copy(values[i][:], encoded[i*sha256.Size:(i+1)*sha256.Size])
	}
	return rawderive.ParityFingerprint{
		Session: values[0], Messages: values[1], Tools: values[2], Usage: values[3],
		Signals: values[4], Findings: values[5], Links: values[6], Exclusions: values[7], Semantic: values[8],
	}, nil
}

func validParityVerdict(verdict rawderive.ParityVerdict) bool {
	switch verdict {
	case rawderive.ParityMatched, rawderive.ParityMismatched, rawderive.ParityAmbiguous,
		rawderive.ParityLegacyOnly, rawderive.ParityMissing, rawderive.ParityPartial, rawderive.ParityStale:
		return true
	default:
		return false
	}
}

func parityVerdictRank(verdict rawderive.ParityVerdict) int {
	switch verdict {
	case rawderive.ParityStale:
		return 7
	case rawderive.ParityPartial:
		return 6
	case rawderive.ParityAmbiguous:
		return 5
	case rawderive.ParityMissing:
		return 4
	case rawderive.ParityMismatched:
		return 3
	case rawderive.ParityLegacyOnly:
		return 2
	case rawderive.ParityMatched:
		return 1
	default:
		return 0
	}
}

func strongerParityVerdict(left, right rawderive.ParityVerdict) rawderive.ParityVerdict {
	if parityVerdictRank(right) > parityVerdictRank(left) {
		return right
	}
	return left
}

func (s *MigrationParityStore) checkParityLeaseTx(ctx context.Context, tx *sql.Tx, lease rawderive.ParityLease, requireSealed bool) error {
	return s.checkParityLeaseStateTx(ctx, tx, lease, requireSealed, true)
}

// ReserveParitySource durably consumes one source attempt before any retained
// bytes are materialized. A crashed owner cannot spend that attempt again in
// the same request generation; an explicit resume advances the generation.
func (s *MigrationParityStore) ReserveParitySource(ctx context.Context, lease rawderive.ParityLease, source rawderive.ParitySource) error {
	if source.ID == "" || lease.RunID == "" || lease.Token == "" {
		return fmt.Errorf("invalid migration parity source reservation")
	}
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseTx(ctx, tx, lease, true); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_sources
		SET evidence_generation=$4
		WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND evidence_generation<$4
		AND dependency_digest=$5`, lease.RunID, lease.InitEpoch, source.ID, lease.RequestGeneration, source.DependencyDigest[:])
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("binding_conflict: parity source attempt is stale")
	}
	result, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_runs
		SET budget_sources=budget_sources-1
		WHERE run_id=$1 AND init_epoch=$2 AND request_generation=$3 AND lease_token=$4 AND lease_owner=$5
		AND lease_expires_at>clock_timestamp() AND budget_sources>0`, lease.RunID, lease.InitEpoch,
		lease.RequestGeneration, lease.Token, lease.Owner)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("binding_conflict: parity request budget exhausted")
	}
	return tx.Commit()
}

func (s *MigrationParityStore) checkParityLeaseReadTx(ctx context.Context, tx *sql.Tx, lease rawderive.ParityLease, requireSealed bool) error {
	return s.checkParityLeaseStateTx(ctx, tx, lease, requireSealed, false)
}

func (s *MigrationParityStore) checkParityLeaseStateTx(ctx context.Context, tx *sql.Tx, lease rawderive.ParityLease, requireSealed, lock bool) error {
	var exists bool
	query := `SELECT true FROM ` + s.quoted + `.migration_parity_runs
		WHERE run_id=$1 AND init_epoch=$2 AND request_generation=$3 AND lease_token=$4 AND lease_owner=$5
		AND lease_expires_at>clock_timestamp() AND binding_digest=$6`
	if requireSealed {
		query += ` AND baseline_sealed`
	}
	if lock {
		query += ` FOR UPDATE`
	}
	if err := tx.QueryRowContext(ctx, query, lease.RunID, lease.InitEpoch, lease.RequestGeneration, lease.Token, lease.Owner, lease.BindingDigest[:]).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("binding_conflict: parity lease is stale")
	} else if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("binding_conflict: parity lease is stale")
	}
	return nil
}

// RecordParitySource replaces one source's candidate evidence transactionally.
// Counts are always reduced from rows, so a completed retry cannot duplicate
// either candidate-only members or aggregate counts.
func (s *MigrationParityStore) RecordParitySource(ctx context.Context, lease rawderive.ParityLease, source rawderive.ParitySource, result rawderive.ParitySourceResult) error {
	if source.ID == "" || lease.RunID == "" || lease.Token == "" {
		return fmt.Errorf("invalid migration parity source result")
	}
	if result.Blocker != "" && !validParityVerdict(result.Blocker) {
		return fmt.Errorf("invalid migration parity source blocker")
	}
	tx, err := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = s.checkParityLeaseTx(ctx, tx, lease, true); err != nil {
		return err
	}
	var storedDependency []byte
	var storedVerdict sql.NullString
	var capturedVerdict sql.NullString
	var storedCode, capturedCode, sourceKeySHA256 string
	var evidenceGeneration int64
	if err = tx.QueryRowContext(ctx, `SELECT dependency_digest,verdict,code,captured_verdict,captured_code,source_key_sha256,evidence_generation FROM `+s.quoted+`.migration_parity_sources WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 FOR UPDATE`, lease.RunID, lease.InitEpoch, source.ID).Scan(&storedDependency, &storedVerdict, &storedCode, &capturedVerdict, &capturedCode, &sourceKeySHA256, &evidenceGeneration); err != nil {
		return err
	}
	if evidenceGeneration != lease.RequestGeneration {
		return fmt.Errorf("binding_conflict: parity source result has no current reservation")
	}
	if source.DependencyDigest == (rawderive.ParityDigest{}) || !bytes.Equal(storedDependency, source.DependencyDigest[:]) {
		return fmt.Errorf("dependency_changed: parity source changed after capture")
	}
	current, found, currentErr := readCurrentParitySource(ctx, tx, lease, source.ID, sourceKeySHA256)
	if currentErr != nil {
		return currentErr
	}
	if !found || !bytes.Equal(storedDependency, current.source.DependencyDigest[:]) {
		if _, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_sources SET verdict='stale',code='dependency_changed' WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3`, lease.RunID, lease.InitEpoch, source.ID); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return fmt.Errorf("dependency_changed: parity runtime source changed after capture")
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM `+s.quoted+`.migration_parity_members WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND inventory_kind='candidate_only'`, lease.RunID, lease.InitEpoch, source.ID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_members SET candidate_fingerprint=NULL,
		verdict=CASE WHEN verdict='stale' THEN 'stale' ELSE captured_verdict END,different=0
		WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND inventory_kind='baseline'`, lease.RunID, lease.InitEpoch, source.ID); err != nil {
		return err
	}

	for _, member := range result.Members {
		if member.Key.SourceID != source.ID || member.Key.LogicalKey == "" || member.Key.Kind == "" || member.Different > 255 {
			return fmt.Errorf("invalid migration parity member result")
		}
		if member.Verdict != "" && !validParityVerdict(member.Verdict) {
			return fmt.Errorf("invalid migration parity member verdict")
		}
		encodedKey, keyDigest := encodeParityMemberKey(member.Key)
		var baselineKey, baselineFingerprint []byte
		var immutableVerdict sql.NullString
		lookupErr := tx.QueryRowContext(ctx, `SELECT member_key,baseline_fingerprint,verdict FROM `+s.quoted+`.migration_parity_members
			WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND member_digest=$4 AND inventory_kind='baseline'`,
			lease.RunID, lease.InitEpoch, source.ID, keyDigest[:]).Scan(&baselineKey, &baselineFingerprint, &immutableVerdict)
		if lookupErr == nil && !bytes.Equal(baselineKey, encodedKey) {
			return fmt.Errorf("binding_conflict: parity member digest collision")
		}
		if lookupErr != nil && lookupErr != sql.ErrNoRows {
			return lookupErr
		}
		if lookupErr == sql.ErrNoRows {
			candidate := []byte(nil)
			if member.Fingerprint != nil {
				candidate = encodeParityFingerprint(*member.Fingerprint)
			}
			verdict := strongerParityVerdict(rawderive.ParityMissing, member.Verdict)
			_, err = tx.ExecContext(ctx, `INSERT INTO `+s.quoted+`.migration_parity_members(run_id,init_epoch,source_id,member_key,member_digest,kind,required,mapping_state,candidate_fingerprint,verdict,different,inventory_kind)
				VALUES($1,$2,$3,$4,$5,$6,TRUE,'missing',$7,$8,0,'candidate_only')`,
				lease.RunID, lease.InitEpoch, source.ID, encodedKey, keyDigest[:], member.Key.Kind, candidate, verdict)
			if err != nil {
				return err
			}
			continue
		}
		if len(baselineFingerprint) == 0 {
			continue
		}

		verdict := member.Verdict
		different := member.Different
		var candidate []byte
		if member.Fingerprint != nil {
			candidate = encodeParityFingerprint(*member.Fingerprint)
		}
		if verdict == "" {
			if member.Fingerprint == nil {
				return fmt.Errorf("invalid migration parity member result")
			}
			expected, decodeErr := decodeParityFingerprint(baselineFingerprint)
			if decodeErr != nil {
				return decodeErr
			}
			comparison := rawderive.CompareParity(expected, *member.Fingerprint)
			verdict, different = comparison.Verdict, comparison.Different
		}
		if immutableVerdict.Valid {
			verdict = strongerParityVerdict(verdict, rawderive.ParityVerdict(immutableVerdict.String))
		}
		_, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_members SET candidate_fingerprint=$5,verdict=$6,different=$7
			WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND member_digest=$4 AND member_key=$8`,
			lease.RunID, lease.InitEpoch, source.ID, keyDigest[:], candidate, verdict, different, encodedKey)
		if err != nil {
			return err
		}
	}
	if result.Complete {
		if _, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_members SET verdict='missing',different=0
			WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND inventory_kind='baseline' AND verdict IS NULL`, lease.RunID, lease.InitEpoch, source.ID); err != nil {
			return err
		}
	}

	verdict := result.Blocker
	if capturedVerdict.Valid {
		verdict = strongerParityVerdict(verdict, rawderive.ParityVerdict(capturedVerdict.String))
	}
	stickyStale := storedVerdict.Valid && rawderive.ParityVerdict(storedVerdict.String) == rawderive.ParityStale
	if stickyStale {
		verdict = strongerParityVerdict(verdict, rawderive.ParityStale)
	}
	rows, err := tx.QueryContext(ctx, `SELECT verdict FROM `+s.quoted+`.migration_parity_members WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND required AND verdict IS NOT NULL`, lease.RunID, lease.InitEpoch, source.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var memberVerdict rawderive.ParityVerdict
		if err = rows.Scan(&memberVerdict); err != nil {
			rows.Close()
			return err
		}
		verdict = strongerParityVerdict(verdict, memberVerdict)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if verdict == "" && result.Complete {
		verdict = rawderive.ParityMatched
	}
	code := result.Code
	if stickyStale {
		code = storedCode
	} else if capturedVerdict.Valid && parityVerdictRank(rawderive.ParityVerdict(capturedVerdict.String)) >= parityVerdictRank(result.Blocker) {
		code = capturedCode
	} else if code == "" {
		code = "pending"
	}
	_, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_sources SET verdict=$4,code=$5,candidate_complete=$6
		WHERE run_id=$1 AND init_epoch=$2 AND source_id=$3 AND evidence_generation=$7`, lease.RunID, lease.InitEpoch, source.ID, nullableParityVerdict(verdict), code, result.Complete, lease.RequestGeneration)
	if err != nil {
		return err
	}
	if err = s.checkParityLeaseTx(ctx, tx, lease, true); err != nil {
		return err
	}
	return tx.Commit()
}

func readCurrentParitySource(ctx context.Context, tx *sql.Tx, lease rawderive.ParityLease, sourceID, sourceKeySHA256 string) (parityCapturedSource, bool, error) {
	if sourceKeySHA256 == "" {
		return parityCapturedSource{}, false, nil
	}
	// The tenant is not part of the query predicate because the hosted
	// connection's tenant binding and forced RLS already scope every row.
	var tenant string
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('agentsview.tenant_id')`).Scan(&tenant); err != nil {
		return parityCapturedSource{}, false, err
	}
	row := tx.QueryRowContext(ctx, `SELECT h.device_id,h.provider,h.configured_root_id,h.source_key,h.source_key_sha256,
		COALESCE(h.manifest_id,''),h.generation,COALESCE(h.receipt,''),p.source_id,p.selected_manifest_id,p.processing_version,
		p.projection_generation,p.selected_job_id,p.successful_manifest_id,p.last_attempt_manifest_id,p.membership_complete,p.diagnostics
		FROM raw_source_heads h LEFT JOIN raw_source_projections p
		ON p.device_id=h.device_id AND p.provider=h.provider AND p.configured_root_id=h.configured_root_id AND p.source_key_sha256=h.source_key_sha256
		WHERE h.device_id=$1 AND h.provider=$2 AND h.configured_root_id=$3 AND h.source_key_sha256=$4 AND h.generation>0`,
		lease.Request.Cohort.DeviceID, string(lease.Request.Cohort.Provider), lease.Request.Cohort.RootID, sourceKeySHA256)
	current, err := scanParityCapturedSource(row, tenant)
	if errors.Is(err, sql.ErrNoRows) {
		return parityCapturedSource{}, false, nil
	}
	if err != nil {
		return parityCapturedSource{}, false, err
	}
	if current.source.ID != sourceID {
		return parityCapturedSource{}, false, nil
	}
	var historyCode string
	current.manifestRows, historyCode, err = readParityReceiptChain(ctx, tx, current)
	if err != nil {
		return parityCapturedSource{}, false, err
	}
	if current.partialCode == "" || historyCode == "limit_exceeded" {
		current.partialCode = historyCode
	}
	current.runtimeDependency, historyCode, err = readParityRuntimeDependency(ctx, tx, current)
	if err != nil {
		return parityCapturedSource{}, false, err
	}
	if current.partialCode == "" || historyCode == "limit_exceeded" {
		current.partialCode = historyCode
	}
	current.source.DependencyDigest = paritySourceDependency(current)
	var locked int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM raw_source_heads WHERE device_id=$1 AND provider=$2 AND configured_root_id=$3 AND source_key_sha256=$4 FOR SHARE`,
		current.source.Identity.DeviceID, current.provider, current.rootID, current.keySHA256).Scan(&locked); err != nil {
		return parityCapturedSource{}, false, err
	}
	return current, true, nil
}

func nullableParityVerdict(verdict rawderive.ParityVerdict) any {
	if verdict == "" {
		return nil
	}
	return string(verdict)
}
