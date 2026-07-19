package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	corerecall "go.kenn.io/agentsview/internal/recall"
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
	// Switch which generation's machine entries are served, in the same
	// transaction as the state flip: staged (archived) entries of the newly
	// active generation are promoted, and other generations' still-automatic
	// entries are archived. Human-touched entries are never moved.
	if _, err := tx.ExecContext(ctx, `
		UPDATE recall_entries
		SET status = 'archived',
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE review_state = 'unreviewed_auto' AND status = 'accepted'
		  AND source_run_id != ?
		  AND source_run_id IN
		      (SELECT fingerprint FROM recall_extract_generations)`,
		fingerprint,
	); err != nil {
		return fmt.Errorf("archiving retired generation entries: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE recall_entries
		SET status = 'accepted',
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE review_state = 'unreviewed_auto' AND status = 'archived'
		  AND source_run_id = ?`,
		fingerprint,
	); err != nil {
		return fmt.Errorf("promoting activated generation entries: %w", err)
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
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning generation retirement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// One conditional statement so the active-state check and the state
	// change cannot interleave with a concurrent activation.
	result, err := tx.ExecContext(ctx, `
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
	if affected == 0 {
		if _, err := db.extractGenerationByFingerprint(ctx, fingerprint); err != nil {
			return err
		}
		return fmt.Errorf(
			"generation %s is active; retiring it leaves no distilled corpus "+
				"to serve (use force to retire anyway)", fingerprint,
		)
	}
	// A retired generation stops serving: its still-automatic entries are
	// archived in the same transaction. Human-touched entries are kept.
	if _, err := tx.ExecContext(ctx, `
		UPDATE recall_entries
		SET status = 'archived',
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE review_state = 'unreviewed_auto' AND status = 'accepted'
		  AND source_run_id = ?`,
		fingerprint,
	); err != nil {
		return fmt.Errorf("archiving retired generation entries: %w", err)
	}
	return tx.Commit()
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

// ExtractProgressUpsert describes one progress upsert. StampedAt is the
// caller's transcript-read cutoff: the time captured *before* it began
// reading the messages the digest describes, so a write landing during
// derivation still compares as after the stamp and re-opens the session.
type ExtractProgressUpsert struct {
	SessionID     string
	Fingerprint   string
	ContentDigest string
	UnitsTotal    int
	StampedAt     time.Time
}

// UpsertExtractProgress ensures a progress row exists for the session under
// the generation. A matching content digest keeps existing progress; a
// changed digest resets the row to pending at cursor zero so the grown
// session is re-segmented and topped up. A session with zero units has
// nothing to extract, so its row lands directly in done — no worker will
// ever advance a cursor over an empty unit list.
func (db *DB) UpsertExtractProgress(
	ctx context.Context, u ExtractProgressUpsert,
) (ExtractProgress, error) {
	sessionID := u.SessionID
	fingerprint := u.Fingerprint
	contentDigest := u.ContentDigest
	unitsTotal := u.UnitsTotal
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
	if u.StampedAt.IsZero() {
		return zero, fmt.Errorf(
			"extract progress for session %s requires the transcript-read "+
				"cutoff: without it the stamp would silently claim coverage "+
				"through the row's write time", sessionID,
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
	// content_stamped_at always takes the caller's pre-read cutoff — on
	// insert, on digest reset, and on a same-digest revisit alike. A
	// revisit re-verified the transcript as of its own read, so keeping an
	// older stamp would leave later metadata writes re-opening the session
	// on every full pass; taking the row's write time instead would claim
	// coverage of writes that landed after the caller read the transcript.
	// Same-digest conflict rules: a zero-unit row is done by construction
	// whatever state it held — the extraction loop runs zero iterations for
	// it, so no cursor advance would ever promote it — and a failed row
	// keeps its state, error, and updated_at, because the retry's opening
	// upsert must not reset the failure backoff clock a cancelled retry
	// would then have to wait out again.
	_, err := db.getWriter().ExecContext(ctx, `
		INSERT INTO recall_extract_progress
			(session_id, generation_fingerprint, unit_cursor, units_total,
			 state, content_digest, content_stamped_at)
		VALUES (?, ?, 0, ?, ?, ?, ?)
		ON CONFLICT(session_id, generation_fingerprint) DO UPDATE SET
			unit_cursor = CASE
				WHEN recall_extract_progress.content_digest = excluded.content_digest
				THEN recall_extract_progress.unit_cursor ELSE 0 END,
			units_total = CASE
				WHEN recall_extract_progress.content_digest = excluded.content_digest
				THEN recall_extract_progress.units_total ELSE excluded.units_total END,
			state = CASE
				WHEN recall_extract_progress.content_digest != excluded.content_digest
				THEN ?
				WHEN excluded.units_total = 0 THEN ?
				ELSE recall_extract_progress.state END,
			content_digest = excluded.content_digest,
			last_error = CASE
				WHEN recall_extract_progress.content_digest = excluded.content_digest
					AND excluded.units_total != 0
				THEN recall_extract_progress.last_error ELSE '' END,
			content_stamped_at = excluded.content_stamped_at,
			updated_at = CASE
				WHEN recall_extract_progress.content_digest = excluded.content_digest
					AND recall_extract_progress.state = ?
					AND excluded.units_total != 0
				THEN recall_extract_progress.updated_at
				ELSE strftime('%Y-%m-%dT%H:%M:%fZ','now') END`,
		sessionID, fingerprint, unitsTotal, initialState,
		contentDigest, u.StampedAt.UTC().Format(extractTimeLayout),
		initialState, ExtractProgressDone, ExtractProgressFailed,
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
// strictly advances within bounds, so a worker that raced a digest reset
// gets ErrStaleExtractProgress instead of overwriting fresh state. A
// replay of an already-recorded cursor is an accepted no-op: it completed
// nothing new, so it must not touch the row's state or error — in
// particular it cannot resurrect a failed row.
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
		  AND unit_cursor < ?
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
	// Digest mismatch outranks the bounds check: after a reset to fewer
	// units a stale worker's cursor may exceed the new total, and it must
	// still get the typed stale error that tells it to re-read the row and
	// re-derive units.
	if progress.ContentDigest != expectedDigest {
		return fmt.Errorf(
			"advancing session %s past digest change: %w",
			sessionID, ErrStaleExtractProgress,
		)
	}
	if cursor > progress.UnitsTotal {
		return fmt.Errorf(
			"cursor %d exceeds %d units for session %s",
			cursor, progress.UnitsTotal, sessionID,
		)
	}
	if cursor == progress.UnitCursor {
		return nil
	}
	return fmt.Errorf(
		"advancing session %s past cursor regression: %w",
		sessionID, ErrStaleExtractProgress,
	)
}

// ExtractFailure identifies the exact progress state a worker observed
// when it failed, so a stale worker cannot demote fresher state.
type ExtractFailure struct {
	SessionID      string
	Fingerprint    string
	ExpectedDigest string
	ExpectedCursor int
	LastError      string
	// Reopen restarts the row's coverage from scratch: the mark may demote
	// a completed row (normally done rows refuse failure marks — a late
	// worker must not clobber finished work) and the cursor resets to
	// zero. Callers set it when the stored coverage claim is invalid: the
	// session's row and transcript disagree, or eligibility was lost
	// mid-extraction and the generated entries were discarded. The cursor
	// reset is what lets the retry converge — the strictly monotonic
	// cursor could never reach done again from a reopened completed row,
	// and a preserved cursor would skip units whose entries were deleted.
	Reopen bool
}

// MarkExtractProgressFailed records a failure without losing the resume
// point: the cursor stays where it was so a retry continues, not restarts.
// The update applies only when the stored digest, cursor, and non-done
// state all match what the failing worker observed; anything else means
// another worker moved the row on, and the failure is reported as
// ErrStaleExtractProgress instead of demoting newer progress. Reopen waives
// only the non-done condition — the digest and cursor guards still apply —
// and resets the row's cursor to zero.
func (db *DB) MarkExtractProgressFailed(
	ctx context.Context, failure ExtractFailure,
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
		SET last_error = ?,
			unit_cursor = CASE WHEN ? THEN 0 ELSE unit_cursor END,
			state = ?,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE session_id = ? AND generation_fingerprint = ?
		  AND content_digest = ?
		  AND unit_cursor = ?
		  AND (? OR state != ?)`,
		failure.LastError, failure.Reopen, ExtractProgressFailed,
		failure.SessionID, failure.Fingerprint,
		failure.ExpectedDigest, failure.ExpectedCursor,
		failure.Reopen, ExtractProgressDone,
	)
	if err != nil {
		return fmt.Errorf(
			"marking extract progress failed for session %s: %w",
			failure.SessionID, err,
		)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(
			"marking extract progress failed for session %s: %w",
			failure.SessionID, err,
		)
	}
	if affected > 0 {
		return nil
	}
	progress, ok, err := db.ExtractProgress(
		ctx, failure.SessionID, failure.Fingerprint,
	)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf(
			"no extract progress row for session %s under generation %s",
			failure.SessionID, failure.Fingerprint,
		)
	}
	if progress.State == ExtractProgressDone {
		return fmt.Errorf(
			"session %s is already done under generation %s: %w",
			failure.SessionID, failure.Fingerprint, ErrStaleExtractProgress,
		)
	}
	if progress.ContentDigest != failure.ExpectedDigest {
		return fmt.Errorf(
			"failing session %s past digest change: %w",
			failure.SessionID, ErrStaleExtractProgress,
		)
	}
	return fmt.Errorf(
		"failing session %s behind cursor %d: %w",
		failure.SessionID, progress.UnitCursor, ErrStaleExtractProgress,
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

// attachedColumnExistsTx reports whether the attached old_db's table carries
// the named column, so copies can adapt to archives written before a column
// was introduced.
func attachedColumnExistsTx(
	ctx context.Context, tx *sql.Tx, table, column string,
) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pragma_table_info(?, 'old_db') WHERE name = ?
		)`, table, column).Scan(&exists); err != nil {
		return false, fmt.Errorf(
			"checking source column %s.%s: %w", table, column, err,
		)
	}
	return exists, nil
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
	// content_stamped_at must survive the copy: an empty stamp reads as
	// "changed since coverage" for every completed session, so losing it
	// would reload the whole archive's transcripts on the next full pass.
	// Archives written before the column existed copy it as '' — those
	// rows re-open once and settle on their first revisit.
	stampExists, err := attachedColumnExistsTx(
		ctx, tx, "recall_extract_progress", "content_stamped_at",
	)
	if err != nil {
		return err
	}
	stampSource := "''"
	if stampExists {
		stampSource = "content_stamped_at"
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO recall_extract_progress (
			session_id, generation_fingerprint, unit_cursor, units_total,
			state, content_digest, content_stamped_at, last_error, updated_at
		)
		SELECT session_id, generation_fingerprint, unit_cursor, units_total,
		       state, content_digest, `+stampSource+`, last_error, updated_at
		FROM old_db.recall_extract_progress
		WHERE session_id IN (SELECT id FROM main.sessions)`); err != nil {
		return fmt.Errorf("copying extract progress: %w", err)
	}
	return nil
}

// extractTimeLayout matches the strftime('%Y-%m-%dT%H:%M:%fZ') format the
// schema stamps into session and progress timestamps, so cutoffs formatted
// with it compare correctly as strings.
const extractTimeLayout = "2006-01-02T15:04:05.000Z"

// ExtractCandidateQuery selects sessions eligible for extraction under one
// generation. QuietCutoff excludes sessions that ended after it, so recently
// finished sessions settle before being read. FailedRetryCutoff gates failed
// rows: only failures last touched at or before it are retried, and the zero
// value retries nothing — a caller that forgets to set it can never cause a
// retry storm. ScanVersions are the secret-scan rules versions considered
// current; sessions whose last scan is missing or stale are excluded, and an
// empty list is an error rather than "trust everything". IncludeDone
// revisits completed sessions so their content digests can be rechecked, but
// only those written to since extraction finished.
type ExtractCandidateQuery struct {
	Fingerprint       string
	QuietCutoff       time.Time
	FailedRetryCutoff time.Time
	ScanVersions      []string
	IncludeDone       bool
	// ChangedSince restricts *discovery* — sessions with no progress row
	// under this generation yet — to those written at or after it, so a
	// steady-state scan pass touches only recent writes instead of the whole
	// archive. Sessions already in progress (pending, partial, retryable
	// failed, revisitable done) are always offered regardless. The zero
	// value leaves discovery unrestricted.
	ChangedSince time.Time
	// DoneChangedSince restricts the *done-revisit* arm the same way: only
	// completed sessions written at or after it are rechecked, so a
	// steady-state full pass walks recent writes via the sessions index
	// instead of every completed progress row. The zero value leaves the
	// revisit unrestricted; ignored unless IncludeDone is set.
	DoneChangedSince time.Time
	Limit            int
}

// extractEligibleSessionSQL is the extraction privacy boundary over one
// sessions row aliased s. Every arm of the candidates query applies it, so
// the discovery and progress paths can never disagree about eligibility. It
// consumes len(ScanVersions)+1 args: the versions, then the quiet cutoff.
const extractEligibleSessionSQL = `s.deleted_at IS NULL
	AND s.is_automated = 0
	AND s.secret_leak_count = 0
	AND s.secrets_rules_version IN (%s)
	AND NOT EXISTS (
		SELECT 1 FROM secret_findings sf WHERE sf.session_id = s.id
	)
	AND s.message_count > 0
	AND s.ended_at IS NOT NULL
	AND s.ended_at != ''
	AND s.ended_at <= ?`

// extractCandidateSQL builds the candidates query as a union of indexed
// arms: discovery walks sessions by local_modified_at (bounded by
// ChangedSince when set) for sessions with no progress row; the queue arm
// walks this generation's pending, partial, and failed progress rows by
// state; and the done-revisit arm (IncludeDone) walks sessions by
// local_modified_at again (bounded by DoneChangedSince when set) joining
// their done progress rows. Keeping the arms separate lets each use its own
// index instead of forcing a full scan of the sessions or progress tables
// on every pass.
func extractCandidateSQL(q ExtractCandidateQuery) (string, []any, error) {
	if strings.TrimSpace(q.Fingerprint) == "" {
		return "", nil, fmt.Errorf(
			"extract candidate query requires a fingerprint")
	}
	if len(q.ScanVersions) == 0 {
		return "", nil, fmt.Errorf(
			"extract candidate query requires the current secret-scan " +
				"versions: without them unscanned sessions would count as clean")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = -1
	}
	versionMarks := strings.Repeat("?,", len(q.ScanVersions))
	versionMarks = versionMarks[:len(versionMarks)-1]
	eligible := fmt.Sprintf(extractEligibleSessionSQL, versionMarks)
	eligibleArgs := make([]any, 0, len(q.ScanVersions)+1)
	for _, version := range q.ScanVersions {
		eligibleArgs = append(eligibleArgs, version)
	}
	eligibleArgs = append(eligibleArgs,
		q.QuietCutoff.UTC().Format(extractTimeLayout))

	var sb strings.Builder
	var args []any
	sb.WriteString(`
		SELECT id FROM (
		SELECT s.id AS id, s.ended_at AS ended_at
		FROM sessions s
		WHERE ` + eligible + `
			AND NOT EXISTS (
				SELECT 1 FROM recall_extract_progress p
				WHERE p.session_id = s.id AND p.generation_fingerprint = ?
			)`)
	args = append(args, eligibleArgs...)
	args = append(args, q.Fingerprint)
	if !q.ChangedSince.IsZero() {
		// A NULL local_modified_at (archives predating the column) must
		// stay discoverable; both branches of the OR are ranges over the
		// same index.
		sb.WriteString(`
			AND (s.local_modified_at IS NULL OR s.local_modified_at >= ?)`)
		args = append(args, q.ChangedSince.UTC().Format(extractTimeLayout))
	}
	sb.WriteString(`
		UNION
		SELECT s.id AS id, s.ended_at AS ended_at
		FROM recall_extract_progress p
		JOIN sessions s ON s.id = p.session_id
		WHERE p.generation_fingerprint = ?
			AND (
				p.state IN (?, ?)
				OR (p.state = ? AND p.updated_at <= ?)
			)
			AND ` + eligible)
	args = append(args,
		q.Fingerprint,
		ExtractProgressPending, ExtractProgressPartial,
		ExtractProgressFailed,
		q.FailedRetryCutoff.UTC().Format(extractTimeLayout),
	)
	args = append(args, eligibleArgs...)
	if q.IncludeDone {
		// Done revisits drive from the sessions side so the planner can
		// bound them with idx_sessions_local_modified; walking done progress
		// rows instead would scan every completed session each pass. A NULL
		// local_modified_at (legacy row, no write recorded since the column
		// existed — every write path records one) revisits only while its
		// stamp is empty: pre-stamp archives re-open once and settle, but a
		// stamped legacy row cannot have changed and must not reload on
		// every full pass.
		sb.WriteString(`
		UNION
		SELECT s.id AS id, s.ended_at AS ended_at
		FROM sessions s
		JOIN recall_extract_progress p ON p.session_id = s.id
			AND p.generation_fingerprint = ?
		WHERE p.state = ?
			AND ((s.local_modified_at IS NULL AND p.content_stamped_at = '')
				OR s.local_modified_at >= p.content_stamped_at)
			AND ` + eligible)
		args = append(args, q.Fingerprint, ExtractProgressDone)
		args = append(args, eligibleArgs...)
		if !q.DoneChangedSince.IsZero() {
			// NULL local_modified_at rows (archives predating the column)
			// must stay revisitable regardless of the bound.
			sb.WriteString(`
			AND (s.local_modified_at IS NULL OR s.local_modified_at >= ?)`)
			args = append(args,
				q.DoneChangedSince.UTC().Format(extractTimeLayout))
		}
	}
	sb.WriteString(`
		)
		ORDER BY ended_at ASC, id ASC
		LIMIT ?`)
	args = append(args, limit)
	return sb.String(), args, nil
}

// ExtractCandidates returns eligible session ids, oldest ended first.
// Eligibility encodes the extraction privacy boundary and is deliberately
// not configurable: automated sessions, trashed sessions, and sessions with
// any secret findings never reach the extraction model.
func (db *DB) ExtractCandidates(
	ctx context.Context, q ExtractCandidateQuery,
) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	query, args, err := extractCandidateSQL(q)
	if err != nil {
		return nil, err
	}
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying extract candidates: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning extract candidate: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading extract candidates: %w", err)
	}
	return ids, nil
}

// InsertExtractedRecallEntries inserts entries whose deterministic ids are
// not yet present and returns how many were new. The batch is atomic: any
// invalid entry rolls back the whole call so a replay can start clean.
// Already-present ids are skipped without touching their evidence, which
// makes replaying a unit after a crash or digest reset idempotent.
func (db *DB) InsertExtractedRecallEntries(
	ctx context.Context, entries []RecallEntry,
) (int, error) {
	if err := db.requireWritable(); err != nil {
		return 0, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := db.getWriter().Begin()
	if err != nil {
		return 0, fmt.Errorf("begin extracted entries insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	inserted := 0
	for _, entry := range entries {
		if entry.ID == "" {
			return 0, fmt.Errorf("extracted recall entry id is required")
		}
		var exists int
		err := tx.QueryRowContext(ctx,
			"SELECT 1 FROM recall_entries WHERE id = ?", entry.ID,
		).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf(
				"checking extracted entry %s: %w", entry.ID, err,
			)
		}
		if err := insertRecallEntryTx(tx, entry); err != nil {
			return 0, err
		}
		inserted++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit extracted entries insert: %w", err)
	}
	return inserted, nil
}

// DeleteExtractedRecallEntries removes one session's machine-generated
// entries under one generation, so a changed transcript can be re-extracted
// without leaving stale entries behind or colliding with their ids. Entries
// whose review state has been touched by a human are preserved. Evidence
// rows cascade with their entries. Returns how many entries were deleted.
func (db *DB) DeleteExtractedRecallEntries(
	ctx context.Context, fingerprint, sessionID string,
) (int, error) {
	if err := db.requireWritable(); err != nil {
		return 0, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := db.getWriter().ExecContext(ctx, `
		DELETE FROM recall_entries
		WHERE source_run_id = ? AND source_session_id = ?
			AND review_state = ?`,
		fingerprint, sessionID, corerecall.ReviewStateUnreviewedAuto,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"deleting extracted entries for %s/%s: %w",
			fingerprint, sessionID, err,
		)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting deleted extracted entries: %w", err)
	}
	return int(deleted), nil
}

// SyncExtractedEntryContext refreshes the session-derived context fields
// (project, cwd, git branch, agent) on one session's generated entries under
// one generation. Entries copy those fields from the session row at insert
// time, and a metadata-only session update keeps the unit digest unchanged —
// without this sync a same-digest revisit would settle the coverage stamp
// while the entries kept matching Recall filters for the old context.
// Human-touched entries are left as they were, mirroring the delete path.
// Returns how many entries changed.
func (db *DB) SyncExtractedEntryContext(
	ctx context.Context, fingerprint string, session *Session,
) (int, error) {
	if err := db.requireWritable(); err != nil {
		return 0, err
	}
	if session == nil {
		return 0, fmt.Errorf(
			"syncing extracted entry context for generation %s requires a "+
				"session", fingerprint,
		)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := db.getWriter().ExecContext(ctx, `
		UPDATE recall_entries
		SET project = ?, cwd = ?, git_branch = ?, agent = ?,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE source_run_id = ? AND source_session_id = ?
			AND review_state = ?
			AND (project != ? OR cwd != ? OR git_branch != ? OR agent != ?)`,
		session.Project, session.Cwd, session.GitBranch, session.Agent,
		fingerprint, session.ID, corerecall.ReviewStateUnreviewedAuto,
		session.Project, session.Cwd, session.GitBranch, session.Agent,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"syncing extracted entry context for %s/%s: %w",
			fingerprint, session.ID, err,
		)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting synced extracted entries: %w", err)
	}
	return int(updated), nil
}

// ExtractProgressStats aggregates one generation's progress rows and its
// corpus size for status reporting.
type ExtractProgressStats struct {
	Pending    int `json:"pending"`
	Partial    int `json:"partial"`
	Done       int `json:"done"`
	Failed     int `json:"failed"`
	UnitsDone  int `json:"units_done"`
	UnitsTotal int `json:"units_total"`
	Entries    int `json:"entries"`
}

// ExtractProgressStats returns per-state session counts, unit totals, and
// the number of entries the generation has produced.
func (db *DB) ExtractProgressStats(
	ctx context.Context, fingerprint string,
) (ExtractProgressStats, error) {
	var stats ExtractProgressStats
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT state, COUNT(*),
			COALESCE(SUM(unit_cursor), 0), COALESCE(SUM(units_total), 0)
		FROM recall_extract_progress
		WHERE generation_fingerprint = ?
		GROUP BY state`, fingerprint)
	if err != nil {
		return stats, fmt.Errorf("querying extract progress stats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count, unitsDone, unitsTotal int
		if err := rows.Scan(&state, &count, &unitsDone, &unitsTotal); err != nil {
			return stats, fmt.Errorf("scanning extract progress stats: %w", err)
		}
		switch state {
		case ExtractProgressPending:
			stats.Pending = count
		case ExtractProgressPartial:
			stats.Partial = count
		case ExtractProgressDone:
			stats.Done = count
		case ExtractProgressFailed:
			stats.Failed = count
		}
		stats.UnitsDone += unitsDone
		stats.UnitsTotal += unitsTotal
	}
	if err := rows.Err(); err != nil {
		return stats, fmt.Errorf("reading extract progress stats: %w", err)
	}
	err = db.getReader().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM recall_entries WHERE source_run_id = ?",
		fingerprint,
	).Scan(&stats.Entries)
	if err != nil {
		return stats, fmt.Errorf("counting extracted entries: %w", err)
	}
	return stats, nil
}
