package postgres

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"slices"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawparity"
)

type rawCarryMessage struct {
	Message       db.Message
	Key, Revision string
}

//nolint:unused // materialized by the later production activation slice
type rawCurationCohort struct {
	ID      string
	Members []rawBranch
}

// The caller prepares under its identity guards, locks all listed cohorts and
// legacy pin/message inputs before any corpus fence, and revalidates the set.
// No helper owns a transaction, publishes or performs general materialization.
//
//nolint:unused // carried into raw overlays by the later activation slice
type rawLegacyCarry struct {
	Legacy, Group    string
	Starred, Trashed bool
	DisplayName      *string
	Pins             []RawPinReference
	UnresolvedPins   int
	Cohorts          []rawCurationCohort
}

func matchRawLegacyPin(source db.Message, targets []rawCarryMessage) (RawPinReference, error) {
	matches := map[string]RawPinReference{}
	if source.SourceUUID == "" {
		return RawPinReference{}, nil
	}
	source.Ordinal = 0
	for _, t := range targets {
		if t.Message.SourceUUID != source.SourceUUID {
			continue
		}
		candidate := t.Message
		candidate.Ordinal = 0
		comparison, err := rawparity.Compare(rawparity.Snapshot{Messages: []db.Message{source}}, rawparity.Snapshot{Messages: []db.Message{candidate}})
		if err != nil {
			return RawPinReference{}, err
		}
		if len(comparison.Differing) == 0 {
			matches[t.Key] = RawPinReference{MessageKey: t.Key, Ordinal: t.Message.Ordinal, ContentRevision: t.Revision, Pinned: true, Resolved: true}
		}
	}
	if len(matches) == 1 {
		for _, ref := range matches {
			return ref, nil
		}
	}
	return RawPinReference{}, nil
}

//nolint:unused // called by the later production activation slice
func prepareRawLegacyCarry(ctx context.Context, tx *sql.Tx, legacy, group string) (rawLegacyCarry, error) {
	carry := rawLegacyCarry{Legacy: legacy, Group: group}
	err := tx.QueryRowContext(ctx, `SELECT display_name,deleted_at IS NOT NULL,EXISTS(SELECT 1 FROM starred_sessions WHERE session_id=s.id) FROM sessions s WHERE id=$1 AND provenance_kind='legacy'`, legacy).Scan(&carry.DisplayName, &carry.Trashed, &carry.Starred)
	if err != nil {
		return carry, err
	}
	branches, err := loadRawBranches(ctx, tx, "group_id", group)
	if err != nil {
		return carry, err
	}
	overlays, err := loadRawOverlays(ctx, tx, group)
	if err != nil {
		return carry, err
	}
	members := map[string][]rawBranch{}
	for _, b := range branches {
		if b.Active && !overlays.boolean(b.ID, "excluded") {
			members[b.Session] = append(members[b.Session], b)
		}
	}
	ids := []string{}
	for id := range members {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var targets []rawCarryMessage
	for _, id := range ids {
		cohort := members[id]
		if len(cohort) == 0 {
			continue
		}
		exists, err := rawPhysicalExists(ctx, tx, id)
		if err != nil {
			return carry, err
		}
		if !exists {
			continue
		}
		carry.Cohorts = append(carry.Cohorts, rawCurationCohort{id, cohort})
		physical, err := loadRawParitySnapshot(ctx, tx, id)
		if err != nil {
			return carry, err
		}
		payload, err := loadRawPayload(ctx, tx, id)
		if err != nil {
			return carry, err
		}
		byOrdinal := map[int]db.Message{}
		for _, m := range physical.Messages {
			byOrdinal[m.Ordinal] = m
		}
		for _, m := range payload.Messages {
			stored, ok := byOrdinal[m.Ordinal]
			if !ok {
				continue
			}
			key, err := rawMessageKey(m)
			if err != nil {
				return carry, err
			}
			targets = append(targets, rawCarryMessage{stored, key, cohort[0].Revision})
		}
	}
	snapshot, err := loadRawParitySnapshot(ctx, tx, legacy)
	if err != nil {
		return carry, err
	}
	messages := map[int]db.Message{}
	for _, m := range snapshot.Messages {
		messages[m.Ordinal] = m
	}
	rows, err := tx.QueryContext(ctx, `SELECT ordinal,source_uuid,COALESCE(note,''),created_at FROM pinned_messages WHERE session_id=$1 ORDER BY id`, legacy)
	if err != nil {
		return carry, err
	}
	defer rows.Close()
	for rows.Next() {
		var pin RawPinReference
		var uuid string
		if err = rows.Scan(&pin.Ordinal, &uuid, &pin.Note, &pin.CreatedAt); err != nil {
			return carry, err
		}
		message, ok := messages[pin.Ordinal]
		if !ok || uuid == "" || uuid != message.SourceUUID {
			carry.UnresolvedPins++
			continue
		}
		ref, err := matchRawLegacyPin(message, targets)
		if err != nil {
			return carry, err
		}
		if !ref.Resolved {
			carry.UnresolvedPins++
			continue
		}
		ref.Note, ref.CreatedAt = pin.Note, pin.CreatedAt
		carry.Pins = append(carry.Pins, ref)
	}
	return carry, rows.Err()
}

// applyRawLegacyCarry inserts only absent base values. Explicit false/null,
// unpins and every branch override survive. The caller publishes curation even
// when no immutable content or identity changed; legacy rows are never edited.
//
//nolint:unused // first production caller arrives with reversible activation
func (s *RawProjectionStore) applyRawLegacyCarry(ctx context.Context, tx *sql.Tx, carry rawLegacyCarry) error {
	for _, field := range []struct {
		name  string
		value any
	}{{"starred", carry.Starred}, {"display_name", carry.DisplayName}, {"trashed", carry.Trashed}} {
		encoded, err := json.Marshal(field.value)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO raw_curation(group_id,branch_id,field,value) VALUES($1,'',$2,$3::jsonb) ON CONFLICT(group_id,branch_id,field) DO NOTHING`, carry.Group, field.name, string(encoded)); err != nil {
			return err
		}
	}
	for _, p := range carry.Pins {
		if _, err := tx.ExecContext(ctx, `INSERT INTO raw_pins(group_id,branch_id,message_key,ordinal,content_revision,pinned,note,created_at) VALUES($1,'',$2,$3,$4,true,$5,$6) ON CONFLICT(group_id,branch_id,message_key) DO NOTHING`, carry.Group, p.MessageKey, p.Ordinal, p.ContentRevision, p.Note, p.CreatedAt); err != nil {
			return err
		}
	}
	return s.materializeRawCurationOnly(ctx, tx, carry.Group, carry.Cohorts)
}

// materializeRawCurationOnly operates on the prepared physical cohorts. Unlike
// materializeGroup it cannot reconcile prefixes, replace content, change source
// proofs or discover more session targets after a future activation fence.
//
//nolint:unused // called by applyRawLegacyCarry in the later activation slice
func (s *RawProjectionStore) materializeRawCurationOnly(ctx context.Context, tx *sql.Tx, group string, cohorts []rawCurationCohort) error {
	for _, c := range cohorts {
		if err := s.materializeCuration(ctx, tx, group, c.ID, c.Members); err != nil {
			return err
		}
	}
	return nil
}
