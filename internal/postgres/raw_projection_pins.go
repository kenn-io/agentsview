package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"
)

// RawPinReference remains durable when no current message matches its key.
// Ordinal alone is never sufficient to resolve or reattach a pin.
type RawPinReference struct {
	MessageKey            string
	Ordinal               int
	ContentRevision, Note string
	Pinned, Resolved      bool
	CreatedAt             time.Time
}

func (s *RawProjectionStore) SetPin(ctx context.Context, alias string, ordinal int, pinned bool, note string) error {
	_, err := s.SetPinReturningID(ctx, alias, ordinal, pinned, note)
	return err
}

// SetPinReturningID captures the response in the transaction that selects the
// cohort. A later publication may replace materialized rows, but cannot turn a
// committed pin into a failed response using an earlier physical identity.
func (s *RawProjectionStore) SetPinReturningID(ctx context.Context, alias string, ordinal int, pinned bool, note string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	target, err := s.lockRawCurationTarget(ctx, tx, alias)
	if err != nil {
		return 0, err
	}
	payload, err := loadRawPayload(ctx, tx, target.SessionID)
	if err != nil {
		return 0, err
	}
	var key string
	for _, m := range payload.Messages {
		if m.Ordinal == ordinal {
			key, err = rawMessageKey(m)
			if err != nil {
				return 0, err
			}
			break
		}
	}
	if key == "" {
		return 0, fmt.Errorf("raw pin message is absent")
	}
	if err = writeRawPin(ctx, tx, target, key, ordinal, target.ContentRevision, pinned, note); err != nil {
		return 0, err
	}
	if err = s.publishRawCuration(ctx, tx, target.GroupID); err != nil {
		return 0, err
	}
	var id int64
	if pinned {
		if err = tx.QueryRowContext(ctx, `SELECT id FROM pinned_messages WHERE session_id=$1 AND ordinal=$2`, target.SessionID, ordinal).Scan(&id); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// RemovePin removes a durable message identity even when no current ordinal
// resolves it. Group and current-cohort overrides follow the same rules as SetPin.
func (s *RawProjectionStore) RemovePin(ctx context.Context, alias, messageKey string) error {
	if messageKey == "" {
		return fmt.Errorf("raw pin message key is required")
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
	var ordinal int
	var revision string
	err = tx.QueryRowContext(ctx, `SELECT ordinal,content_revision FROM raw_pins WHERE group_id=$1 AND message_key=$2 ORDER BY branch_id LIMIT 1`, target.GroupID, messageKey).Scan(&ordinal, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if err = writeRawPin(ctx, tx, target, messageKey, ordinal, revision, false, ""); err != nil {
		return err
	}
	if err = s.publishRawCuration(ctx, tx, target.GroupID); err != nil {
		return err
	}
	return tx.Commit()
}

func writeRawPin(ctx context.Context, tx *sql.Tx, target RawIdentity, key string, ordinal int, revision string, pinned bool, note string) error {
	var err error
	if target.AnchorBranch == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM raw_pins WHERE group_id=$1 AND message_key=$2 AND branch_id<>''`, target.GroupID, key)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO raw_pins(group_id,branch_id,message_key,ordinal,content_revision,pinned,note) VALUES($1,'',$2,$3,$4,$5,$6) ON CONFLICT(group_id,branch_id,message_key) DO UPDATE SET ordinal=EXCLUDED.ordinal,content_revision=EXCLUDED.content_revision,pinned=EXCLUDED.pinned,note=EXCLUDED.note,created_at=CASE WHEN raw_pins.pinned THEN raw_pins.created_at ELSE clock_timestamp() END`, target.GroupID, key, ordinal, revision, pinned, note)
	} else {
		members, memberErr := rawCurationMembers(ctx, tx, target)
		if memberErr != nil {
			return memberErr
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO raw_pins(group_id,branch_id,message_key,ordinal,content_revision,pinned,note) SELECT group_id,branch_id,$3,$4,$5,$6,$7 FROM raw_session_branches WHERE group_id=$1 AND session_id=$2 AND active AND branch_id=ANY($8) ON CONFLICT(group_id,branch_id,message_key) DO UPDATE SET ordinal=EXCLUDED.ordinal,content_revision=EXCLUDED.content_revision,pinned=EXCLUDED.pinned,note=EXCLUDED.note,created_at=CASE WHEN raw_pins.pinned THEN raw_pins.created_at ELSE clock_timestamp() END`, target.GroupID, target.SessionID, key, ordinal, revision, pinned, note, members)
	}
	if err != nil {
		return err
	}
	return nil
}

func (s *RawProjectionStore) PinReferences(ctx context.Context, alias string) ([]RawPinReference, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	target, err := resolveRawIdentity(ctx, tx, alias)
	if err != nil {
		return nil, err
	}
	if target.State != RawIdentityUnique {
		return nil, &RawIdentityError{target}
	}
	branches, err := loadRawBranches(ctx, tx, "group_id", target.GroupID)
	if err != nil {
		return nil, err
	}
	overlays, err := loadRawOverlays(ctx, tx, target.GroupID)
	if err != nil {
		return nil, err
	}
	var members, visible []rawBranch
	for _, b := range branches {
		if b.Active && !overlays.boolean(b.ID, "excluded") && b.Session == target.SessionID {
			members = append(members, b)
			if !overlays.boolean(b.ID, "trashed") {
				visible = append(visible, b)
			}
		}
	}
	if len(visible) == 0 {
		visible = members
	}
	refs, err := rawEffectivePins(ctx, tx, target.GroupID, target.SessionID, visible)
	if err != nil {
		return nil, err
	}
	return refs, tx.Commit()
}
func rawEffectivePins(ctx context.Context, tx *sql.Tx, group, id string, members []rawBranch) ([]RawPinReference, error) {
	rows, err := tx.QueryContext(ctx, `SELECT branch_id,message_key,ordinal,content_revision,pinned,note,created_at FROM raw_pins WHERE group_id=$1 ORDER BY branch_id,message_key`, group)
	if err != nil {
		return nil, err
	}
	overlays := map[string]map[string]RawPinReference{}
	keys := map[string]bool{}
	for rows.Next() {
		var branch string
		var p RawPinReference
		if err = rows.Scan(&branch, &p.MessageKey, &p.Ordinal, &p.ContentRevision, &p.Pinned, &p.Note, &p.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if overlays[branch] == nil {
			overlays[branch] = map[string]RawPinReference{}
		}
		overlays[branch][p.MessageKey] = p
		keys[p.MessageKey] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	payload, err := loadRawPayload(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	current := map[string]int{}
	for _, m := range payload.Messages {
		key, err := rawMessageKey(m)
		if err != nil {
			return nil, err
		}
		current[key] = m.Ordinal
	}
	return selectRawEffectivePins(overlays, current, members), nil
}
func selectRawEffectivePins(overlays map[string]map[string]RawPinReference, current map[string]int, members []rawBranch) []RawPinReference {
	keys := map[string]bool{}
	for _, pins := range overlays {
		for key := range pins {
			keys[key] = true
		}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	slices.Sort(ordered)
	var refs []RawPinReference
	for _, key := range ordered {
		var selected *RawPinReference
		explicit := false
		for _, b := range members {
			p, override := overlays[b.ID][key]
			if !override {
				p = overlays[""][key]
			}
			if !p.Pinned {
				continue
			}
			if selected == nil || override && !explicit {
				copy := p
				selected = &copy
				explicit = override
			}
		}
		if selected != nil {
			ordinal, ok := current[key]
			selected.Resolved = ok && ordinal == selected.Ordinal
			refs = append(refs, *selected)
		}
	}
	return refs
}
func materializeRawPins(ctx context.Context, tx *sql.Tx, group, id string, members []rawBranch) error {
	refs, err := rawEffectivePins(ctx, tx, group, id, members)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM pinned_messages WHERE session_id=$1`, id); err != nil {
		return err
	}
	for _, p := range refs {
		if !p.Resolved {
			continue
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO pinned_messages(session_id,message_id,ordinal,source_uuid,note,created_at) SELECT session_id,ordinal,ordinal,source_uuid,$3,$4 FROM messages WHERE session_id=$1 AND ordinal=$2`, id, p.Ordinal, p.Note, p.CreatedAt)
		if err != nil {
			return err
		}
	}
	return nil
}
