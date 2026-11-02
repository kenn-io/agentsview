package postgres

import (
	"context"
	"database/sql"
	"errors"
)

// Relation locks exclude DDL before any publication fence. ROW EXCLUSIVE is the
// access mode the prepared writes need, not a broad exclusive table lock; it is
// compatible with other ordinary writers. Parity/input tables need ACCESS SHARE.
// FK parents are already in the write set. The tenant-led keys and indexes are
// protected by these relation locks and the rollout/alias/group/session guards.
//

func lockRawActivationRelations(ctx context.Context, tx *sql.Tx) error {
	for _, entry := range []struct{ tables, mode string }{
		{`pinned_messages,raw_corpus_state,raw_curation,raw_legacy_supersessions,raw_pins,raw_rollout_scopes,raw_session_groups,sessions,starred_sessions`, "ROW EXCLUSIVE"},
		{`friction_findings,messages,raw_content_revisions,raw_ingest_jobs,raw_message_dialogue,raw_session_branches,raw_session_links,raw_session_public_aliases,raw_source_heads,raw_source_projections,secret_findings,tool_calls,tool_result_events,usage_events`, "ACCESS SHARE"},
	} {
		if _, err := tx.ExecContext(ctx, `LOCK TABLE `+entry.tables+` IN `+entry.mode+` MODE`); err != nil {
			return err
		}
	}
	return nil
}

type rawPreparedActivation struct {
	unscoped      bool
	groups        []string
	served        map[string]bool
	supersessions rawSupersessionPlan
	carries       []rawLegacyCarry
}

// Activation is an owner operation over a stable tenant-wide identity universe.
// It locks the whole discovered closure, including aliases/collision groups and
// old map owners outside the requested selector. Scope filtering is gate input,
// never permission to hide global identity ambiguity or skip trigger targets.
//

func prepareRawActivation(ctx context.Context, tx *sql.Tx, device, provider string) (rawPreparedActivation, error) {
	return prepareRawScopeChange(ctx, tx, device, provider, "active")
}

func prepareRawScopeChange(ctx context.Context, tx *sql.Tx, device, provider, state string) (rawPreparedActivation, error) {
	var p rawPreparedActivation
	if err := lockRawActivationRelations(ctx, tx); err != nil {
		return p, err
	}
	if _, err := tx.ExecContext(ctx, `SELECT device_id FROM raw_rollout_scopes ORDER BY device_id,provider FOR UPDATE`); err != nil {
		return p, err
	}
	scopes, err := loadRawRolloutScopes(ctx, tx)
	if err != nil {
		return p, err
	}
	scopes, err = prospectiveRawScopes(scopes, device, provider, state)
	if err != nil {
		return p, err
	}
	p.unscoped = len(scopes) == 0
	candidates, err := loadRawParityCandidates(ctx, tx)
	if err != nil {
		return p, err
	}
	var aliases []string
	for _, c := range candidates {
		aliases = append(aliases, c.ID, c.PublicID)
		aliases = append(aliases, c.Aliases...)
		if c.Group == "" {
			aliases = append(aliases, legacyPublicVariant(c.ID))
		}
	}
	extra, err := rawStringRows(ctx, tx, `SELECT alias_id FROM raw_session_public_aliases ORDER BY alias_id`)
	if err != nil {
		return p, err
	}
	aliases = append(aliases, extra...)
	if err = lockHostedAliases(ctx, tx, aliases); err != nil {
		return p, err
	}
	p.groups, err = rawStringRows(ctx, tx, `SELECT group_id FROM raw_session_groups ORDER BY group_id FOR UPDATE`)
	if err != nil {
		return p, err
	}
	// Locks every existing physical cohort/legacy row and every FK parent the
	// cache, star and pin statements can reach. Absent keys are excluded by the
	// exclusive rollout guard; legacy trigger try-lock closure is now owned too.
	if _, err = tx.ExecContext(ctx, `SELECT id FROM sessions ORDER BY id FOR UPDATE`); err != nil {
		return p, err
	}
	for _, query := range []string{
		`SELECT legacy_session_id FROM raw_legacy_supersessions ORDER BY legacy_session_id FOR UPDATE`,
		`SELECT group_id FROM raw_curation ORDER BY group_id,branch_id,field FOR UPDATE`,
		`SELECT group_id FROM raw_pins ORDER BY group_id,branch_id,message_key FOR UPDATE`,
		`SELECT session_id FROM starred_sessions ORDER BY session_id FOR UPDATE`,
		`SELECT id FROM pinned_messages ORDER BY session_id,id FOR UPDATE`,
		`SELECT ordinal FROM messages ORDER BY session_id,ordinal FOR SHARE`,
	} {
		if _, err = tx.ExecContext(ctx, query); err != nil {
			return p, err
		}
	}
	// nextval takes and retains the sequence's ordinary access lock now. A gap on
	// refusal/rollback is intentional, as for any aborted pin write. Later pin
	// rematerialization cannot first encounter a sequence/DDL wait after corpus.
	var sequenceValue int64
	if err = tx.QueryRowContext(ctx, `SELECT nextval(pg_get_serial_sequence('pinned_messages','id'))`).Scan(&sequenceValue); err != nil {
		return p, err
	}
	p.served, err = rawServedGroups(ctx, tx, scopes)
	if err != nil {
		return p, err
	}
	p.supersessions, err = prepareRawSupersessions(ctx, tx, p.served)
	if err != nil {
		return p, err
	}
	for _, change := range p.supersessions.Changes {
		if change.After == "" {
			continue
		}
		carry, err := prepareRawLegacyCarry(ctx, tx, change.Legacy, change.After)
		if err != nil {
			return p, err
		}
		p.carries = append(p.carries, carry)
	}
	return p, nil
}

func (p rawPreparedActivation) apply(ctx context.Context, tx *sql.Tx) error {
	for _, group := range p.groups {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET rollout_hidden=$2 WHERE provenance_kind='raw' AND raw_group_id=$1 AND rollout_hidden IS DISTINCT FROM $2`, group, !p.unscoped && !p.served[group]); err != nil {
			return err
		}
	}
	sink := &RawProjectionStore{}
	for _, carry := range p.carries {
		if err := sink.applyRawLegacyCarry(ctx, tx, carry); err != nil {
			return err
		}
	}
	if err := writeRawSupersessionCache(ctx, tx, p.supersessions); err != nil {
		return err
	}
	// This is the first legacy visibility write: no ordinary row writes follow.
	return applyRawLegacyVisibility(ctx, tx, p.supersessions)
}

// prospectiveRawScopes enforces first/last tenant rules without writing control
// state before acceptance. Clear inherits the remaining narrower/broader rules.
//

func prospectiveRawScopes(scopes []RawRolloutScope, device, provider, state string) ([]RawRolloutScope, error) {
	tenantExists := false
	for _, s := range scopes {
		tenantExists = tenantExists || s.DeviceID == "" && s.Provider == ""
	}
	if state != "clear" && (device != "" || provider != "") && !tenantExists {
		return nil, errors.New("create the tenant rollout scope before narrower scopes")
	}
	if state == "clear" && device == "" && provider == "" && len(scopes) > 1 {
		return nil, errors.New("clear narrower rollout scopes before the tenant scope")
	}
	out := make([]RawRolloutScope, 0, len(scopes)+1)
	for _, s := range scopes {
		if s.DeviceID != device || s.Provider != provider {
			out = append(out, s)
		}
	}
	if state != "clear" {
		out = append(out, RawRolloutScope{DeviceID: device, Provider: provider, State: state})
	}
	return out, nil
}

func rawStringRows(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// A nonempty policy is the explicit authority for supersession. User trash is
// deliberately absent: it does not withdraw the logical group. Exclusion does.
//

func rawServedGroups(ctx context.Context, tx *sql.Tx, scopes []RawRolloutScope) (map[string]bool, error) {
	return rawServedGroupsWithin(ctx, tx, scopes, nil)
}

func rawServedGroupsWithin(ctx context.Context, tx *sql.Tx, scopes []RawRolloutScope, groups []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(scopes) == 0 {
		return out, nil
	}
	query := `SELECT DISTINCT b.group_id,p.device_id,p.provider FROM raw_session_branches b JOIN raw_source_projections p ON p.source_id=b.source_id WHERE ` + rawBranchEligibleSQL
	var args []any
	if groups != nil {
		query += ` AND b.group_id=ANY($1)`
		args = append(args, groups)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var group, device, provider string
		if err = rows.Scan(&group, &device, &provider); err != nil {
			return nil, err
		}
		out[group] = out[group] || effectiveRawRollout(scopes, device, provider) == "active"
	}
	return out, rows.Err()
}
