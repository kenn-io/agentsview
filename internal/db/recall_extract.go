package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Extraction generation states. A generation is one distillation
// configuration's corpus; at most one is active at a time.
const (
	ExtractGenerationBuilding = "building"
	ExtractGenerationActive   = "active"
	ExtractGenerationRetired  = "retired"
)

// Extraction progress states for one (session, generation) pair.
const (
	ExtractProgressPending = "pending"
	ExtractProgressPartial = "partial"
	ExtractProgressDone    = "done"
	ExtractProgressFailed  = "failed"
)

// ErrStaleExtractProgress reports a cursor or failure update that no longer
// matches the stored row: the session's content digest changed (the row was
// reset for re-extraction) or the cursor would regress. Workers treat it as
// "re-read the row and re-derive units", never as data loss.
var ErrStaleExtractProgress = errors.New("extract progress is stale")

// ExtractGeneration is one row of the extraction generation registry.
type ExtractGeneration struct {
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
	Model       string `json:"model"`
	Segmenter   string `json:"segmenter"`
	ParamsJSON  string `json:"params_json"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// ExtractProgress is the resume state for one session under one generation.
// UnitCursor counts completed units of the session's deterministic unit
// list; a restart resumes at the cursor instead of re-extracting.
type ExtractProgress struct {
	SessionID             string `json:"session_id"`
	GenerationFingerprint string `json:"generation_fingerprint"`
	UnitCursor            int    `json:"unit_cursor"`
	UnitsTotal            int    `json:"units_total"`
	State                 string `json:"state"`
	ContentDigest         string `json:"content_digest"`
	LastError             string `json:"last_error,omitempty"`
	UpdatedAt             string `json:"updated_at"`
}

// EnsureExtractGeneration registers a generation if its fingerprint is new
// and returns the stored row. An existing fingerprint wins: the caller's
// metadata is ignored so a re-registration can never mutate a corpus's
// recorded identity.
func (db *DB) EnsureExtractGeneration(
	ctx context.Context, gen ExtractGeneration,
) (ExtractGeneration, error) {
	var zero ExtractGeneration
	if err := db.requireWritable(); err != nil {
		return zero, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	gen.Fingerprint = strings.TrimSpace(gen.Fingerprint)
	if gen.Fingerprint == "" {
		return zero, fmt.Errorf("extract generation fingerprint is required")
	}
	if strings.TrimSpace(gen.Model) == "" {
		return zero, fmt.Errorf("extract generation model is required")
	}
	if strings.TrimSpace(gen.Segmenter) == "" {
		return zero, fmt.Errorf("extract generation segmenter is required")
	}
	if gen.ParamsJSON == "" {
		gen.ParamsJSON = "{}"
	}
	_, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO recall_extract_generations
			(fingerprint, state, model, segmenter, params_json)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(fingerprint) DO NOTHING`,
		gen.Fingerprint, ExtractGenerationBuilding,
		gen.Model, gen.Segmenter, gen.ParamsJSON,
	)
	if err != nil {
		return zero, fmt.Errorf(
			"registering extract generation %s: %w", gen.Fingerprint, err,
		)
	}
	return db.extractGenerationByFingerprint(ctx, gen.Fingerprint)
}

// ExtractGenerations lists all registered generations, newest first.
func (db *DB) ExtractGenerations(
	ctx context.Context,
) ([]ExtractGeneration, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT fingerprint, state, model, segmenter, params_json,
		       created_at, updated_at
		FROM recall_extract_generations
		ORDER BY created_at DESC, fingerprint`)
	if err != nil {
		return nil, fmt.Errorf("listing extract generations: %w", err)
	}
	defer rows.Close()
	var generations []ExtractGeneration
	for rows.Next() {
		var gen ExtractGeneration
		if err := rows.Scan(
			&gen.Fingerprint, &gen.State, &gen.Model, &gen.Segmenter,
			&gen.ParamsJSON, &gen.CreatedAt, &gen.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning extract generation: %w", err)
		}
		generations = append(generations, gen)
	}
	return generations, rows.Err()
}

// ActivateExtractGeneration makes the identified generation active and
// retires whichever generation was previously active, in one transaction so
// two generations can never be active simultaneously.
func (db *DB) ActivateExtractGeneration(
	ctx context.Context, fingerprint string,
) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning generation activation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.ExecContext(ctx, `
		UPDATE recall_extract_generations
		SET state = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE fingerprint = ?`,
		ExtractGenerationActive, fingerprint,
	)
	if err != nil {
		return fmt.Errorf("activating generation %s: %w", fingerprint, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("activating generation %s: %w", fingerprint, err)
	}
	if affected == 0 {
		return fmt.Errorf("extract generation %s not found", fingerprint)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE recall_extract_generations
		SET state = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE state = ? AND fingerprint != ?`,
		ExtractGenerationRetired, ExtractGenerationActive, fingerprint,
	); err != nil {
		return fmt.Errorf("retiring previous active generation: %w", err)
	}
	return tx.Commit()
}

// RetireExtractGeneration retires a generation. Retiring the active
// generation is refused without force, because it leaves recall with no
// machine-distilled corpus to serve.
func (db *DB) RetireExtractGeneration(
	ctx context.Context, fingerprint string, force bool,
) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	// One conditional statement so the active-state check and the state
	// change cannot interleave with a concurrent activation.
	result, err := db.getWriter().ExecContext(ctx, `
		UPDATE recall_extract_generations
		SET state = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE fingerprint = ? AND (? OR state != ?)`,
		ExtractGenerationRetired, fingerprint, force, ExtractGenerationActive,
	)
	if err != nil {
		return fmt.Errorf("retiring generation %s: %w", fingerprint, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("retiring generation %s: %w", fingerprint, err)
	}
	if affected > 0 {
		return nil
	}
	if _, err := db.extractGenerationByFingerprint(ctx, fingerprint); err != nil {
		return err
	}
	return fmt.Errorf(
		"generation %s is active; retiring it leaves no distilled corpus "+
			"to serve (use force to retire anyway)", fingerprint,
	)
}

func (db *DB) extractGenerationByFingerprint(
	ctx context.Context, fingerprint string,
) (ExtractGeneration, error) {
	var gen ExtractGeneration
	err := db.getReader().QueryRowContext(ctx, `
		SELECT fingerprint, state, model, segmenter, params_json,
		       created_at, updated_at
		FROM recall_extract_generations
		WHERE fingerprint = ?`, fingerprint,
	).Scan(
		&gen.Fingerprint, &gen.State, &gen.Model, &gen.Segmenter,
		&gen.ParamsJSON, &gen.CreatedAt, &gen.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return gen, fmt.Errorf("extract generation %s not found", fingerprint)
	}
	if err != nil {
		return gen, fmt.Errorf("reading generation %s: %w", fingerprint, err)
	}
	return gen, nil
}

// UpsertExtractProgress ensures a progress row exists for the session under
// the generation. A matching content digest keeps existing progress; a
// changed digest resets the row to pending at cursor zero so the grown
// session is re-segmented and topped up. A session with zero units has
// nothing to extract, so its row lands directly in done — no worker will
// ever advance a cursor over an empty unit list.
func (db *DB) UpsertExtractProgress(
	ctx context.Context,
	sessionID, fingerprint, contentDigest string,
	unitsTotal int,
) (ExtractProgress, error) {
	var zero ExtractProgress
	if err := db.requireWritable(); err != nil {
		return zero, err
	}
	if unitsTotal < 0 {
		return zero, fmt.Errorf(
			"units total %d for session %s must not be negative",
			unitsTotal, sessionID,
		)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	initialState := ExtractProgressPending
	if unitsTotal == 0 {
		initialState = ExtractProgressDone
	}
	_, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO recall_extract_progress
			(session_id, generation_fingerprint, unit_cursor, units_total,
			 state, content_digest)
		VALUES (?, ?, 0, ?, ?, ?)
		ON CONFLICT(session_id, generation_fingerprint) DO UPDATE SET
			unit_cursor = CASE
				WHEN recall_extract_progress.content_digest = excluded.content_digest
				THEN recall_extract_progress.unit_cursor ELSE 0 END,
			units_total = CASE
				WHEN recall_extract_progress.content_digest = excluded.content_digest
				THEN recall_extract_progress.units_total ELSE excluded.units_total END,
			state = CASE
				WHEN recall_extract_progress.content_digest = excluded.content_digest
				THEN recall_extract_progress.state ELSE ? END,
			content_digest = excluded.content_digest,
			last_error = CASE
				WHEN recall_extract_progress.content_digest = excluded.content_digest
				THEN recall_extract_progress.last_error ELSE '' END,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		sessionID, fingerprint, unitsTotal, initialState,
		contentDigest, initialState,
	)
	if err != nil {
		return zero, fmt.Errorf(
			"upserting extract progress for session %s: %w", sessionID, err,
		)
	}
	progress, ok, err := db.ExtractProgress(ctx, sessionID, fingerprint)
	if err != nil {
		return zero, err
	}
	if !ok {
		return zero, fmt.Errorf(
			"extract progress for session %s vanished after upsert", sessionID,
		)
	}
	return progress, nil
}

// AdvanceExtractCursor records that units before cursor are complete,
// marking the row done when the cursor reaches the unit total. The update
// only applies when expectedDigest still matches the row and the cursor
// moves forward within bounds, so a worker that raced a digest reset gets
// ErrStaleExtractProgress instead of overwriting fresh state.
func (db *DB) AdvanceExtractCursor(
	ctx context.Context,
	sessionID, fingerprint, expectedDigest string,
	cursor int,
) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := db.getWriter().ExecContext(ctx, `
		UPDATE recall_extract_progress
		SET unit_cursor = ?,
			state = CASE WHEN ? >= units_total THEN ? ELSE ? END,
			last_error = '',
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE session_id = ? AND generation_fingerprint = ?
		  AND content_digest = ?
		  AND unit_cursor <= ?
		  AND ? <= units_total`,
		cursor, cursor, ExtractProgressDone, ExtractProgressPartial,
		sessionID, fingerprint, expectedDigest, cursor, cursor,
	)
	if err != nil {
		return fmt.Errorf(
			"advancing extract cursor for session %s: %w", sessionID, err,
		)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(
			"advancing extract cursor for session %s: %w", sessionID, err,
		)
	}
	if affected > 0 {
		return nil
	}
	progress, ok, err := db.ExtractProgress(ctx, sessionID, fingerprint)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf(
			"no extract progress row for session %s under generation %s",
			sessionID, fingerprint,
		)
	}
	if cursor > progress.UnitsTotal {
		return fmt.Errorf(
			"cursor %d exceeds %d units for session %s",
			cursor, progress.UnitsTotal, sessionID,
		)
	}
	return fmt.Errorf(
		"advancing session %s past digest change or cursor regression: %w",
		sessionID, ErrStaleExtractProgress,
	)
}

// MarkExtractProgressFailed records a failure without losing the resume
// point: the cursor stays where it was so a retry continues, not restarts.
// A row that already reached done is left untouched: the failure comes from
// a worker that raced completion, not from the completed extraction.
func (db *DB) MarkExtractProgressFailed(
	ctx context.Context,
	sessionID, fingerprint, expectedDigest, lastError string,
) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := db.getWriter().ExecContext(ctx, `
		UPDATE recall_extract_progress
		SET state = ?, last_error = ?,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE session_id = ? AND generation_fingerprint = ?
		  AND content_digest = ?
		  AND state != ?`,
		ExtractProgressFailed, lastError, sessionID, fingerprint,
		expectedDigest, ExtractProgressDone,
	)
	if err != nil {
		return fmt.Errorf(
			"marking extract progress failed for session %s: %w",
			sessionID, err,
		)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(
			"marking extract progress failed for session %s: %w",
			sessionID, err,
		)
	}
	if affected > 0 {
		return nil
	}
	progress, ok, err := db.ExtractProgress(ctx, sessionID, fingerprint)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf(
			"no extract progress row for session %s under generation %s",
			sessionID, fingerprint,
		)
	}
	if progress.State == ExtractProgressDone {
		return fmt.Errorf(
			"session %s is already done under generation %s: %w",
			sessionID, fingerprint, ErrStaleExtractProgress,
		)
	}
	return fmt.Errorf(
		"failing session %s past digest change: %w",
		sessionID, ErrStaleExtractProgress,
	)
}

// ExtractProgress reads the progress row for one session and generation.
func (db *DB) ExtractProgress(
	ctx context.Context, sessionID, fingerprint string,
) (ExtractProgress, bool, error) {
	var progress ExtractProgress
	if ctx == nil {
		ctx = context.Background()
	}
	err := db.getReader().QueryRowContext(ctx, `
		SELECT session_id, generation_fingerprint, unit_cursor, units_total,
		       state, content_digest, last_error, updated_at
		FROM recall_extract_progress
		WHERE session_id = ? AND generation_fingerprint = ?`,
		sessionID, fingerprint,
	).Scan(
		&progress.SessionID, &progress.GenerationFingerprint,
		&progress.UnitCursor, &progress.UnitsTotal, &progress.State,
		&progress.ContentDigest, &progress.LastError, &progress.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return progress, false, nil
	}
	if err != nil {
		return progress, false, fmt.Errorf(
			"reading extract progress for session %s: %w", sessionID, err,
		)
	}
	return progress, true, nil
}

// copyRecallExtractStateFromAttachedTx carries the extraction generation
// registry and per-session resume cursors across a full resync. Without it a
// rebuild silently discards the active generation and every cursor, forcing
// a full re-extraction. Progress rows are filtered to sessions present in
// the rebuilt DB, mirroring the entry copy. Archives written by releases
// without these tables are tolerated.
func copyRecallExtractStateFromAttachedTx(
	ctx context.Context, tx *sql.Tx,
) error {
	generationsExist, err := attachedRecallTableExistsTx(
		ctx, tx, "recall_extract_generations",
	)
	if err != nil {
		return err
	}
	if !generationsExist {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO recall_extract_generations (
			fingerprint, state, model, segmenter, params_json,
			created_at, updated_at
		)
		SELECT fingerprint, state, model, segmenter, params_json,
		       created_at, updated_at
		FROM old_db.recall_extract_generations`); err != nil {
		return fmt.Errorf("copying extract generations: %w", err)
	}
	progressExists, err := attachedRecallTableExistsTx(
		ctx, tx, "recall_extract_progress",
	)
	if err != nil {
		return err
	}
	if !progressExists {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO recall_extract_progress (
			session_id, generation_fingerprint, unit_cursor, units_total,
			state, content_digest, last_error, updated_at
		)
		SELECT session_id, generation_fingerprint, unit_cursor, units_total,
		       state, content_digest, last_error, updated_at
		FROM old_db.recall_extract_progress
		WHERE session_id IN (SELECT id FROM main.sessions)`); err != nil {
		return fmt.Errorf("copying extract progress: %w", err)
	}
	return nil
}
