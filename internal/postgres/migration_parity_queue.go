package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/agentsview/internal/rawderive"
)

func validParityCode(code string) bool {
	switch code {
	case "pending", "profile_unavailable", "baseline_unprovisioned", "binding_conflict",
		"snapshot_timeout", "invalid", "missing_history", "missing_object",
		"sandbox_unavailable", "parse_failed", "limit_exceeded", "cleanup_failed",
		"canceled", "dependency_changed", "historical_evidence",
		"generation_not_available", "exclusion_provenance_unavailable", "internal":
		return true
	default:
		return false
	}
}

// ClaimParity leases one runnable parity request. Reclaiming an abandoned
// unsealed capture discards only that unsealed evidence and starts a new epoch;
// sealed evidence is retained for finite source replay and validation resumes.
func (s *MigrationParityStore) ClaimParity(
	ctx context.Context,
	owner string,
	duration time.Duration,
) (*rawderive.ParityLease, error) {
	if !rawderive.ValidLeaseOwner(owner) || duration <= 0 {
		return nil, fmt.Errorf("invalid migration parity lease")
	}
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var runID string
	var encodedRequest []byte
	var generation, epoch int64
	var batch, budget int
	var sealed bool
	var priorToken sql.NullString
	var bindingDigest []byte
	var observedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT run_id::text,request,request_generation,init_epoch,batch_size,budget_sources,
		baseline_sealed,lease_token::text,binding_digest,observed_at
		FROM `+s.quoted+`.migration_parity_runs
		WHERE state IN ('requested','initializing','running','validating') AND (budget_sources>0 OR baseline_sealed)
		AND (lease_token IS NULL OR lease_expires_at<=clock_timestamp())
		ORDER BY run_id FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(
		&runID, &encodedRequest, &generation, &epoch, &batch, &budget,
		&sealed, &priorToken, &bindingDigest, &observedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	request, err := decodeParityRequest(encodedRequest)
	if err != nil {
		return nil, err
	}
	if request.RunID != runID {
		return nil, fmt.Errorf("binding_conflict: parity request identity changed")
	}
	if priorToken.Valid && !sealed {
		if err = deleteUnsealedParityEpoch(ctx, tx, runID, epoch); err != nil {
			return nil, err
		}
		epoch++
		_, err = tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_runs SET init_epoch=$2,
			baseline_sealed=FALSE,inventory_digest=NULL,baseline_digest=NULL,
			validation_epoch=0,validation_state='unchecked',validation_digest=NULL,
			baseline_observed_at=NULL,runtime_observed_at=NULL,state='requested',last_code='pending'
			WHERE run_id=$1`, runID, epoch)
		if err != nil {
			return nil, err
		}
	}
	if budget < batch {
		batch = budget
	}
	token := uuid.NewString()
	state := "running"
	if !sealed {
		state = "initializing"
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_runs SET lease_owner=$2,lease_token=$3,
		lease_expires_at=clock_timestamp()+make_interval(secs=>$4),state=$5
		WHERE run_id=$1 AND request_generation=$6 AND init_epoch=$7`,
		runID, owner, token, duration.Seconds(), state, generation, epoch)
	if err != nil {
		return nil, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("binding_conflict: parity request changed while claiming")
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	lease := &rawderive.ParityLease{
		RunID: runID, Owner: owner, Token: token, Request: request,
		RequestGeneration: generation, InitEpoch: epoch, BatchSize: batch,
	}
	if len(bindingDigest) != 0 {
		if len(bindingDigest) != len(lease.BindingDigest) {
			return nil, fmt.Errorf("invalid migration parity binding digest")
		}
		copy(lease.BindingDigest[:], bindingDigest)
	}
	if observedAt.Valid {
		lease.ObservedAt = observedAt.Time.UTC()
	}
	return lease, nil
}

// HeartbeatParity extends only the currently owned, unexpired run lease.
func (s *MigrationParityStore) HeartbeatParity(
	ctx context.Context,
	lease rawderive.ParityLease,
	duration time.Duration,
) error {
	if duration <= 0 || lease.RunID == "" || lease.Token == "" || lease.Owner == "" {
		return fmt.Errorf("invalid migration parity lease")
	}
	result, err := s.pg.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_runs
		SET lease_expires_at=clock_timestamp()+make_interval(secs=>$6)
		WHERE run_id=$1 AND init_epoch=$2 AND request_generation=$3 AND lease_token=$4 AND lease_owner=$5
		AND lease_expires_at>clock_timestamp()`, lease.RunID, lease.InitEpoch,
		lease.RequestGeneration, lease.Token, lease.Owner, duration.Seconds())
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("binding_conflict: parity lease is stale")
	}
	return nil
}

// FinishParityRequest consumes exactly the claimed request generation and
// releases its run lease. Remaining sources require another explicit resume.
func (s *MigrationParityStore) FinishParityRequest(
	ctx context.Context,
	lease rawderive.ParityLease,
	code string,
) error {
	if !validParityCode(code) || lease.RunID == "" || lease.Token == "" || lease.Owner == "" {
		return fmt.Errorf("invalid migration parity completion")
	}
	result, err := s.pg.ExecContext(ctx, `UPDATE `+s.quoted+`.migration_parity_runs
		SET state='complete',completed_generation=request_generation,budget_sources=0,last_code=$6,
		validation_state=CASE WHEN $6='pending' THEN validation_state ELSE 'incomplete' END,
		validation_digest=CASE WHEN $6='pending' THEN validation_digest ELSE NULL END,
		lease_owner='',lease_token=NULL,lease_expires_at=NULL
		WHERE run_id=$1 AND init_epoch=$2 AND request_generation=$3 AND lease_token=$4 AND lease_owner=$5
		AND lease_expires_at>clock_timestamp()`, lease.RunID, lease.InitEpoch,
		lease.RequestGeneration, lease.Token, lease.Owner, code)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("binding_conflict: parity lease is stale")
	}
	return nil
}
