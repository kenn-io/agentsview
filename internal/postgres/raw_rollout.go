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
	quoted, _ := quoteIdentifier(schema)
	if _, err = tx.ExecContext(ctx, `SELECT set_config('search_path',$1,true),set_config('agentsview.tenant_id',$2,true)`, quoted, tenant); err != nil {
		return fail(err)
	}
	var owner bool
	err = tx.QueryRowContext(ctx, `SELECT n.nspowner=r.oid OR r.rolsuper FROM pg_namespace n CROSS JOIN pg_roles r WHERE n.nspname=$1 AND r.rolname=current_user`, schema).Scan(&owner)
	if err != nil {
		return fail(err)
	}
	if !owner {
		return fail(errors.New("raw-rollout requires the hosted schema owner connection"))
	}
	if err = checkHostedBinding(ctx, tx, schema, tenant); err != nil {
		return fail(err)
	}
	if err = checkHostedCatalog(ctx, tx, schema, tenant); err != nil {
		return fail(err)
	}
	return tx, nil
}

// ChangeRawRollout records shadow policy or clears exactly one scope. Activation
// is deliberately unavailable until its parity gate is implemented.
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
		scopes, err := loadRawRolloutScopes(ctx, tx)
		if err != nil {
			return struct{}{}, err
		}
		tenantExists := false
		for _, scope := range scopes {
			tenantExists = tenantExists || scope.DeviceID == "" && scope.Provider == ""
		}
		if action == "shadow" {
			if (device != "" || provider != "") && !tenantExists {
				return struct{}{}, errors.New("create the tenant rollout scope before narrower scopes")
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO raw_rollout_scopes(tenant_id,device_id,provider,state) VALUES($1,$2,$3,'shadow') ON CONFLICT(tenant_id,device_id,provider) DO UPDATE SET state='shadow',updated_at=clock_timestamp(),updated_by=current_user`, tenant, device, provider)
		} else {
			if device == "" && provider == "" && len(scopes) > 1 {
				return struct{}{}, errors.New("clear narrower rollout scopes before the tenant scope")
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM raw_rollout_scopes WHERE tenant_id=$1 AND device_id=$2 AND provider=$3`, tenant, device, provider)
		}
		if err != nil {
			return struct{}{}, err
		}
		// First and last tenant transitions affect the whole tenant. Narrow writes
		// touch only groups with a source matching the selector.
		rows, err := tx.QueryContext(ctx, `SELECT g.group_id FROM raw_session_groups g WHERE ($1='' AND $2='') OR EXISTS(SELECT 1 FROM raw_session_branches b JOIN raw_source_projections p ON p.source_id=b.source_id WHERE b.group_id=g.group_id AND ($1='' OR p.device_id=$1) AND ($2='' OR p.provider=$2)) ORDER BY g.group_id FOR UPDATE OF g`, device, provider)
		if err != nil {
			return struct{}{}, err
		}
		defer rows.Close()
		var groups []string
		for rows.Next() {
			var group string
			if err = rows.Scan(&group); err != nil {
				break
			}
			groups = append(groups, group)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return struct{}{}, err
		}
		for _, group := range groups {
			if err = recomputeRawRollout(ctx, tx, group); err != nil {
				return struct{}{}, err
			}
		}
		// Publication is last: no later statement can wait on another row lock.
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
	scopes, err := loadRawRolloutScopes(ctx, tx)
	if err != nil {
		return nil, err
	}
	selected := []RawRolloutScope{}
	for _, scope := range scopes {
		if device != "" && scope.DeviceID != device || provider != "" && scope.Provider != provider {
			continue
		}
		err = tx.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE NOT s.rollout_hidden),count(*) FILTER(WHERE s.rollout_hidden) FROM sessions s WHERE s.provenance_kind='raw' AND EXISTS(SELECT 1 FROM raw_session_branches b JOIN raw_source_projections p ON p.source_id=b.source_id WHERE b.group_id=s.raw_group_id AND b.active AND ($1='' OR p.device_id=$1) AND ($2='' OR p.provider=$2))`, scope.DeviceID, scope.Provider).Scan(&scope.Served, &scope.Shadow)
		if err != nil {
			return nil, err
		}
		selected = append(selected, scope)
	}
	return selected, tx.Commit()
}
