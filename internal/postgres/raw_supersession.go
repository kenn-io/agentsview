package postgres

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"go.kenn.io/agentsview/internal/rawparity"
)

// rawLogicalSupersessions matches the global universe before eligibility. One
// logical group may have several physical revisions/cohorts. Public IDs and
// content equality supply no identity evidence. Eligibility must be supplied
// explicitly by the caller; no-scope/default-active is not activation authority.
func rawLogicalSupersessions(rows []rawparity.Candidate, served map[string]bool, previous map[string]string) map[string]string {
	// New automatic pairs retain physical ambiguity checks. An established map
	// may survive a physical split, but only with the same unique logical edge.
	initial := map[string]string{}
	for _, pair := range rawparity.Match(rows) {
		if pair.Class == "equal" {
			initial[pair.Legacy[0].ID] = pair.Raw[0].Group
		}
	}
	logical := []rawparity.Candidate{}
	groups := map[string]int{}
	for _, c := range rows {
		if c.Group == "" {
			logical = append(logical, c)
			continue
		}
		i, ok := groups[c.Group]
		if !ok {
			i = len(logical)
			groups[c.Group] = i
			logical = append(logical, rawparity.Candidate{ID: c.Group, Group: c.Group, Agent: c.Agent})
		}
		logical[i].Aliases = append(logical[i].Aliases, c.Aliases...)
		logical[i].SourceLocal = logical[i].SourceLocal || c.SourceLocal
	}
	out := map[string]string{}
	for _, pair := range rawparity.Match(logical) {
		if pair.Class == "equal" {
			legacy, group := pair.Legacy[0].ID, pair.Raw[0].Group
			if served[group] && (initial[legacy] == group || previous[legacy] == group) {
				out[legacy] = group
			}
		}
	}
	return out
}

//nolint:unused // consumed by the later production activation slice
type rawSupersessionChange struct{ Legacy, Before, After string }

//nolint:unused // consumed by the later production activation slice
type rawLegacyVisibility struct {
	Legacy string
	Hidden bool
}

// Changes and Visibility expose the complete map/legacy row write set for the
// caller's prelock closure. Cache/curation writes and final legacy visibility
// writes are separate: legacy UPDATE triggers publish corpus immediately.
// IdentityChanged must reach the caller's publication even if its ordinary
// materializer reported no content change. No exact revision count is promised.
//
//nolint:unused // consumed by the later production activation slice
type rawSupersessionPlan struct {
	Changes         []rawSupersessionChange
	Visibility      []rawLegacyVisibility
	IdentityChanged bool
}

//nolint:unused // called by the later production activation slice
func prepareRawSupersessions(ctx context.Context, tx *sql.Tx, served map[string]bool) (rawSupersessionPlan, error) {
	return prepareRawSupersessionsWithin(ctx, tx, served, nil, nil)
}

func prepareRawSupersessionsWithin(ctx context.Context, tx *sql.Tx, served map[string]bool, groups, legacyIDs []string) (rawSupersessionPlan, error) {
	var plan rawSupersessionPlan
	candidates, err := loadRawParityCandidatesWithin(ctx, tx, groups, legacyIDs)
	if err != nil {
		return plan, err
	}
	previous := map[string]string{}
	query := `SELECT legacy_session_id,group_id FROM raw_legacy_supersessions`
	var args []any
	if groups != nil {
		query += ` WHERE group_id=ANY($1) OR legacy_session_id=ANY($2)`
		args = append(args, groups, legacyIDs)
	}
	rows, err := tx.QueryContext(ctx, query+` ORDER BY legacy_session_id`, args...)
	if err != nil {
		return plan, err
	}
	defer rows.Close()
	for rows.Next() {
		var legacy, group string
		if err = rows.Scan(&legacy, &group); err != nil {
			break
		}
		previous[legacy] = group
	}
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return plan, err
	}
	next := rawLogicalSupersessions(candidates, served, previous)
	ids := []string{}
	for legacy := range previous {
		ids = append(ids, legacy)
	}
	for legacy := range next {
		ids = append(ids, legacy)
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, legacy := range ids {
		if previous[legacy] != next[legacy] {
			plan.Changes = append(plan.Changes, rawSupersessionChange{legacy, previous[legacy], next[legacy]})
		}
	}
	visibilityRows, err := tx.QueryContext(ctx, `SELECT id,rollout_hidden FROM sessions WHERE provenance_kind='legacy' AND id=ANY($1) ORDER BY id`, ids)
	if err != nil {
		return plan, err
	}
	defer visibilityRows.Close()
	for visibilityRows.Next() {
		var id string
		var hidden bool
		if err = visibilityRows.Scan(&id, &hidden); err != nil {
			break
		}
		if want := next[id] != ""; want != hidden {
			plan.Visibility = append(plan.Visibility, rawLegacyVisibility{id, want})
		}
	}
	if err == nil {
		err = visibilityRows.Err()
	}
	plan.IdentityChanged = len(plan.Changes) > 0 || len(plan.Visibility) > 0
	return plan, err
}

// writeRawSupersessionCache requires the caller's rollout/alias/group/session
// and child/FK locks. It neither acquires corpus nor publishes. Absent keys are
// protected by those writer guards, not by a SELECT of existing cache rows.
//
//nolint:unused // called by the later production activation slice
func writeRawSupersessionCache(ctx context.Context, tx *sql.Tx, plan rawSupersessionPlan) error {
	for _, c := range plan.Changes {
		if c.Before != "" {
			result, err := tx.ExecContext(ctx, `DELETE FROM raw_legacy_supersessions WHERE legacy_session_id=$1 AND group_id=$2`, c.Legacy, c.Before)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return errors.New("supersession preparation changed; retry transaction")
			}
		}
		if c.After != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO raw_legacy_supersessions(legacy_session_id,group_id) VALUES($1,$2)`, c.Legacy, c.After); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyRawLegacyVisibility belongs after ALL ordinary writes and prelocks, not
// inside a per-group materialization loop. The caller retains its publication
// fences through commit and accounts for the trigger's identity/corpus bumps.
// Only derived visibility changes; original legacy content/curation survives.
//
//nolint:unused // called by the later production activation slice
func applyRawLegacyVisibility(ctx context.Context, tx *sql.Tx, plan rawSupersessionPlan) error {
	for _, v := range plan.Visibility {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET rollout_hidden=$2 WHERE id=$1 AND provenance_kind='legacy' AND rollout_hidden IS DISTINCT FROM $2`, v.Legacy, v.Hidden); err != nil {
			return err
		}
	}
	return nil
}
