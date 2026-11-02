package postgres

import (
	"context"
	"database/sql"
	"errors"
)

// Promotion retains the old branch and contributions as history. Its effective
// overlays move only to the new source/member, preserving destination defaults
// and other branches. Old variant URLs follow it. Projection already holds
// source, alias, group and session locks.
func promoteRawMemberState(ctx context.Context, tx *sql.Tx, previous rawBranch, group, branch string) error {
	var conflict bool
	// Conflicting names and pin notes require an explicit user decision; never
	// choose one by hash order. Rollback keeps the previous publication intact.
	err := tx.QueryRowContext(ctx, `WITH old AS (
 SELECT DISTINCT ON (field) field,value FROM raw_curation
 WHERE group_id=$1 AND branch_id IN ('',$2) ORDER BY field,branch_id DESC)
 SELECT EXISTS(SELECT 1 FROM old JOIN raw_curation n ON n.group_id=$3 AND n.field=old.field
 WHERE old.field='display_name' AND n.value IS DISTINCT FROM old.value)`, previous.Group, previous.ID, group).Scan(&conflict)
	if err != nil {
		return err
	}
	if conflict {
		return errors.New("native identity promotion conflicts with saved display names; reconcile curation before retrying")
	}
	err = tx.QueryRowContext(ctx, `WITH old AS (
 SELECT DISTINCT ON (message_key) * FROM raw_pins WHERE group_id=$1 AND branch_id IN ('',$2) ORDER BY message_key,branch_id DESC)
 SELECT EXISTS(SELECT 1 FROM old JOIN raw_pins n ON n.group_id=$3 AND n.message_key=old.message_key
 WHERE old.pinned AND n.pinned AND (n.note IS DISTINCT FROM old.note OR n.ordinal<>old.ordinal))`, previous.Group, previous.ID, group).Scan(&conflict)
	if err != nil {
		return err
	}
	if conflict {
		return errors.New("native identity promotion conflicts with saved pin notes; reconcile pins before retrying")
	}
	for _, query := range []string{
		`INSERT INTO raw_curation(group_id,branch_id,field,value)
 SELECT $3,$4,field,CASE WHEN field IN ('trashed','excluded') AND EXISTS(SELECT 1 FROM raw_curation c WHERE c.group_id=$3 AND c.branch_id='' AND c.field=old.field AND c.value='true'::jsonb) THEN 'true'::jsonb ELSE value END
 FROM (SELECT DISTINCT ON (field) field,value FROM raw_curation WHERE group_id=$1 AND branch_id IN ('',$2) ORDER BY field,branch_id DESC) old
 ON CONFLICT(group_id,branch_id,field) DO UPDATE SET value=EXCLUDED.value`,
		`INSERT INTO raw_pins(group_id,branch_id,message_key,ordinal,content_revision,pinned,note,created_at)
 SELECT $3,$4,message_key,ordinal,content_revision,pinned,note,created_at FROM (SELECT DISTINCT ON (message_key) * FROM raw_pins WHERE group_id=$1 AND branch_id IN ('',$2) ORDER BY message_key,branch_id DESC) old
 ON CONFLICT(group_id,branch_id,message_key) DO UPDATE SET ordinal=EXCLUDED.ordinal,content_revision=EXCLUDED.content_revision,pinned=EXCLUDED.pinned,note=EXCLUDED.note,created_at=EXCLUDED.created_at`,
		`INSERT INTO raw_session_public_aliases(alias_id,group_id,anchor_branch)
 SELECT alias_id,$3,CASE WHEN anchor_branch='' THEN '' ELSE $4 END FROM raw_session_public_aliases WHERE group_id=$1 AND anchor_branch IN ('',$2)
 ON CONFLICT(alias_id,group_id) DO NOTHING`,
		`UPDATE raw_session_branches SET active=false WHERE branch_id=$2 AND group_id=$1`,
	} {
		// Supplying unused numbered parameters is valid when all four types are
		// explicit; use typed bindings so each statement shares the same inputs.
		query = `WITH promotion AS (SELECT $1::text old_group,$2::text old_branch,$3::text new_group,$4::text new_branch) ` + query
		if _, err := tx.ExecContext(ctx, query, previous.Group, previous.ID, group, branch); err != nil {
			return err
		}
	}
	return nil
}
