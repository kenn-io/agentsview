package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// RawIdentityError lets the hosted boundary distinguish conflict from gone
// without accepting a private row ID or selecting an arbitrary cohort.
type RawIdentityError struct{ Identity RawIdentity }

func (e *RawIdentityError) Error() string {
	return "raw session identity is " + string(e.Identity.State)
}

func (s *RawProjectionStore) lockRawCurationTarget(ctx context.Context, tx *sql.Tx, alias string) (RawIdentity, error) {
	rows, err := tx.QueryContext(ctx, `SELECT g.group_id FROM raw_session_groups g JOIN raw_session_public_aliases a ON a.group_id=g.group_id WHERE a.alias_id=$1 ORDER BY g.group_id FOR UPDATE OF g`, alias)
	if err != nil {
		return RawIdentity{}, err
	}
	for rows.Next() {
		var group string
		if err = rows.Scan(&group); err != nil {
			rows.Close()
			return RawIdentity{}, err
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return RawIdentity{}, err
	}
	resolved, err := resolveRawIdentity(ctx, tx, alias)
	if err != nil {
		return resolved, err
	}
	if resolved.State != RawIdentityUnique {
		return resolved, &RawIdentityError{resolved}
	}
	if s.curationGuard != nil {
		if err := s.curationGuard(ctx, tx, alias, resolved); err != nil {
			return resolved, err
		}
	}
	return resolved, nil
}

// SetCuration changes exactly one field on the current target. A base alias
// supersedes the same field's branch overrides, including inactive branches.
// A variant action writes every active member of its current content cohort.
func (s *RawProjectionStore) SetCuration(ctx context.Context, alias, field string, value any) error {
	switch field {
	case "starred", "trashed":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("curation field requires boolean")
		}
	case "display_name":
		if value != nil {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("display name requires string or null")
			}
		}
	default:
		return fmt.Errorf("unsupported raw curation field")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	target, err := s.lockRawCurationTarget(ctx, tx, alias)
	if err != nil {
		return err
	}
	if target.AnchorBranch == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM raw_curation WHERE group_id=$1 AND field=$2 AND branch_id<>''`, target.GroupID, field)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO raw_curation(group_id,branch_id,field,value) VALUES($1,'',$2,$3) ON CONFLICT(group_id,branch_id,field) DO UPDATE SET value=EXCLUDED.value`, target.GroupID, field, string(encoded))
	} else {
		members, memberErr := rawCurationMembers(ctx, tx, target)
		if memberErr != nil {
			return memberErr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO raw_curation(group_id,branch_id,field,value) SELECT group_id,branch_id,$3,$4::jsonb FROM raw_session_branches WHERE group_id=$1 AND session_id=$2 AND active AND branch_id=ANY($5) ON CONFLICT(group_id,branch_id,field) DO UPDATE SET value=EXCLUDED.value`, target.GroupID, target.SessionID, field, string(encoded), members)
	}
	if err != nil {
		return err
	}
	if err = s.publishRawCuration(ctx, tx, target.GroupID); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *RawProjectionStore) publishRawCuration(ctx context.Context, tx *sql.Tx, group string) error {
	var revision int64
	err := tx.QueryRowContext(ctx, `UPDATE raw_corpus_state SET corpus_revision=corpus_revision+1 WHERE singleton=1 RETURNING corpus_revision`).Scan(&revision)
	if err != nil {
		return err
	}
	return s.materializeGroup(ctx, tx, group, revision)
}

type rawOverlays map[string]map[string]json.RawMessage

func loadRawOverlays(ctx context.Context, tx *sql.Tx, group string) (rawOverlays, error) {
	rows, err := tx.QueryContext(ctx, `SELECT branch_id,field,value FROM raw_curation WHERE group_id=$1`, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := rawOverlays{}
	for rows.Next() {
		var branch, field string
		var value []byte
		if err = rows.Scan(&branch, &field, &value); err != nil {
			return nil, err
		}
		if out[branch] == nil {
			out[branch] = map[string]json.RawMessage{}
		}
		out[branch][field] = value
	}
	return out, rows.Err()
}
func (o rawOverlays) effective(branch, field string) json.RawMessage {
	if value, ok := o[branch][field]; ok {
		return value
	}
	return o[""][field]
}
func (o rawOverlays) boolean(branch, field string) bool {
	var value bool
	_ = json.Unmarshal(o.effective(branch, field), &value)
	return value
}
func (s *RawProjectionStore) materializeCuration(ctx context.Context, tx *sql.Tx, group, id string, members []rawBranch) error {
	overlays, err := loadRawOverlays(ctx, tx, group)
	if err != nil {
		return err
	}
	visible := make([]rawBranch, 0, len(members))
	for _, b := range members {
		if !overlays.boolean(b.ID, "trashed") {
			visible = append(visible, b)
		}
	}
	trashed := len(visible) == 0
	if trashed {
		visible = members
	}
	starred := false
	var name *string
	selectedName := false
	for _, b := range visible {
		if !trashed && overlays.boolean(b.ID, "starred") {
			starred = true
		}
		if raw, present := overlays[b.ID]["display_name"]; present && !selectedName {
			if err = json.Unmarshal(raw, &name); err != nil {
				return err
			}
			selectedName = true
		}
	}
	if !selectedName {
		if raw, ok := overlays[""]["display_name"]; ok {
			if err = json.Unmarshal(raw, &name); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE sessions SET display_name=$2,deleted_at=CASE WHEN $3 THEN COALESCE(deleted_at,clock_timestamp()) ELSE NULL END,deletion_cause=CASE WHEN $3 THEN 'user' ELSE NULL END WHERE id=$1`, id, name, trashed)
	if err != nil {
		return err
	}
	if starred {
		_, err = tx.ExecContext(ctx, `INSERT INTO starred_sessions(session_id) VALUES($1) ON CONFLICT(session_id) DO NOTHING`, id)
	} else {
		_, err = tx.ExecContext(ctx, `DELETE FROM starred_sessions WHERE session_id=$1`, id)
	}
	if err != nil {
		return err
	}
	return materializeRawPins(ctx, tx, group, id, visible)
}

func rawCurationMembers(ctx context.Context, tx *sql.Tx, target RawIdentity) ([]string, error) {
	branches, err := loadRawBranches(ctx, tx, "group_id", target.GroupID)
	if err != nil {
		return nil, err
	}
	overlays, err := loadRawOverlays(ctx, tx, target.GroupID)
	if err != nil {
		return nil, err
	}
	var members []string
	for _, b := range branches {
		if b.Active && b.Session == target.SessionID && !overlays.boolean(b.ID, "excluded") {
			members = append(members, b.ID)
		}
	}
	return members, nil
}
