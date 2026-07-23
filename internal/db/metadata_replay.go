package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrMetadataTargetUnavailable means a metadata event depends on session or
// message content that is not durable locally yet.
var ErrMetadataTargetUnavailable = errors.New("metadata target unavailable")

// MetadataPinProjection identifies a pinned message during metadata replay.
type MetadataPinProjection struct {
	SourceUUID string  `json:"source_uuid,omitempty"`
	Ordinal    int     `json:"ordinal"`
	Note       *string `json:"note,omitempty"`
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

// MetadataEventIdentity identifies one immutable artifact metadata event.
type MetadataEventIdentity struct {
	Origin   string
	OrderKey string
}

const metadataReplayWinnerPageSize = 128

// VisitMetadataReplayWinnersAuthoredBy visits the current per-field replay
// winners whose winning event was authored by origin. Keyset pages are fully
// read and closed before visit runs, so callers may write new canonical events
// without retaining the read cursor or materializing all replay state.
func (db *DB) VisitMetadataReplayWinnersAuthoredBy(
	ctx context.Context,
	origin string,
	visit func(MetadataProjection) error,
) error {
	if strings.TrimSpace(origin) == "" {
		return errors.New("metadata replay winner origin is required")
	}
	if visit == nil {
		return errors.New("metadata replay winner visitor is required")
	}
	afterSessionGID, afterField := "", ""
	for {
		rows, err := db.getReader().QueryContext(ctx,
			`SELECT session_gid, field, order_key, hlc, artifact_hash, origin, op, value
			 FROM metadata_replay_state
			 WHERE origin = ?
			   AND (session_gid > ? OR (session_gid = ? AND field > ?))
			 ORDER BY session_gid, field
			 LIMIT ?`,
			origin, afterSessionGID, afterSessionGID, afterField,
			metadataReplayWinnerPageSize,
		)
		if err != nil {
			return fmt.Errorf("listing metadata replay winners for %s: %w", origin, err)
		}
		winners := make([]MetadataProjection, 0, metadataReplayWinnerPageSize)
		for rows.Next() {
			var sessionGID, field string
			var state metadataReplayState
			if err := rows.Scan(
				&sessionGID, &field, &state.OrderKey, &state.HLC,
				&state.ArtifactHash, &state.Origin, &state.Op, &state.Value,
			); err != nil {
				rows.Close()
				return fmt.Errorf("scanning metadata replay winner: %w", err)
			}
			winner, err := metadataReplayStateProjection(sessionGID, sessionGID, field, state)
			if err != nil {
				rows.Close()
				return err
			}
			winners = append(winners, winner)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if err := errors.Join(rowsErr, closeErr); err != nil {
			return fmt.Errorf("iterating metadata replay winners: %w", err)
		}
		if len(winners) == 0 {
			return nil
		}
		for _, winner := range winners {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(winner); err != nil {
				return err
			}
		}
		last := winners[len(winners)-1]
		afterSessionGID, afterField = last.SessionGID, last.Field
		if len(winners) < metadataReplayWinnerPageSize {
			return nil
		}
	}
}

// MetadataArtifactProvenance is the point-read index for one validated
// metadata artifact. It is recorded independently of visible replay state so
// local bookkeeping repair never has to scan the append-only ledger.
type MetadataArtifactProvenance struct {
	Origin       string
	OrderKey     string
	ArtifactHash string
	SessionGID   string
	Op           string
}

// RecordMetadataArtifactProvenance idempotently records one immutable event
// identity and rejects a different hash, session, or operation at the same
// origin/order key.
func (db *DB) RecordMetadataArtifactProvenance(
	ctx context.Context, provenance MetadataArtifactProvenance,
) error {
	if provenance.Origin == "" || provenance.OrderKey == "" ||
		provenance.ArtifactHash == "" || provenance.SessionGID == "" || provenance.Op == "" {
		return errors.New("complete metadata artifact provenance is required")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	result, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO metadata_artifact_provenance(
			origin, order_key, artifact_hash, session_gid, op
		) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(origin, order_key) DO UPDATE SET
			artifact_hash = excluded.artifact_hash,
			session_gid = excluded.session_gid,
			op = excluded.op
		WHERE metadata_artifact_provenance.artifact_hash = excluded.artifact_hash
		  AND metadata_artifact_provenance.session_gid = excluded.session_gid
		  AND metadata_artifact_provenance.op = excluded.op`,
		provenance.Origin, provenance.OrderKey, provenance.ArtifactHash,
		provenance.SessionGID, provenance.Op,
	)
	if err != nil {
		return fmt.Errorf("recording metadata artifact provenance: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading metadata artifact provenance result: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("metadata artifact provenance %s/%s conflicts with its immutable identity",
			provenance.Origin, provenance.OrderKey)
	}
	return nil
}

// MetadataArtifactProvenanceForSession returns only one origin/session's
// indexed events, optionally filtered by operation, in ledger order.
func (db *DB) MetadataArtifactProvenanceForSession(
	ctx context.Context, origin, sessionGID string, ops ...string,
) ([]MetadataArtifactProvenance, error) {
	if origin == "" || sessionGID == "" {
		return nil, errors.New("metadata artifact provenance origin and session are required")
	}
	query := `SELECT origin, order_key, artifact_hash, session_gid, op
		FROM metadata_artifact_provenance WHERE origin = ? AND session_gid = ?`
	args := []any{origin, sessionGID}
	if len(ops) > 0 {
		query += " AND op IN (" + strings.TrimRight(strings.Repeat("?,", len(ops)), ",") + ")"
		for _, op := range ops {
			args = append(args, op)
		}
	}
	query += " ORDER BY order_key"
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing metadata artifact provenance: %w", err)
	}
	defer rows.Close()
	var result []MetadataArtifactProvenance
	for rows.Next() {
		var provenance MetadataArtifactProvenance
		if err := rows.Scan(&provenance.Origin, &provenance.OrderKey,
			&provenance.ArtifactHash, &provenance.SessionGID, &provenance.Op); err != nil {
			return nil, fmt.Errorf("scanning metadata artifact provenance: %w", err)
		}
		result = append(result, provenance)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating metadata artifact provenance: %w", err)
	}
	return result, nil
}

// MetadataAppliedEventIdentities bulk-loads the events already durably handled
// by metadata replay.
func (db *DB) MetadataAppliedEventIdentities(
	ctx context.Context,
) (map[MetadataEventIdentity]struct{}, error) {
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT origin, order_key FROM metadata_applied_events`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing applied metadata events: %w", err)
	}
	defer rows.Close()

	identities := make(map[MetadataEventIdentity]struct{})
	for rows.Next() {
		var identity MetadataEventIdentity
		if err := rows.Scan(&identity.Origin, &identity.OrderKey); err != nil {
			return nil, fmt.Errorf("scanning applied metadata event: %w", err)
		}
		identities[identity] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating applied metadata events: %w", err)
	}
	return identities, nil
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

// ReapplyMetadataReplayState reapplies the current visible metadata projection
// for a session from the durable replay register. It does not alter LWW state or
// applied-event markers; it only repairs fields that content import may have
// overwritten or invalidated while replacing session/message rows.
func (db *DB) ReapplyMetadataReplayState(
	ctx context.Context,
	sessionGID string,
	localSessionID string,
) (int, error) {
	if err := db.requireWritable(); err != nil {
		return 0, err
	}
	if strings.TrimSpace(sessionGID) == "" || strings.TrimSpace(localSessionID) == "" {
		return 0, errors.New("metadata replay session id is required")
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin metadata reapply tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT field, order_key, hlc, artifact_hash, origin, op, value
		 FROM metadata_replay_state
		 WHERE session_gid = ?
		 ORDER BY field`,
		sessionGID,
	)
	if err != nil {
		return 0, fmt.Errorf("reading metadata replay state: %w", err)
	}
	projections := []MetadataProjection{}
	for rows.Next() {
		var field string
		var state metadataReplayState
		if err := rows.Scan(
			&field, &state.OrderKey, &state.HLC, &state.ArtifactHash,
			&state.Origin, &state.Op, &state.Value,
		); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scanning metadata replay state: %w", err)
		}
		ev, err := metadataReplayStateProjection(sessionGID, localSessionID, field, state)
		if err != nil {
			rows.Close()
			return 0, err
		}
		projections = append(projections, ev)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("closing metadata replay state rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating metadata replay state: %w", err)
	}

	applied := 0
	for _, ev := range projections {
		if err := ctx.Err(); err != nil {
			return applied, err
		}
		if err := applyMetadataProjectionTx(ctx, tx, ev); err != nil {
			if errors.Is(err, ErrMetadataTargetUnavailable) {
				continue
			}
			return applied, fmt.Errorf("reapplying metadata replay state: %w", err)
		}
		applied++
	}
	if err := tx.Commit(); err != nil {
		return applied, fmt.Errorf("commit metadata reapply tx: %w", err)
	}
	return applied, nil
}

func recordMetadataArtifactProvenanceTx(
	ctx context.Context, tx *sql.Tx, provenance MetadataArtifactProvenance,
) error {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO metadata_artifact_provenance(
			origin, order_key, artifact_hash, session_gid, op
		) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(origin, order_key) DO UPDATE SET
			artifact_hash = excluded.artifact_hash,
			session_gid = excluded.session_gid,
			op = excluded.op
		WHERE metadata_artifact_provenance.artifact_hash = excluded.artifact_hash
		  AND metadata_artifact_provenance.session_gid = excluded.session_gid
		  AND metadata_artifact_provenance.op = excluded.op`,
		provenance.Origin, provenance.OrderKey, provenance.ArtifactHash,
		provenance.SessionGID, provenance.Op,
	)
	if err != nil {
		return fmt.Errorf("recording metadata artifact provenance: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("reading metadata artifact provenance result: %w", err)
	}
	if rows != 1 {
		return fmt.Errorf("metadata artifact provenance %s/%s conflicts with its immutable identity",
			provenance.Origin, provenance.OrderKey)
	}
	return nil
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
	if err := recordMetadataArtifactProvenanceTx(ctx, tx, MetadataArtifactProvenance{
		Origin: ev.EventOrigin, OrderKey: ev.OrderKey, ArtifactHash: ev.ArtifactHash,
		SessionGID: ev.SessionGID, Op: ev.Op,
	}); err != nil {
		return MetadataApplyResult{}, err
	}

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

func metadataReplayStateProjection(
	sessionGID string,
	localSessionID string,
	field string,
	state metadataReplayState,
) (MetadataProjection, error) {
	ev := MetadataProjection{
		EventOrigin:    state.Origin,
		OrderKey:       state.OrderKey,
		HLC:            state.HLC,
		ArtifactHash:   state.ArtifactHash,
		SessionGID:     sessionGID,
		LocalSessionID: localSessionID,
		Field:          field,
		Op:             state.Op,
		Value:          state.Value,
	}
	switch state.Op {
	case "rename":
		var payload struct {
			DisplayName *string `json:"display_name"`
		}
		if err := json.Unmarshal([]byte(state.Value), &payload); err != nil {
			return MetadataProjection{}, fmt.Errorf("decoding rename metadata replay state: %w", err)
		}
		ev.DisplayName = payload.DisplayName
	case "pin", "unpin":
		var pin MetadataPinProjection
		if err := json.Unmarshal([]byte(state.Value), &pin); err != nil {
			return MetadataProjection{}, fmt.Errorf("decoding pin metadata replay state: %w", err)
		}
		ev.Pin = &pin
	}
	return ev, nil
}

// metadataEventWallTime renders a replayed event's HLC wall-clock portion in
// the sessions.deleted_at column format, so trash retention is anchored to
// when the deletion happened on the authoring machine rather than when each
// peer imported the event. The HLC shape ("<wall>-<20-digit logical>", wall
// layout without ":" separators) is pinned by the artifact format contract in
// internal/artifact. Returns "" when the HLC has no parseable wall portion.
func metadataEventWallTime(hlc string) string {
	idx := strings.LastIndex(hlc, "-")
	if idx <= 0 {
		return ""
	}
	wall, err := time.Parse("2006-01-02T150405.000000000Z", hlc[:idx])
	if err != nil {
		return ""
	}
	return wall.UTC().Format("2006-01-02T15:04:05.000Z")
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
			 SET deleted_at = COALESCE(deleted_at, NULLIF(?, ''), strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			     local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			 WHERE id = ?`,
			metadataEventWallTime(ev.HLC), ev.LocalSessionID,
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
		return applyMetadataPinTx(ctx, tx, ev.LocalSessionID, *ev.Pin)
	case "unpin":
		if err := requireMetadataSessionTx(ctx, tx, ev.LocalSessionID); err != nil {
			return err
		}
		if ev.Pin == nil {
			return errors.New("unpin metadata event missing pin payload")
		}
		return unpinMetadataTx(ctx, tx, ev.LocalSessionID, *ev.Pin)
	case "purge":
		return applyMetadataPurgeTx(ctx, tx, ev.LocalSessionID)
	default:
		return fmt.Errorf("unsupported metadata op %q", ev.Op)
	}
}

func applyMetadataPinTx(ctx context.Context, tx *sql.Tx, sessionID string, pin MetadataPinProjection) error {
	msg, ok, err := metadataPinTargetTx(ctx, tx, sessionID, pin)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: pin target %s ordinal %d",
			ErrMetadataTargetUnavailable, sessionID, pin.Ordinal)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO pinned_messages (session_id, message_id, ordinal, note)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(session_id, message_id) DO UPDATE SET note = excluded.note`,
		sessionID, msg.id, msg.ordinal, pin.Note,
	)
	return err
}

func applyMetadataPurgeTx(ctx context.Context, tx *sql.Tx, sessionID string) error {
	aliasIDs, err := sessionAliasIDsTx(tx, "id = ?", sessionID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO excluded_sessions (id) VALUES (?)`,
		sessionID,
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
		sessionID,
	)
	return err
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
