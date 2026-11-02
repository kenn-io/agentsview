package postgres

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"time"
)

// RawRolloutScope selects all sources, one device, one provider, or their
// intersection. Empty selectors mean all. Parity is the last stored gate report.
type RawRolloutScope struct {
	DeviceID                string         `json:"device_id"`
	Provider                string         `json:"provider"`
	State                   string         `json:"state"`
	Parity                  jsontext.Value `json:"parity"`
	ActivatedCorpusRevision *int64         `json:"activated_corpus_revision"`
	UpdatedAt               time.Time      `json:"updated_at"`
	UpdatedBy               string         `json:"updated_by"`
	Served                  int64          `json:"served"`
	Shadow                  int64          `json:"shadow"`
	Superseded              int64          `json:"superseded"`
	UnresolvedPins          int64          `json:"unresolved_pins"`
}

func effectiveRawRollout(scopes []RawRolloutScope, device, provider string) string {
	for _, key := range [][2]string{{device, provider}, {device, ""}, {"", provider}, {"", ""}} {
		for _, scope := range scopes {
			if scope.DeviceID == key[0] && scope.Provider == key[1] {
				return scope.State
			}
		}
	}
	return "active"
}

func loadRawRolloutScopes(ctx context.Context, tx *sql.Tx) ([]RawRolloutScope, error) {
	rows, err := tx.QueryContext(ctx, `SELECT device_id,provider,state,parity,activated_corpus_revision,updated_at,updated_by FROM raw_rollout_scopes ORDER BY device_id,provider`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scopes := []RawRolloutScope{}
	for rows.Next() {
		var scope RawRolloutScope
		var parity []byte
		if err := rows.Scan(&scope.DeviceID, &scope.Provider, &scope.State, &parity, &scope.ActivatedCorpusRevision, &scope.UpdatedAt, &scope.UpdatedBy); err != nil {
			return nil, err
		}
		scope.Parity = parity
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

// lockRawRolloutShared must be the transaction's first statement, before head,
// lease, alias, group or publication locks. Scope writers use its exclusive twin.
func lockRawRolloutShared(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('agentsview-rollout:'||current_schema(),0))`)
	return err
}

func beginRawRollout(ctx context.Context, database *sql.DB, schema, tenant string, write bool) (*sql.Tx, error) {
	if err := validateHostedBinding(schema, tenant); err != nil {
		return nil, err
	}
	options := &sql.TxOptions{ReadOnly: !write, Isolation: sql.LevelRepeatableRead}
	if write {
		options = nil
	}
	tx, err := database.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*sql.Tx, error) { _ = tx.Rollback(); return nil, err }
	if write {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "agentsview-rollout:"+schema); err != nil {
			return fail(err)
		}
	}
	if err = validateRawRolloutTransaction(ctx, tx, schema, tenant); err != nil {
		return fail(err)
	}
	return tx, nil
}

func validateRawRolloutTransaction(ctx context.Context, tx *sql.Tx, schema, tenant string) error {
	quoted, _ := quoteIdentifier(schema)
	if _, err := tx.ExecContext(ctx, `SELECT set_config('search_path',$1,true),set_config('agentsview.tenant_id',$2,true)`, quoted, tenant); err != nil {
		return err
	}
	var owner bool
	err := tx.QueryRowContext(ctx, `SELECT n.nspowner=r.oid OR r.rolsuper FROM pg_namespace n CROSS JOIN pg_roles r WHERE n.nspname=$1 AND r.rolname=current_user`, schema).Scan(&owner)
	if err != nil {
		return err
	}
	if !owner {
		return errors.New("raw-rollout requires the hosted schema owner connection")
	}
	if err = checkHostedBinding(ctx, tx, schema, tenant); err != nil {
		return err
	}
	if err = checkHostedCatalog(ctx, tx, schema, tenant); err != nil {
		return err
	}
	return nil
}

// ChangeRawRollout records shadow policy or clears exactly one scope. Its
// complete cache/carry/visibility recomputation shares activation's prepared
// write set, but never bypasses the active gate.
func ChangeRawRollout(ctx context.Context, database *sql.DB, schema, tenant, device, provider, action string) error {
	if action != "shadow" && action != "clear" {
		return errors.New("raw-rollout action must be shadow or clear")
	}
	_, err := retryPGCuration(ctx, func() (struct{}, error) {
		tx, err := beginRawRollout(ctx, database, schema, tenant, true)
		if err != nil {
			return struct{}{}, err
		}
		defer func() { _ = tx.Rollback() }()
		prepared, err := prepareRawScopeChange(ctx, tx, device, provider, action)
		if err != nil {
			return struct{}{}, err
		}
		if action == "shadow" {
			_, err = tx.ExecContext(ctx, `INSERT INTO raw_rollout_scopes(tenant_id,device_id,provider,state) VALUES($1,$2,$3,'shadow') ON CONFLICT(tenant_id,device_id,provider) DO UPDATE SET state='shadow',updated_at=clock_timestamp(),updated_by=current_user`, tenant, device, provider)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM raw_rollout_scopes WHERE tenant_id=$1 AND device_id=$2 AND provider=$3`, tenant, device, provider)
		}
		if err != nil {
			return struct{}{}, err
		}
		if err = prepared.apply(ctx, tx); err != nil {
			return struct{}{}, err
		}
		if err = publishRawRevision(ctx, tx, true, nil); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, tx.Commit()
	})
	return err
}

// rawBranchEligibleSQL is shared by content rematerialization, rollout
// visibility and supersession matching. Branch overrides beat group defaults;
// trash never withdraws a branch. Queries use the branch table alias b.
const rawBranchEligibleSQL = `b.active AND NOT COALESCE((SELECT c.value::boolean FROM raw_curation c WHERE c.group_id=b.group_id AND c.field='excluded' AND c.branch_id IN ('',b.branch_id) ORDER BY c.branch_id DESC LIMIT 1),false)`

// recomputeRawRollout requires the group's row lock and the rollout lock.
// Eligibility belongs to the logical group, including all its physical cohorts.
func recomputeRawRollout(ctx context.Context, tx *sql.Tx, group string) error {
	scopes, err := loadRawRolloutScopes(ctx, tx)
	if err != nil {
		return err
	}
	served, err := rawServedGroupsWithin(ctx, tx, scopes, []string{group})
	if err != nil {
		return err
	}
	active := len(scopes) == 0 || served[group]
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET rollout_hidden=$2 WHERE provenance_kind='raw' AND raw_group_id=$1 AND rollout_hidden IS DISTINCT FROM $2`, group, !active)
	return err
}

// ListRawRollout reads stored parity and deduplicated physical counts in one
// snapshot. A shared group can contribute to several scope rows.
func ListRawRollout(ctx context.Context, database *sql.DB, schema, tenant, device, provider string) ([]RawRolloutScope, error) {
	tx, err := beginRawRollout(ctx, database, schema, tenant, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	scopes, err := loadRawRolloutDiagnostics(ctx, tx, device, provider)
	if err != nil {
		return nil, err
	}
	return scopes, tx.Commit()
}
