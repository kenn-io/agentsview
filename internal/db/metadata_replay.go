package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrMetadataTargetUnavailable means a metadata event depends on session or
// message content that is not durable locally yet.
var ErrMetadataTargetUnavailable = errors.New("metadata target unavailable")

// MetadataPinProjection identifies a pinned message during metadata replay.
type MetadataPinProjection struct {
	SourceUUID string
	Ordinal    int
	Note       *string
}

// MetadataProjection is one decoded artifact metadata event ready for replay.
type MetadataProjection struct {
	EventOrigin    string
	OrderKey       string
	HLC            string
	ArtifactHash   string
	SessionGID     string
	LocalSessionID string
	Field          string
	Op             string
	Value          string
	DisplayName    *string
	Pin            *MetadataPinProjection
}

// MetadataApplyResult summarizes how replay handled an event.
type MetadataApplyResult struct {
	Applied   bool
	Skipped   bool
	Conflict  bool
	Duplicate bool
}

// MetadataConflict is a losing metadata value recorded during deterministic
// replay.
type MetadataConflict struct {
	ID              int64  `json:"id"`
	SessionGID      string `json:"session_gid"`
	Field           string `json:"field"`
	WinningOrderKey string `json:"winning_order_key"`
	LosingOrderKey  string `json:"losing_order_key"`
	WinningOrigin   string `json:"winning_origin"`
	LosingOrigin    string `json:"losing_origin"`
	WinningOp       string `json:"winning_op"`
	LosingOp        string `json:"losing_op"`
	WinningValue    string `json:"winning_value"`
	LosingValue     string `json:"losing_value"`
	CreatedAt       string `json:"created_at"`
}

type metadataReplayState struct {
	OrderKey     string
	HLC          string
	ArtifactHash string
	Origin       string
	Op           string
	Value        string
}

// MetadataEventApplied reports whether an artifact metadata event was already
// durably handled.
func (db *DB) MetadataEventApplied(ctx context.Context, origin, orderKey string) (bool, error) {
	var exists int
	err := db.getReader().QueryRowContext(ctx,
		`SELECT 1 FROM metadata_applied_events
		 WHERE origin = ? AND order_key = ?`,
		origin, orderKey,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking metadata event %s/%s: %w", origin, orderKey, err)
	}
	return true, nil
}

// ListMetadataConflicts returns conflict rows for one or more global session
// identifiers.
func (db *DB) ListMetadataConflicts(
	ctx context.Context,
	sessionGIDs []string,
) ([]MetadataConflict, error) {
	ids := uniqueNonEmptyStrings(sessionGIDs)
	if len(ids) == 0 {
		return []MetadataConflict{}, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT id, session_gid, field, winning_order_key, losing_order_key,
		        winning_origin, losing_origin, winning_op, losing_op,
		        winning_value, losing_value, created_at
		 FROM metadata_conflicts
		 WHERE session_gid IN (`+placeholders+`)
		   AND winning_origin <> losing_origin
		 ORDER BY created_at DESC, id DESC`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("listing metadata conflicts: %w", err)
	}
	defer rows.Close()

	conflicts := []MetadataConflict{}
	for rows.Next() {
		var c MetadataConflict
		if err := rows.Scan(
			&c.ID, &c.SessionGID, &c.Field,
			&c.WinningOrderKey, &c.LosingOrderKey,
			&c.WinningOrigin, &c.LosingOrigin,
			&c.WinningOp, &c.LosingOp,
			&c.WinningValue, &c.LosingValue,
			&c.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning metadata conflict: %w", err)
		}
		conflicts = append(conflicts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating metadata conflicts: %w", err)
	}
	return conflicts, nil
}

// CountMetadataConflicts returns the total number of recorded metadata
// conflicts across all sessions.
func (db *DB) CountMetadataConflicts(ctx context.Context) (int, error) {
	var count int
	err := db.getReader().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM metadata_conflicts
		 WHERE winning_origin <> losing_origin`,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("counting metadata conflicts: %w", err)
	}
	return count, nil
}

// MarkMetadataEventApplied records a metadata event that was intentionally
// skipped, such as an unknown future op.
func (db *DB) MarkMetadataEventApplied(ctx context.Context, origin, orderKey, hash string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().ExecContext(ctx,
		`INSERT OR IGNORE INTO metadata_applied_events
			(origin, order_key, artifact_hash)
		 VALUES (?, ?, ?)`,
		origin, orderKey, hash,
	)
	if err != nil {
		return fmt.Errorf("marking metadata event %s/%s applied: %w", origin, orderKey, err)
	}
	return nil
}

// ApplyMetadataProjection applies one known metadata event if it wins the
// per-field LWW register, recording conflicts and the applied-event marker in
// the same transaction.
func (db *DB) ApplyMetadataProjection(
	ctx context.Context,
	ev MetadataProjection,
) (MetadataApplyResult, error) {
	return db.applyMetadataProjection(ctx, ev, true)
}

// RecordLocalMetadataProjection records the LWW register, conflict rows, and
// applied-event marker for a locally-originated metadata event whose session
// mutation the caller has already applied. It runs the same per-field LWW
// bookkeeping as replay but does not re-apply the mutation, so a later peer
// event with a lower order key cannot silently overwrite a newer local edit.
func (db *DB) RecordLocalMetadataProjection(
	ctx context.Context,
	ev MetadataProjection,
) (MetadataApplyResult, error) {
	return db.applyMetadataProjection(ctx, ev, false)
}

// MetadataReplayStateOp returns the current LWW operation recorded for a
// metadata field.
func (db *DB) MetadataReplayStateOp(
	ctx context.Context,
	sessionGID string,
	field string,
) (string, bool, error) {
	var op string
	err := db.getReader().QueryRowContext(ctx,
		`SELECT op FROM metadata_replay_state
		 WHERE session_gid = ? AND field = ?`,
		sessionGID, field,
	).Scan(&op)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading metadata replay state: %w", err)
	}
	return op, true, nil
}

func (db *DB) applyMetadataProjection(
	ctx context.Context,
	ev MetadataProjection,
	applyMutation bool,
) (MetadataApplyResult, error) {
	if ev.EventOrigin == "" || ev.OrderKey == "" || ev.ArtifactHash == "" {
		return MetadataApplyResult{}, errors.New("metadata projection event identity is required")
	}
	if ev.SessionGID == "" || ev.LocalSessionID == "" || ev.Field == "" || ev.Op == "" {
		return MetadataApplyResult{}, errors.New("metadata projection target is required")
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return MetadataApplyResult{}, fmt.Errorf("begin metadata replay tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	already, err := metadataEventAppliedTx(ctx, tx, ev.EventOrigin, ev.OrderKey)
	if err != nil {
		return MetadataApplyResult{}, err
	}
	if already {
		if err := tx.Commit(); err != nil {
			return MetadataApplyResult{}, fmt.Errorf("commit metadata replay duplicate: %w", err)
		}
		return MetadataApplyResult{Skipped: true, Duplicate: true}, nil
	}

	current, hasCurrent, err := metadataReplayStateTx(ctx, tx, ev.SessionGID, ev.Field)
	if err != nil {
		return MetadataApplyResult{}, err
	}
	result := MetadataApplyResult{}
	if hasCurrent && ev.OrderKey <= current.OrderKey {
		if metadataStateDiffers(current.Op, current.Value, ev.Op, ev.Value) &&
			metadataConflictOriginsDiffer(current.Origin, ev.EventOrigin) {
			if err := insertMetadataConflictTx(ctx, tx, metadataConflict{
				sessionGID:      ev.SessionGID,
				field:           ev.Field,
				winningOrderKey: current.OrderKey,
				losingOrderKey:  ev.OrderKey,
				winningOrigin:   current.Origin,
				losingOrigin:    ev.EventOrigin,
				winningOp:       current.Op,
				losingOp:        ev.Op,
				winningValue:    current.Value,
				losingValue:     ev.Value,
			}); err != nil {
				return MetadataApplyResult{}, err
			}
			result.Conflict = true
		}
		if err := markMetadataEventAppliedTx(ctx, tx, ev.EventOrigin, ev.OrderKey, ev.ArtifactHash); err != nil {
			return MetadataApplyResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return MetadataApplyResult{}, fmt.Errorf("commit metadata replay loser: %w", err)
		}
		result.Skipped = true
		return result, nil
	}

	if hasCurrent && metadataStateDiffers(current.Op, current.Value, ev.Op, ev.Value) &&
		metadataConflictOriginsDiffer(ev.EventOrigin, current.Origin) {
		if err := insertMetadataConflictTx(ctx, tx, metadataConflict{
			sessionGID:      ev.SessionGID,
			field:           ev.Field,
			winningOrderKey: ev.OrderKey,
			losingOrderKey:  current.OrderKey,
			winningOrigin:   ev.EventOrigin,
			losingOrigin:    current.Origin,
			winningOp:       ev.Op,
			losingOp:        current.Op,
			winningValue:    ev.Value,
			losingValue:     current.Value,
		}); err != nil {
			return MetadataApplyResult{}, err
		}
		result.Conflict = true
	}
	if applyMutation {
		if err := applyMetadataProjectionTx(ctx, tx, ev); err != nil {
			return MetadataApplyResult{}, err
		}
	}
	if err := upsertMetadataReplayStateTx(ctx, tx, ev); err != nil {
		return MetadataApplyResult{}, err
	}
	if err := markMetadataEventAppliedTx(ctx, tx, ev.EventOrigin, ev.OrderKey, ev.ArtifactHash); err != nil {
		return MetadataApplyResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return MetadataApplyResult{}, fmt.Errorf("commit metadata replay: %w", err)
	}
	result.Applied = true
	return result, nil
}

func metadataEventAppliedTx(ctx context.Context, tx *sql.Tx, origin, orderKey string) (bool, error) {
	var exists int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM metadata_applied_events
		 WHERE origin = ? AND order_key = ?`,
		origin, orderKey,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking metadata event %s/%s: %w", origin, orderKey, err)
	}
	return true, nil
}

func metadataReplayStateTx(
	ctx context.Context,
	tx *sql.Tx,
	sessionGID, field string,
) (metadataReplayState, bool, error) {
	var state metadataReplayState
	err := tx.QueryRowContext(ctx,
		`SELECT order_key, hlc, artifact_hash, origin, op, value
		 FROM metadata_replay_state
		 WHERE session_gid = ? AND field = ?`,
		sessionGID, field,
	).Scan(
		&state.OrderKey, &state.HLC, &state.ArtifactHash,
		&state.Origin, &state.Op, &state.Value,
	)
	if err == sql.ErrNoRows {
		return metadataReplayState{}, false, nil
	}
	if err != nil {
		return metadataReplayState{}, false, fmt.Errorf("reading metadata replay state: %w", err)
	}
	return state, true, nil
}

func applyMetadataProjectionTx(ctx context.Context, tx *sql.Tx, ev MetadataProjection) error {
	switch ev.Op {
	case "rename":
		if err := requireMetadataSessionTx(ctx, tx, ev.LocalSessionID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions
			 SET display_name = ?,
			     local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			 WHERE id = ?`,
			ev.DisplayName, ev.LocalSessionID,
		)
		return err
	case "soft_delete":
		if err := requireMetadataSessionTx(ctx, tx, ev.LocalSessionID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions
			 SET deleted_at = COALESCE(deleted_at, strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			     local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			 WHERE id = ?`,
			ev.LocalSessionID,
		)
		return err
	case "restore":
		if err := requireMetadataSessionTx(ctx, tx, ev.LocalSessionID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions
			 SET deleted_at = NULL,
			     local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			 WHERE id = ?`,
			ev.LocalSessionID,
		)
		return err
	case "star":
		if err := requireMetadataSessionTx(ctx, tx, ev.LocalSessionID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO starred_sessions (session_id)
			 VALUES (?)`,
			ev.LocalSessionID,
		)
		return err
	case "unstar":
		if err := requireMetadataSessionTx(ctx, tx, ev.LocalSessionID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			`DELETE FROM starred_sessions WHERE session_id = ?`,
			ev.LocalSessionID,
		)
		return err
	case "pin":
		if err := requireMetadataSessionTx(ctx, tx, ev.LocalSessionID); err != nil {
			return err
		}
		if ev.Pin == nil {
			return errors.New("pin metadata event missing pin payload")
		}
		msg, ok, err := metadataPinTargetTx(ctx, tx, ev.LocalSessionID, *ev.Pin)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: pin target %s ordinal %d",
				ErrMetadataTargetUnavailable, ev.LocalSessionID, ev.Pin.Ordinal)
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO pinned_messages (session_id, message_id, ordinal, note)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(session_id, message_id) DO UPDATE SET note = excluded.note`,
			ev.LocalSessionID, msg.id, msg.ordinal, ev.Pin.Note,
		)
		return err
	case "unpin":
		if err := requireMetadataSessionTx(ctx, tx, ev.LocalSessionID); err != nil {
			return err
		}
		if ev.Pin == nil {
			return errors.New("unpin metadata event missing pin payload")
		}
		return unpinMetadataTx(ctx, tx, ev.LocalSessionID, *ev.Pin)
	case "purge":
		aliasIDs, err := sessionAliasIDsTx(tx, "id = ?", ev.LocalSessionID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO excluded_sessions (id) VALUES (?)`,
			ev.LocalSessionID,
		); err != nil {
			return err
		}
		for _, aliasID := range aliasIDs {
			if err := excludeSessionIDTx(tx, aliasID); err != nil {
				return fmt.Errorf("excluding metadata purge alias %s: %w", aliasID, err)
			}
		}
		_, err = tx.ExecContext(ctx,
			`DELETE FROM sessions WHERE id = ?`,
			ev.LocalSessionID,
		)
		return err
	default:
		return fmt.Errorf("unsupported metadata op %q", ev.Op)
	}
}

func requireMetadataSessionTx(ctx context.Context, tx *sql.Tx, id string) error {
	var exists int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM sessions WHERE id = ?`,
		id,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: session %s", ErrMetadataTargetUnavailable, id)
	}
	if err != nil {
		return fmt.Errorf("checking metadata session %s: %w", id, err)
	}
	return nil
}

type metadataPinTarget struct {
	id      int64
	ordinal int
}

func metadataPinTargetTx(
	ctx context.Context,
	tx *sql.Tx,
	sessionID string,
	pin MetadataPinProjection,
) (metadataPinTarget, bool, error) {
	if pin.SourceUUID != "" {
		target, ok, err := metadataPinTargetByQueryTx(ctx, tx,
			`SELECT id, ordinal FROM messages
			 WHERE session_id = ? AND source_uuid = ?
			 ORDER BY ordinal LIMIT 1`,
			sessionID, pin.SourceUUID,
		)
		if err != nil || ok {
			return target, ok, err
		}
	}
	return metadataPinTargetByQueryTx(ctx, tx,
		`SELECT id, ordinal FROM messages
		 WHERE session_id = ? AND ordinal = ?
		 ORDER BY id LIMIT 1`,
		sessionID, pin.Ordinal,
	)
}

func metadataPinTargetByQueryTx(
	ctx context.Context,
	tx *sql.Tx,
	query string,
	args ...any,
) (metadataPinTarget, bool, error) {
	var target metadataPinTarget
	err := tx.QueryRowContext(ctx, query, args...).Scan(&target.id, &target.ordinal)
	if err == sql.ErrNoRows {
		return metadataPinTarget{}, false, nil
	}
	if err != nil {
		return metadataPinTarget{}, false, fmt.Errorf("finding metadata pin target: %w", err)
	}
	return target, true, nil
}

func unpinMetadataTx(
	ctx context.Context,
	tx *sql.Tx,
	sessionID string,
	pin MetadataPinProjection,
) error {
	if pin.SourceUUID != "" {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM pinned_messages
			 WHERE session_id = ?
			   AND message_id IN (
			     SELECT id FROM messages
			     WHERE session_id = ? AND source_uuid = ?
			   )`,
			sessionID, sessionID, pin.SourceUUID,
		)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
	}
	_, err := tx.ExecContext(ctx,
		`DELETE FROM pinned_messages
		 WHERE session_id = ?
		   AND message_id IN (
		     SELECT id FROM messages
		     WHERE session_id = ? AND ordinal = ?
		   )`,
		sessionID, sessionID, pin.Ordinal,
	)
	return err
}

func upsertMetadataReplayStateTx(ctx context.Context, tx *sql.Tx, ev MetadataProjection) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO metadata_replay_state
			(session_gid, field, order_key, hlc, artifact_hash, origin, op, value, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		 ON CONFLICT(session_gid, field) DO UPDATE SET
			order_key = excluded.order_key,
			hlc = excluded.hlc,
			artifact_hash = excluded.artifact_hash,
			origin = excluded.origin,
			op = excluded.op,
			value = excluded.value,
			updated_at = excluded.updated_at`,
		ev.SessionGID, ev.Field, ev.OrderKey, ev.HLC, ev.ArtifactHash,
		ev.EventOrigin, ev.Op, ev.Value,
	)
	if err != nil {
		return fmt.Errorf("upserting metadata replay state: %w", err)
	}
	return nil
}

func markMetadataEventAppliedTx(
	ctx context.Context,
	tx *sql.Tx,
	origin, orderKey, hash string,
) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO metadata_applied_events
			(origin, order_key, artifact_hash)
		 VALUES (?, ?, ?)`,
		origin, orderKey, hash,
	)
	if err != nil {
		return fmt.Errorf("marking metadata event %s/%s applied: %w", origin, orderKey, err)
	}
	return nil
}

type metadataConflict struct {
	sessionGID      string
	field           string
	winningOrderKey string
	losingOrderKey  string
	winningOrigin   string
	losingOrigin    string
	winningOp       string
	losingOp        string
	winningValue    string
	losingValue     string
}

func insertMetadataConflictTx(ctx context.Context, tx *sql.Tx, c metadataConflict) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO metadata_conflicts
			(session_gid, field, winning_order_key, losing_order_key,
			 winning_origin, losing_origin, winning_op, losing_op,
			 winning_value, losing_value)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.sessionGID, c.field, c.winningOrderKey, c.losingOrderKey,
		c.winningOrigin, c.losingOrigin, c.winningOp, c.losingOp,
		c.winningValue, c.losingValue,
	)
	if err != nil {
		return fmt.Errorf("inserting metadata conflict: %w", err)
	}
	return nil
}

func metadataStateDiffers(aOp, aValue, bOp, bValue string) bool {
	return aOp != bOp || aValue != bValue
}

func metadataConflictOriginsDiffer(winningOrigin, losingOrigin string) bool {
	return winningOrigin != losingOrigin
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	return unique
}
