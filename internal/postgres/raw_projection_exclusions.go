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
	target, err := s.lockRawCurationTarget(ctx, tx, alias)
	if err != nil {
		var identityErr *RawIdentityError
		if errors.As(err, &identityErr) && identityErr.Identity.State == RawIdentityGone {
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
	if err = s.publishRawExclusion(ctx, tx, target.GroupID); err != nil {
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
	rows, err := tx.QueryContext(ctx, `SELECT g.group_id FROM raw_session_groups g WHERE EXISTS(SELECT 1 FROM sessions s WHERE s.raw_group_id=g.group_id AND s.deleted_at IS NOT NULL) ORDER BY g.group_id FOR UPDATE OF g`)
	if err != nil {
		return 0, err
	}
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
	for _, group := range groups {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions WHERE raw_group_id=$1 AND raw_group_id<>'' AND deleted_at IS NOT NULL ORDER BY id`, group)
		if err != nil {
			return 0, err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return 0, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
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
			if err = s.publishRawExclusion(ctx, tx, group); err != nil {
				return 0, err
			}
		}
	}
	return count, tx.Commit()
}
func excludeRawCohort(ctx context.Context, tx *sql.Tx, group, id string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO raw_curation(group_id,branch_id,field,value) SELECT group_id,branch_id,'excluded','true'::jsonb FROM raw_session_branches WHERE group_id=$1 AND session_id=$2 AND active ON CONFLICT(group_id,branch_id,field) DO UPDATE SET value='true'`, group, id)
	return err
}
func (s *RawProjectionStore) publishRawExclusion(ctx context.Context, tx *sql.Tx, group string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE raw_corpus_state SET identity_revision=identity_revision+1 WHERE singleton=1`); err != nil {
		return err
	}
	return s.publishRawCuration(ctx, tx, group)
}
