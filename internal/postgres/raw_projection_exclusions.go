package postgres

import (
	"context"
	"database/sql"
	"errors"
)

// ExcludeTrashedSession retains accepted custody and source proof, but durably
// excludes the resolved group/cohort from every physical read inventory.
func (s *RawProjectionStore) ExcludeTrashedSession(ctx context.Context, alias string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	// Lock outgoing spawn target aliases before group locks so permanent
	// deletion serializes with a concurrent first import of a spawned worker
	// using the same lock order as Project.
	if err := lockRawSpawnTargetsForAlias(ctx, tx, alias); err != nil {
		return false, err
	}
	target, err := s.lockRawCurationTarget(ctx, tx, alias)
	if err != nil {
		if identityErr, ok := errors.AsType[*RawIdentityError](err); ok && identityErr.Identity.State == RawIdentityGone {
			return false, nil
		}
		return false, err
	}
	var trashed bool
	err = tx.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL FROM sessions WHERE id=$1`, target.SessionID).Scan(&trashed)
	if err != nil {
		return false, err
	}
	if !trashed {
		return false, nil
	}
	if target.AnchorBranch == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM raw_curation WHERE group_id=$1 AND field='excluded' AND branch_id<>''`, target.GroupID)
		if err != nil {
			return false, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO raw_curation(group_id,branch_id,field,value) VALUES($1,'','excluded','true') ON CONFLICT(group_id,branch_id,field) DO UPDATE SET value='true'`, target.GroupID)
	} else {
		err = excludeRawCohort(ctx, tx, target.GroupID, target.SessionID)
	}
	if err != nil {
		return false, err
	}
	changes, err := s.materializeGroup(ctx, tx, target.GroupID, nil)
	if err != nil {
		return false, err
	}
	if _, err = reconcileRawSpawnLinks(ctx, tx); err != nil {
		return false, err
	}
	if err = publishRawRevision(ctx, tx, true, changes); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// EmptyTrash selects only fully trashed current cohorts under sorted group
// locks. An invisible branch in an otherwise visible cohort is not selected.
func (s *RawProjectionStore) EmptyTrash(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	trashedGroups, err := loadTrashedRawGroups(ctx, tx)
	if err != nil {
		return 0, err
	}
	if len(trashedGroups) == 0 {
		return 0, tx.Commit()
	}
	if err := lockRawSpawnTargetsForGroups(ctx, tx, trashedGroups); err != nil {
		return 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT g.group_id FROM raw_session_groups g WHERE g.group_id=ANY($1) AND EXISTS(SELECT 1 FROM sessions s WHERE s.raw_group_id=g.group_id AND s.deleted_at IS NOT NULL) ORDER BY g.group_id FOR UPDATE OF g`, trashedGroups)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var groups []string
	for rows.Next() {
		var group string
		if err = rows.Scan(&group); err != nil {
			rows.Close()
			return 0, err
		}
		groups = append(groups, group)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	count := 0
	var excluded [][]rawEmbeddingChange
	for _, group := range groups {
		ids, err := func() ([]string, error) {
			rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions WHERE raw_group_id=$1 AND raw_group_id<>'' AND deleted_at IS NOT NULL ORDER BY id`, group)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var ids []string
			for rows.Next() {
				var id string
				if err = rows.Scan(&id); err != nil {
					return nil, err
				}
				ids = append(ids, id)
			}
			return ids, rows.Err()
		}()
		if err != nil {
			return 0, err
		}
		for _, id := range ids {
			if err = excludeRawCohort(ctx, tx, group, id); err != nil {
				return 0, err
			}
			count++
		}
		if len(ids) > 0 {
			changes, err := s.materializeGroup(ctx, tx, group, nil)
			if err != nil {
				return 0, err
			}
			excluded = append(excluded, changes)
		}
	}
	if count > 0 {
		if _, err = reconcileRawSpawnLinks(ctx, tx); err != nil {
			return 0, err
		}
	}
	for _, changes := range excluded {
		if err = publishRawRevision(ctx, tx, true, changes); err != nil {
			return 0, err
		}
	}
	return count, tx.Commit()
}

func excludeRawCohort(ctx context.Context, tx *sql.Tx, group, id string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO raw_curation(group_id,branch_id,field,value) SELECT group_id,branch_id,'excluded','true' FROM raw_session_branches WHERE group_id=$1 AND session_id=$2 AND active ON CONFLICT(group_id,branch_id,field) DO UPDATE SET value='true'`, group, id)
	return err
}

func loadTrashedRawGroups(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT raw_group_id FROM sessions WHERE raw_group_id<>'' AND deleted_at IS NOT NULL ORDER BY raw_group_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []string
	for rows.Next() {
		var group string
		if err = rows.Scan(&group); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func lockRawSpawnTargetsForAlias(ctx context.Context, tx *sql.Tx, alias string) error {
	return lockRawSpawnTargetsQuery(ctx, tx, `SELECT DISTINCT e.target_alias
		FROM raw_session_public_aliases a
		JOIN raw_session_branches b ON b.group_id=a.group_id AND b.active
		  AND (a.anchor_branch='' OR a.anchor_branch=b.branch_id)
		JOIN raw_session_links e ON e.branch_id=b.branch_id
		WHERE a.alias_id=$1 AND e.kind IN ('call','event')`, alias)
}

func lockRawSpawnTargetsForGroups(ctx context.Context, tx *sql.Tx, groups []string) error {
	return lockRawSpawnTargetsQuery(ctx, tx, `SELECT DISTINCT e.target_alias
		FROM raw_session_branches b
		JOIN raw_session_links e ON e.branch_id=b.branch_id
		WHERE b.group_id=ANY($1) AND b.active AND e.kind IN ('call','event')`, groups)
}

func lockRawSpawnTargetsQuery(ctx context.Context, tx *sql.Tx, query string, arg any) error {
	rows, err := tx.QueryContext(ctx, query, arg)
	if err != nil {
		return err
	}
	defer rows.Close()
	var aliases []string
	for rows.Next() {
		var alias string
		if err = rows.Scan(&alias); err != nil {
			return err
		}
		aliases = append(aliases, alias)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	rows.Close()
	if len(aliases) == 0 {
		return nil
	}
	return lockHostedAliases(ctx, tx, aliases)
}
