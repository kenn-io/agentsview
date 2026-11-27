package postgres

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// ExcludeTrashedSession retains accepted custody and source proof, but durably
// excludes the resolved group/cohort from every physical read inventory.
func (s *RawProjectionStore) ExcludeTrashedSession(ctx context.Context, alias string) (bool, error) {
	for {
		removed, retry, err := s.excludeTrashedSessionOnce(ctx, alias)
		if err != nil || !retry {
			return removed, err
		}
	}
}

func (s *RawProjectionStore) excludeTrashedSessionOnce(ctx context.Context, alias string) (bool, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = tx.Rollback() }()
	// Lock outgoing spawn target aliases across every active branch in the
	// alias's group before group locks so permanent deletion serializes with a
	// concurrent first import of a spawned worker using Project's lock order.
	lockedTargets, err := rawAliasSpawnTargets(ctx, tx, alias)
	if err != nil {
		return false, false, err
	}
	if err := lockRawSpawnTargets(ctx, tx, lockedTargets); err != nil {
		return false, false, err
	}
	target, err := s.lockRawCurationTarget(ctx, tx, alias)
	if err != nil {
		if identityErr, ok := errors.AsType[*RawIdentityError](err); ok && identityErr.Identity.State == RawIdentityGone {
			return false, false, nil
		}
		return false, false, err
	}
	var trashed bool
	err = tx.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL FROM sessions WHERE id=$1`, target.SessionID).Scan(&trashed)
	if err != nil {
		return false, false, err
	}
	if !trashed {
		return false, false, nil
	}
	currentTargets, err := queryStrings(ctx, tx, rawGroupSpawnTargetsSQL, []string{target.GroupID})
	if err != nil {
		return false, false, err
	}
	if hasUnlockedSpawnTargets(lockedTargets, currentTargets) {
		return false, true, nil
	}
	if target.AnchorBranch == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM raw_curation WHERE group_id=$1 AND field='excluded' AND branch_id<>''`, target.GroupID)
		if err != nil {
			return false, false, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO raw_curation(group_id,branch_id,field,value) VALUES($1,'','excluded','true') ON CONFLICT(group_id,branch_id,field) DO UPDATE SET value='true'`, target.GroupID)
	} else {
		err = excludeRawCohort(ctx, tx, target.GroupID, target.SessionID)
	}
	if err != nil {
		return false, false, err
	}
	changes, err := s.materializeGroup(ctx, tx, target.GroupID, nil)
	if err != nil {
		return false, false, err
	}
	groupAliases, err := queryStrings(ctx, tx, `SELECT alias_id FROM raw_session_public_aliases WHERE group_id=$1`, target.GroupID)
	if err != nil {
		return false, false, err
	}
	affectedAliases := append(append([]string(nil), currentTargets...), groupAliases...)
	if _, err = reconcileRawSpawnLinks(ctx, tx, []string{target.GroupID}, affectedAliases); err != nil {
		return false, false, err
	}
	if err = publishRawRevision(ctx, tx, true, changes); err != nil {
		return false, false, err
	}
	return true, false, tx.Commit()
}

// EmptyTrash selects only fully trashed current cohorts under sorted group
// locks. An invisible branch in an otherwise visible cohort is not selected.
func (s *RawProjectionStore) EmptyTrash(ctx context.Context) (int, error) {
	for {
		count, retry, err := s.emptyTrashOnce(ctx)
		if err != nil || !retry {
			return count, err
		}
	}
}

func (s *RawProjectionStore) emptyTrashOnce(ctx context.Context) (int, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()
	trashedGroups, err := queryStrings(ctx, tx, `SELECT DISTINCT raw_group_id FROM sessions WHERE raw_group_id<>'' AND deleted_at IS NOT NULL ORDER BY raw_group_id`)
	if err != nil {
		return 0, false, err
	}
	if len(trashedGroups) == 0 {
		return 0, false, tx.Commit()
	}
	lockedTargets, err := queryStrings(ctx, tx, rawGroupSpawnTargetsSQL, trashedGroups)
	if err != nil {
		return 0, false, err
	}
	if err := lockRawSpawnTargets(ctx, tx, lockedTargets); err != nil {
		return 0, false, err
	}
	groups, err := queryStrings(ctx, tx, `SELECT g.group_id FROM raw_session_groups g WHERE g.group_id=ANY($1) AND EXISTS(SELECT 1 FROM sessions s WHERE s.raw_group_id=g.group_id AND s.deleted_at IS NOT NULL) ORDER BY g.group_id FOR UPDATE OF g`, trashedGroups)
	if err != nil {
		return 0, false, err
	}
	var currentTargets []string
	if len(groups) > 0 {
		currentTargets, err = queryStrings(ctx, tx, rawGroupSpawnTargetsSQL, groups)
		if err != nil {
			return 0, false, err
		}
		if hasUnlockedSpawnTargets(lockedTargets, currentTargets) {
			return 0, true, nil
		}
	}
	count := 0
	var excluded [][]rawEmbeddingChange
	for _, group := range groups {
		ids, err := queryStrings(ctx, tx, `SELECT id FROM sessions WHERE raw_group_id=$1 AND raw_group_id<>'' AND deleted_at IS NOT NULL ORDER BY id`, group)
		if err != nil {
			return 0, false, err
		}
		for _, id := range ids {
			if err = excludeRawCohort(ctx, tx, group, id); err != nil {
				return 0, false, err
			}
			count++
		}
		if len(ids) > 0 {
			changes, err := s.materializeGroup(ctx, tx, group, nil)
			if err != nil {
				return 0, false, err
			}
			excluded = append(excluded, changes)
		}
	}
	if count > 0 {
		groupAliases, err := queryStrings(ctx, tx, `SELECT alias_id FROM raw_session_public_aliases WHERE group_id=ANY($1)`, groups)
		if err != nil {
			return 0, false, err
		}
		affectedAliases := append(append([]string(nil), currentTargets...), groupAliases...)
		if _, err = reconcileRawSpawnLinks(ctx, tx, groups, affectedAliases); err != nil {
			return 0, false, err
		}
	}
	for _, changes := range excluded {
		if err = publishRawRevision(ctx, tx, true, changes); err != nil {
			return 0, false, err
		}
	}
	return count, false, tx.Commit()
}

func excludeRawCohort(ctx context.Context, tx *sql.Tx, group, id string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO raw_curation(group_id,branch_id,field,value) SELECT group_id,branch_id,'excluded','true' FROM raw_session_branches WHERE group_id=$1 AND session_id=$2 AND active ON CONFLICT(group_id,branch_id,field) DO UPDATE SET value='true'`, group, id)
	return err
}

const rawGroupSpawnTargetsSQL = `SELECT DISTINCT e.target_alias
	FROM raw_session_branches b
	JOIN raw_session_links e ON e.branch_id=b.branch_id
	WHERE b.group_id=ANY($1) AND b.active AND e.kind IN ('call','event')
	ORDER BY e.target_alias`

func rawAliasSpawnTargets(ctx context.Context, tx *sql.Tx, alias string) ([]string, error) {
	return queryStrings(ctx, tx, `SELECT DISTINCT e.target_alias
		FROM raw_session_public_aliases a
		JOIN raw_session_branches b ON b.group_id=a.group_id AND b.active
		JOIN raw_session_links e ON e.branch_id=b.branch_id
		WHERE a.alias_id=$1 AND e.kind IN ('call','event')
		ORDER BY e.target_alias`, alias)
}

func lockRawSpawnTargets(ctx context.Context, tx *sql.Tx, aliases []string) error {
	if len(aliases) == 0 {
		return nil
	}
	return lockHostedAliases(ctx, tx, aliases)
}

func hasUnlockedSpawnTargets(locked, current []string) bool {
	return slices.ContainsFunc(current, func(alias string) bool {
		return !slices.Contains(locked, alias)
	})
}

func queryStrings(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err = rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
