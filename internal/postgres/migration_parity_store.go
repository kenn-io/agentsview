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
	"time"

	"github.com/google/uuid"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

// MigrationParityOptions identifies one permanently hosted evidence schema.
type MigrationParityOptions struct {
	Schema string
	Tenant string
}

// MigrationParityStore owns only migration evidence writes. Baseline corpus
// access is always supplied separately through a read-only transaction.
type MigrationParityStore struct {
	pg                     *sql.DB
	schema                 string
	tenant                 string
	quoted                 string
	parityEvidencePageHook func(context.Context, string) error
	parityCensusPageHook   func(string, int)
}

func NewMigrationParityStore(ctx context.Context, database *sql.DB, options MigrationParityOptions) (*MigrationParityStore, error) {
	if database == nil {
		return nil, fmt.Errorf("migration parity requires a database")
	}
	if err := validateHostedBinding(options.Schema, options.Tenant); err != nil {
		return nil, err
	}
	if err := CheckHostedTenant(ctx, database, options.Schema, options.Tenant); err != nil {
		return nil, err
	}
	if err := checkMigrationParityCatalog(ctx, database, options.Schema); err != nil {
		return nil, err
	}
	if err := checkMigrationParityRuntimePrivileges(ctx, database, options.Schema); err != nil {
		return nil, err
	}
	quoted, _ := quoteIdentifier(options.Schema)
	return &MigrationParityStore{pg: database, schema: options.Schema, tenant: options.Tenant, quoted: quoted}, nil
}

func validateParityUUID(value, field string) error {
	id, err := uuid.Parse(value)
	if err != nil || id.String() != value {
		return fmt.Errorf("invalid migration parity %s", field)
	}
	return nil
}

func validateParityRequest(request rawderive.ParityRequest) error {
	if err := validateParityUUID(request.RunID, "run ID"); err != nil {
		return err
	}
	if err := validateParityUUID(request.RuntimeID, "runtime identity"); err != nil {
		return err
	}
	if request.BaselineProfile == "" {
		return fmt.Errorf("invalid migration parity baseline profile")
	}
	if _, err := rawsync.NewAuthIdentity("parity-validation", request.Cohort.DeviceID); err != nil {
		return fmt.Errorf("invalid migration parity device")
	}
	if _, err := rawsync.NewAuthIdentity("parity-validation", request.Cohort.RootID); err != nil {
		return fmt.Errorf("invalid migration parity root")
	}
	def, ok := parser.AgentByType(request.Cohort.Provider)
	if !ok || def.RemoteSyncExcluded {
		return fmt.Errorf("invalid migration parity provider")
	}
	return nil
}

func parityPutString(buf *bytes.Buffer, value string) {
	_ = binary.Write(buf, binary.BigEndian, uint32(len(value)))
	_, _ = buf.WriteString(value)
}

func encodeParityRequest(request rawderive.ParityRequest) ([]byte, rawderive.ParityDigest, error) {
	if err := validateParityRequest(request); err != nil {
		return nil, rawderive.ParityDigest{}, err
	}
	var buf bytes.Buffer
	_, _ = buf.WriteString("agentsview-parity-request\x00")
	_ = binary.Write(&buf, binary.BigEndian, uint32(rawderive.ParitySchemaVersion))
	parityPutString(&buf, request.RunID)
	parityPutString(&buf, request.RuntimeID)
	parityPutString(&buf, request.BaselineProfile)
	parityPutString(&buf, request.Cohort.DeviceID)
	parityPutString(&buf, string(request.Cohort.Provider))
	parityPutString(&buf, request.Cohort.RootID)
	b := buf.Bytes()
	return append([]byte(nil), b...), rawderive.ParityDigest(sha256.Sum256(b)), nil
}

func parityReadString(r *bytes.Reader) (string, error) {
	var size uint32
	if err := binary.Read(r, binary.BigEndian, &size); err != nil {
		return "", err
	}
	if uint64(size) > uint64(r.Len()) {
		return "", io.ErrUnexpectedEOF
	}
	b := make([]byte, int(size))
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeParityRequest(data []byte) (rawderive.ParityRequest, error) {
	const prefix = "agentsview-parity-request\x00"
	if len(data) < len(prefix)+4 || string(data[:len(prefix)]) != prefix {
		return rawderive.ParityRequest{}, fmt.Errorf("invalid migration parity request encoding")
	}
	r := bytes.NewReader(data[len(prefix):])
	var version uint32
	if err := binary.Read(r, binary.BigEndian, &version); err != nil || version != rawderive.ParitySchemaVersion {
		return rawderive.ParityRequest{}, fmt.Errorf("invalid migration parity request encoding")
	}
	values := make([]string, 6)
	for i := range values {
		var err error
		values[i], err = parityReadString(r)
		if err != nil {
			return rawderive.ParityRequest{}, fmt.Errorf("invalid migration parity request encoding")
		}
	}
	if r.Len() != 0 {
		return rawderive.ParityRequest{}, fmt.Errorf("invalid migration parity request encoding")
	}
	request := rawderive.ParityRequest{RunID: values[0], RuntimeID: values[1], BaselineProfile: values[2], Cohort: rawderive.ParityCohort{DeviceID: values[3], Provider: parser.AgentType(values[4]), RootID: values[5]}}
	return request, validateParityRequest(request)
}

func encodeParityBinding(binding rawderive.ParityBinding) ([]byte, rawderive.ParityDigest, error) {
	digest, err := rawderive.DigestParityBinding(binding)
	if err != nil {
		return nil, rawderive.ParityDigest{}, err
	}
	request, _, err := encodeParityRequest(binding.Request)
	if err != nil {
		return nil, rawderive.ParityDigest{}, err
	}
	if err := validateParityUUID(binding.BaselineID, "baseline identity"); err != nil {
		return nil, rawderive.ParityDigest{}, err
	}
	var buf bytes.Buffer
	_, _ = buf.WriteString("agentsview-parity-binding\x00")
	_ = binary.Write(&buf, binary.BigEndian, uint32(rawderive.ParitySchemaVersion))
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(request)))
	_, _ = buf.Write(request)
	parityPutString(&buf, binding.BaselineID)
	_, _ = buf.Write(binding.BaselineConfig[:])
	parityPutString(&buf, binding.Tenant)
	_, _ = buf.Write(binding.Versions.ParserBuild[:])
	_ = binary.Write(&buf, binary.BigEndian, int64(binding.Versions.Data))
	parityPutString(&buf, binding.Versions.Preparation)
	parityPutString(&buf, binding.Versions.Projection)
	_ = binary.Write(&buf, binary.BigEndian, int64(binding.Versions.Comparison))
	_ = binary.Write(&buf, binary.BigEndian, int64(binding.Versions.Quality))
	parityPutString(&buf, binding.Versions.SecretRules)
	_, _ = buf.Write(binding.Versions.Policy[:])
	parityPutString(&buf, binding.ObservedAt.UTC().Format(time.RFC3339Nano))
	return buf.Bytes(), digest, nil
}

func (s *MigrationParityStore) CreateOrResumeParity(ctx context.Context, request rawderive.ParityRequest, batch int) (rawderive.ParityReport, error) {
	if batch < 1 || batch > 128 {
		return rawderive.ParityReport{}, fmt.Errorf("invalid migration parity batch size")
	}
	encoded, digest, err := encodeParityRequest(request)
	if err != nil {
		return rawderive.ParityReport{}, err
	}
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return rawderive.ParityReport{}, err
	}
	defer func() { _ = tx.Rollback() }()
	query := `INSERT INTO ` + s.quoted + `.migration_parity_runs(run_id,request,request_digest,batch_size,budget_sources) VALUES($1,$2,$3,$4,$4) ON CONFLICT(tenant_id,run_id) DO NOTHING`
	if _, err = tx.ExecContext(ctx, query, request.RunID, encoded, digest[:], batch); err != nil {
		return rawderive.ParityReport{}, err
	}
	var storedRequest, storedDigest []byte
	var state string
	var generation int64
	if err = tx.QueryRowContext(ctx, `SELECT request,request_digest,state,request_generation FROM `+s.quoted+`.migration_parity_runs WHERE run_id=$1 FOR UPDATE`, request.RunID).Scan(&storedRequest, &storedDigest, &state, &generation); err != nil {
		return rawderive.ParityReport{}, err
	}
	if !bytes.Equal(storedRequest, encoded) || !bytes.Equal(storedDigest, digest[:]) {
		return rawderive.ParityReport{}, fmt.Errorf("binding_conflict: run ID is already bound to another request")
	}
	if state == "complete" {
		generation++
		_, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_runs SET state='requested',batch_size=$2,budget_sources=$2,request_generation=$3,validation_state='unchecked',last_code='pending',lease_owner='',lease_token=NULL,lease_expires_at=NULL WHERE run_id=$1`, request.RunID, batch, generation)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_runs SET batch_size=$2,budget_sources=LEAST(budget_sources,$2) WHERE run_id=$1`, request.RunID, batch)
	}
	if err != nil {
		return rawderive.ParityReport{}, err
	}
	report, err := s.readParityReportTx(ctx, tx, request.RunID, nil)
	if err != nil {
		return rawderive.ParityReport{}, err
	}
	if err = tx.Commit(); err != nil {
		return rawderive.ParityReport{}, err
	}
	return report, nil
}

func (s *MigrationParityStore) ReadParityReport(ctx context.Context, runID string) (rawderive.ParityReport, error) {
	if err := validateParityUUID(runID, "run ID"); err != nil {
		return rawderive.ParityReport{}, err
	}
	tx, err := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return rawderive.ParityReport{}, err
	}
	defer func() { _ = tx.Rollback() }()
	report, err := s.readParityReportTx(ctx, tx, runID, nil)
	if err != nil {
		return rawderive.ParityReport{}, err
	}
	if report.State == "complete" {
		report.Passing = false
		report.Freshness = "unchecked"
		report.Code = "historical_evidence"
	}
	return report, tx.Commit()
}

func (s *MigrationParityStore) ReadParityRequestReport(ctx context.Context, runID string, requestGeneration int64) (rawderive.ParityReport, error) {
	if err := validateParityUUID(runID, "run ID"); err != nil {
		return rawderive.ParityReport{}, err
	}
	if requestGeneration <= 0 {
		return rawderive.ParityReport{}, fmt.Errorf("invalid migration parity request generation")
	}
	tx, err := s.pg.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return rawderive.ParityReport{}, err
	}
	defer func() { _ = tx.Rollback() }()
	report, err := s.readParityReportTx(ctx, tx, runID, &requestGeneration)
	if err != nil {
		return rawderive.ParityReport{}, err
	}
	return report, tx.Commit()
}

func (s *MigrationParityStore) readParityReportTx(ctx context.Context, tx *sql.Tx, runID string, requested *int64) (rawderive.ParityReport, error) {
	var report rawderive.ParityReport
	var validation, code string
	err := tx.QueryRowContext(ctx, `SELECT run_id::text,state,last_code,baseline_sealed,request_generation,completed_generation,validation_state,baseline_observed_at,runtime_observed_at FROM `+s.quoted+`.migration_parity_runs WHERE run_id=$1`, runID).Scan(&report.RunID, &report.State, &code, &report.BaselineSealed, &report.RequestGeneration, &report.CompletedGeneration, &validation, &report.BaselineObservedAt, &report.RuntimeObservedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return rawderive.ParityReport{}, fmt.Errorf("migration parity run not found")
	}
	if err != nil {
		return rawderive.ParityReport{}, err
	}
	report.Code = code
	report.Freshness = validation
	if requested != nil && *requested != report.RequestGeneration {
		report.Code = "generation_not_available"
		report.Freshness = "unchecked"
		return report, nil
	}

	loadCounts := func(table string, counts *rawderive.ParityCounts) error {
		query := `SELECT verdict,count(*) FROM ` + s.quoted + `.` + table + ` WHERE run_id=$1 AND init_epoch=(SELECT init_epoch FROM ` + s.quoted + `.migration_parity_runs WHERE run_id=$1) AND required GROUP BY verdict`
		switch table {
		case "migration_parity_sources":
			query = `SELECT src.verdict,count(*) FROM ` + s.quoted + `.migration_parity_sources src
				JOIN ` + s.quoted + `.migration_parity_runs r ON r.run_id=src.run_id AND r.init_epoch=src.init_epoch
				WHERE src.run_id=$1 AND src.required AND (src.inventory_kind<>'validation_delta' OR
				(src.validation_epoch=r.validation_epoch AND r.validation_state IN ('checked','stale')))
				GROUP BY src.verdict`
		case "migration_parity_members":
			query = `SELECT m.verdict,count(*) FROM ` + s.quoted + `.migration_parity_members m
				JOIN ` + s.quoted + `.migration_parity_runs r ON r.run_id=m.run_id AND r.init_epoch=m.init_epoch
				WHERE m.run_id=$1 AND m.required AND (m.inventory_kind<>'validation_delta' OR
				(m.validation_epoch=r.validation_epoch AND r.validation_state IN ('checked','stale')))
				GROUP BY m.verdict`
		}
		rows, err := tx.QueryContext(ctx, query, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var verdict sql.NullString
			var n int64
			if err := rows.Scan(&verdict, &n); err != nil {
				return err
			}
			if !verdict.Valid {
				continue
			}
			switch rawderive.ParityVerdict(verdict.String) {
			case rawderive.ParityMatched:
				counts.Matched += n
			case rawderive.ParityMismatched:
				counts.Mismatched += n
			case rawderive.ParityAmbiguous:
				counts.Ambiguous += n
			case rawderive.ParityLegacyOnly:
				counts.LegacyOnly += n
			case rawderive.ParityMissing:
				counts.Missing += n
			case rawderive.ParityPartial:
				counts.PartialUnsupported += n
			case rawderive.ParityStale:
				counts.Stale += n
			}
		}
		return rows.Err()
	}
	if err := loadCounts("migration_parity_members", &report.Members); err != nil {
		return rawderive.ParityReport{}, err
	}
	if err := loadCounts("migration_parity_sources", &report.Sources); err != nil {
		return rawderive.ParityReport{}, err
	}
	var pendingMembers int64
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+s.quoted+`.migration_parity_members m
		JOIN `+s.quoted+`.migration_parity_runs r ON r.run_id=m.run_id AND r.init_epoch=m.init_epoch
		WHERE m.run_id=$1 AND m.required AND m.verdict IS NULL AND (m.inventory_kind<>'validation_delta' OR
		(m.validation_epoch=r.validation_epoch AND r.validation_state IN ('checked','stale')))`, runID).Scan(&pendingMembers); err != nil {
		return rawderive.ParityReport{}, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+s.quoted+`.migration_parity_sources src
		JOIN `+s.quoted+`.migration_parity_runs r ON r.run_id=src.run_id AND r.init_epoch=src.init_epoch
		WHERE src.run_id=$1 AND src.required AND (src.verdict IS NULL OR (src.source_kind='raw' AND NOT src.candidate_complete))
		AND (src.inventory_kind<>'validation_delta' OR (src.validation_epoch=r.validation_epoch AND r.validation_state IN ('checked','stale')))`, runID).Scan(&report.PendingSources); err != nil {
		return rawderive.ParityReport{}, err
	}
	memberTotal := report.Members.Matched + report.Members.Mismatched + report.Members.Ambiguous + report.Members.LegacyOnly + report.Members.Missing + report.Members.PartialUnsupported + report.Members.Stale
	badMembers := memberTotal - report.Members.Matched
	badSources := report.Sources.Mismatched + report.Sources.Ambiguous + report.Sources.LegacyOnly + report.Sources.Missing + report.Sources.PartialUnsupported + report.Sources.Stale
	report.Complete = report.State == "complete" && report.BaselineSealed && validation == "checked" && report.PendingSources == 0 && pendingMembers == 0 && memberTotal > 0
	if requested != nil && report.CompletedGeneration != *requested {
		report.Code = "pending"
		report.Freshness = "unchecked"
		report.Complete = false
		return report, nil
	}
	if requested != nil && report.Complete {
		report.Freshness = "historical_checked"
		report.Passing = badMembers == 0 && badSources == 0
	}
	if validation == "stale" {
		report.Freshness = "stale"
		report.Passing = false
	}
	return report, nil
}

func (s *MigrationParityStore) BindParity(ctx context.Context, lease rawderive.ParityLease, binding rawderive.ParityBinding) (rawderive.ParityLease, error) {
	if binding.Request.RunID != lease.RunID || binding.Request != lease.Request || binding.Tenant != s.tenant {
		return lease, fmt.Errorf("binding_conflict: parity binding does not match lease")
	}
	if binding.ObservedAt.IsZero() || binding.ObservedAt.Nanosecond()%1000 != 0 {
		return lease, fmt.Errorf("binding_conflict: observation clock must be nonzero and microsecond-aligned")
	}
	fixedObservedAt := binding.ObservedAt.UTC()
	if !lease.ObservedAt.IsZero() && !lease.ObservedAt.Equal(fixedObservedAt) {
		return lease, fmt.Errorf("binding_conflict: parity lease observation clock changed")
	}
	if lease.Token == "" || lease.Owner == "" {
		return lease, fmt.Errorf("binding_conflict: parity binding requires a claimed lease")
	}
	if err := validateParityUUID(lease.Token, "lease token"); err != nil {
		return lease, fmt.Errorf("binding_conflict: %w", err)
	}
	encodedRequest, requestDigest, err := encodeParityRequest(binding.Request)
	if err != nil {
		return lease, err
	}
	encodedBinding, bindingDigest, err := encodeParityBinding(binding)
	if err != nil {
		return lease, err
	}
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return lease, err
	}
	defer func() { _ = tx.Rollback() }()
	var storedRequest, storedRequestDigest, storedBinding, storedBindingDigest []byte
	var generation, epoch int64
	query := `SELECT request,request_digest,binding,binding_digest,request_generation,init_epoch FROM ` + s.quoted + `.migration_parity_runs WHERE run_id=$1 FOR UPDATE`
	if err = tx.QueryRowContext(ctx, query, lease.RunID).Scan(&storedRequest, &storedRequestDigest, &storedBinding, &storedBindingDigest, &generation, &epoch); err != nil {
		return lease, err
	}
	if !bytes.Equal(storedRequest, encodedRequest) || !bytes.Equal(storedRequestDigest, requestDigest[:]) || generation != lease.RequestGeneration || epoch != lease.InitEpoch {
		return lease, fmt.Errorf("binding_conflict: parity lease is stale")
	}
	if len(storedBinding) != 0 && (!bytes.Equal(storedBinding, encodedBinding) || !bytes.Equal(storedBindingDigest, bindingDigest[:])) {
		return lease, fmt.Errorf("binding_conflict: parity run already has another binding")
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_runs SET binding=$2,binding_digest=$3,observed_at=$4
		WHERE run_id=$1 AND request_generation=$5 AND init_epoch=$6 AND lease_token=$7 AND lease_owner=$8
		AND lease_expires_at>clock_timestamp() AND (observed_at IS NULL OR observed_at=$4)`,
		lease.RunID, encodedBinding, bindingDigest[:], fixedObservedAt, lease.RequestGeneration, lease.InitEpoch, lease.Token, lease.Owner)
	if err != nil {
		return lease, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return lease, fmt.Errorf("binding_conflict: parity lease is stale")
	}
	if err = tx.Commit(); err != nil {
		return lease, err
	}
	lease.BindingDigest = bindingDigest
	lease.ObservedAt = fixedObservedAt
	return lease, nil
}

func deleteUnsealedParityEpoch(ctx context.Context, tx *sql.Tx, runID string, epoch int64) error {
	var sealed bool
	if err := tx.QueryRowContext(ctx, `SELECT baseline_sealed FROM migration_parity_runs WHERE run_id=$1 AND init_epoch=$2 FOR UPDATE`, runID, epoch).Scan(&sealed); err != nil {
		return err
	}
	if sealed {
		return fmt.Errorf("cannot delete sealed migration parity evidence")
	}
	for _, table := range []string{"migration_parity_dependencies", "migration_parity_members", "migration_parity_sources"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE run_id=$1 AND init_epoch=$2`, runID, epoch); err != nil {
			return err
		}
	}
	return nil
}

// observeParityCensusPage is a nil-by-default test observer at the census caller
// boundary. It receives only counts and cannot retain or mutate loaded pages.
func (s *MigrationParityStore) observeParityCensusPage(phase string, rows int) {
	if s.parityCensusPageHook != nil && rows > 0 {
		s.parityCensusPageHook(phase, rows)
	}
}
