package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
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

// ErrExtractSessionDrifted reports that a guarded commit found the session
// changed or no longer eligible: the transcript state the caller derived its
// unit from is not the state on disk, or the session was trashed, flagged
// automated, or gained secret findings. Nothing was persisted.
var ErrExtractSessionDrifted = errors.New(
	"extract session drifted during commit")

func extractDriftErrorf(format string, args ...any) error {
	return fmt.Errorf(
		format+": %w", append(args, ErrExtractSessionDrifted)...)
}

// ErrExtractActivationBlocked reports that an activation's in-transaction
// guards refused to switch generations: coverage regressed, staged coverage
// went stale, or promotion would leave nothing servable. Nothing was
// changed; the caller retries after the next build pass restores coverage.
var ErrExtractActivationBlocked = errors.New(
	"extract generation activation blocked")

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
// two generations can never be active simultaneously. The caller's coverage
// checks are advisory: sessions can slip back to pending, gain writes past
// their coverage stamp, lose their scan stamp, or become eligible without
// ever being extracted between those checks and this write, so the same
// gates are re-verified inside the transaction — against scanVersions as
// the set of current secret-scan rules and quietCutoff as the eligibility
// cutoff — and any mismatch aborts with ErrExtractActivationBlocked instead
// of retiring the served corpus around it.
func (db *DB) ActivateExtractGeneration(
	ctx context.Context, fingerprint string,
	scanVersions []string, quietCutoff time.Time,
) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	if len(scanVersions) == 0 {
		return fmt.Errorf(
			"activating generation %s requires the current secret-scan "+
				"versions: without them stale coverage would count as "+
				"current", fingerprint)
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

	if err := verifyExtractActivationCoverageTx(
		ctx, tx, fingerprint, scanVersions, quietCutoff,
	); err != nil {
		return err
	}
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
	// Promotion re-verifies full eligibility inside the activation
	// transaction, and clears — not merely skips — what fails it. Any
	// session no longer fully eligible (trashed, flagged automated,
	// carrying findings, reopened, back inside the quiet period, awaiting
	// rescan, or gone entirely) has its staged output and progress rows
	// deleted: an archived entry under a surviving progress row would
	// never be promoted or rediscovered once the generation is active —
	// deferring hard-ineligible rows to the retraction pass loses the
	// race against a restore that arrives first — while a cleared session
	// is rediscovered and re-extracted from scratch when it settles or
	// returns.
	versionMarks := strings.TrimSuffix(
		strings.Repeat("?,", len(scanVersions)), ",")
	// Concatenation, not a second Sprintf pass: the rendered eligibility
	// SQL contains literal % characters (strftime), which a re-format
	// would mangle into %!Y(MISSING)-style garbage.
	staleSessionFor := func(idColumn string) string {
		return `NOT EXISTS (SELECT 1 FROM sessions s
			WHERE s.id = ` + idColumn + `
			  AND ` +
			fmt.Sprintf(extractEligibleSessionSQL, versionMarks) + `)`
	}
	staleArgs := make([]any, 0, len(scanVersions)+2)
	staleArgs = append(staleArgs, fingerprint)
	for _, version := range scanVersions {
		staleArgs = append(staleArgs, version)
	}
	staleArgs = append(staleArgs, quietCutoff.UTC().Format(extractTimeLayout))
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM recall_entries
		WHERE review_state = 'unreviewed_auto' AND status = 'archived'
		  AND source_run_id = ?
		  AND `+
		staleSessionFor("recall_entries.source_session_id"),
		staleArgs...,
	); err != nil {
		return fmt.Errorf(
			"clearing staged entries of ineligible sessions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM recall_extract_progress
		WHERE generation_fingerprint = ?
		  AND `+
		staleSessionFor("recall_extract_progress.session_id"),
		staleArgs...,
	); err != nil {
		return fmt.Errorf(
			"clearing progress of ineligible sessions: %w", err)
	}
	// Everything still staged has a fully eligible session: the deletes
	// above removed the rest in this same transaction.
	// Superseded entries stay archived: a reviewed replacement archived the
	// obsolete entry with a superseded_by link, and promoting it back into
	// service would serve both the obsolete entry and its replacement.
	if _, err := tx.ExecContext(ctx, `
		UPDATE recall_entries
		SET status = 'accepted',
		    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE review_state = 'unreviewed_auto' AND status = 'archived'
		  AND superseded_by_entry_id = ''
		  AND source_run_id = ?`,
		fingerprint,
	); err != nil {
		return fmt.Errorf("promoting activated generation entries: %w", err)
	}
	// The switch must leave something servable: every staged entry may
	// have been excluded (sessions turned ineligible) or retracted since
	// the caller's checks, and committing then would retire the served
	// corpus with no replacement. Serving additionally requires verified
	// provenance, so revoked entries do not count.
	var servable int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM recall_entries
		WHERE review_state = 'unreviewed_auto' AND status = 'accepted'
		  AND superseded_by_entry_id = ''
		  AND provenance_ok != 0 AND source_run_id = ?`,
		fingerprint,
	).Scan(&servable); err != nil {
		return fmt.Errorf("counting servable entries: %w", err)
	}
	if servable == 0 {
		return fmt.Errorf(
			"generation %s has no servable entries to promote: %w",
			fingerprint, ErrExtractActivationBlocked)
	}
	return tx.Commit()
}

// verifyExtractActivationCoverageTx re-verifies, inside the activation
// transaction, that the generation's coverage still supports serving: no
// fully eligible session is pending or partial, no completed session —
// still extraction-eligible — has transcript writes past its coverage
// stamp or a scan stamp outside the current rules versions, and no eligible session
// lacks a progress row entirely (a single-session run, or a session ending
// after the caller's checks, leaves uncovered work that no progress-based
// gate can see). Failed sessions do not block (they retry and top the
// corpus up later) unless they hold staged output made stale by a later
// session write, and sessions that turned ineligible are ignored by
// every gate — including the unfinished count, since their extraction can
// never finish and an explicit activation runs no retraction pass to clear
// their rows first: promotion excludes their entries and the retraction
// pass removes them.
func verifyExtractActivationCoverageTx(
	ctx context.Context, tx *sql.Tx, fingerprint string,
	scanVersions []string, quietCutoff time.Time,
) error {
	versionMarks := strings.TrimSuffix(
		strings.Repeat("?,", len(scanVersions)), ",")
	// Full eligibility, not merely "not hard-ineligible": a pending or
	// partial row whose session is in transient flux (reopened, scan
	// stamp lost) is skipped by candidate selection and left alone by
	// reconciliation, so no pass can ever finish it — counting it here
	// would block activation until the session happens to settle,
	// possibly forever. The cleanup below deletes such rows with their
	// staged output, and rediscovery re-extracts once the session
	// settles.
	buildingArgs := make([]any, 0, len(scanVersions)+4)
	buildingArgs = append(buildingArgs,
		fingerprint, ExtractProgressPending, ExtractProgressPartial)
	for _, version := range scanVersions {
		buildingArgs = append(buildingArgs, version)
	}
	buildingArgs = append(buildingArgs,
		quietCutoff.UTC().Format(extractTimeLayout))
	var building int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM recall_extract_progress p
		JOIN sessions s ON s.id = p.session_id
		WHERE p.generation_fingerprint = ? AND p.state IN (?, ?)
		  AND `+fmt.Sprintf(extractEligibleSessionSQL, versionMarks),
		buildingArgs...,
	).Scan(&building); err != nil {
		return fmt.Errorf("counting unfinished coverage: %w", err)
	}
	if building > 0 {
		return fmt.Errorf(
			"generation %s has %d sessions still being extracted: %w",
			fingerprint, building, ErrExtractActivationBlocked)
	}
	args := make([]any, 0, len(scanVersions)+2)
	args = append(args, fingerprint, ExtractProgressDone)
	for _, version := range scanVersions {
		args = append(args, version)
	}
	// The stamp comparison mirrors the done-revisit gate (>= keeps the
	// same-millisecond write on the safe side; the NULL pair settles
	// legacy rows once), and the scan-stamp arm catches the case the
	// backlog probe cannot see: a transcript write clears the stamp, which
	// removes the session from the eligible candidate set entirely while
	// its staged entries would still promote.
	var stale int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM recall_extract_progress p
		JOIN sessions s ON s.id = p.session_id
		WHERE p.generation_fingerprint = ? AND p.state = ?
		  AND NOT (`+extractSessionIneligibleSQL+`)
		  AND (
			((s.local_modified_at IS NULL AND p.content_stamped_at = '')
				OR s.local_modified_at >= p.content_stamped_at)
			OR s.secrets_rules_version IS NULL
			OR s.secrets_rules_version NOT IN (`+versionMarks+`)
		  )`,
		args...,
	).Scan(&stale); err != nil {
		return fmt.Errorf("counting stale coverage: %w", err)
	}
	if stale > 0 {
		return fmt.Errorf(
			"generation %s has %d completed sessions whose coverage went "+
				"stale: %w", fingerprint, stale, ErrExtractActivationBlocked)
	}
	failedArgs := make([]any, 0, len(scanVersions)+3)
	failedArgs = append(failedArgs, fingerprint, ExtractProgressFailed)
	for _, version := range scanVersions {
		failedArgs = append(failedArgs, version)
	}
	failedArgs = append(failedArgs, quietCutoff.UTC().Format(extractTimeLayout))
	// Failed rows do not block in general — they retry and top the corpus
	// up later — but a failed partial row keeps its staged entries behind
	// the failure backoff, and a session write past its stamp (content, or
	// a remap to another project, cwd, or branch) leaves them stale in
	// ways only the retry's refresh repairs. Only fully eligible sessions
	// count: staged output of every other session is deleted by the
	// cleanup later in this transaction, and a failed row with nothing
	// staged promotes nothing.
	var staleFailed int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM recall_extract_progress p
		JOIN sessions s ON s.id = p.session_id
		WHERE p.generation_fingerprint = ? AND p.state = ?
		  AND `+fmt.Sprintf(extractEligibleSessionSQL, versionMarks)+`
		  AND ((s.local_modified_at IS NULL AND p.content_stamped_at = '')
			OR s.local_modified_at >= p.content_stamped_at)
		  AND EXISTS (
			SELECT 1 FROM recall_entries e
			WHERE e.source_session_id = p.session_id
			  AND e.source_run_id = p.generation_fingerprint
			  AND e.status = 'archived'
		  )`,
		failedArgs...,
	).Scan(&staleFailed); err != nil {
		return fmt.Errorf("counting stale failed coverage: %w", err)
	}
	if staleFailed > 0 {
		return fmt.Errorf(
			"generation %s has %d failed sessions whose staged output "+
				"went stale: %w",
			fingerprint, staleFailed, ErrExtractActivationBlocked)
	}
	eligibleArgs := make([]any, 0, len(scanVersions)+2)
	for _, version := range scanVersions {
		eligibleArgs = append(eligibleArgs, version)
	}
	eligibleArgs = append(eligibleArgs,
		quietCutoff.UTC().Format(extractTimeLayout), fingerprint)
	var uncovered bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM sessions s
			WHERE `+fmt.Sprintf(extractEligibleSessionSQL, versionMarks)+`
			  AND NOT EXISTS (
				SELECT 1 FROM recall_extract_progress p
				WHERE p.session_id = s.id
				  AND p.generation_fingerprint = ?
			  )
		)`,
		eligibleArgs...,
	).Scan(&uncovered); err != nil {
		return fmt.Errorf("probing uncovered eligible sessions: %w", err)
	}
	if uncovered {
		return fmt.Errorf(
			"generation %s has eligible sessions that were never "+
				"extracted: %w", fingerprint, ErrExtractActivationBlocked)
	}
	return nil
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
	tx, err := db.getWriter().Begin()
	if err != nil {
		return zero, fmt.Errorf(
			"begin extract progress upsert for session %s: %w",
			sessionID, err)
	}
	defer func() { _ = tx.Rollback() }()
	// A digest change means the previous derivation's entries are stale:
	// they are deleted in the same transaction that resets the row, so no
	// failure between the two can leave a done row claiming coverage for
	// entries that no longer exist. Human-touched entries are never
	// machine-deleted.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM recall_entries
		WHERE review_state = 'unreviewed_auto'
		  AND source_run_id = ?
		  AND source_session_id = ?
		  AND EXISTS (
			SELECT 1 FROM recall_extract_progress p
			WHERE p.session_id = ? AND p.generation_fingerprint = ?
			  AND p.content_digest != ?
		  )`,
		fingerprint, sessionID, sessionID, fingerprint, contentDigest,
	); err != nil {
		return zero, fmt.Errorf(
			"removing stale entries for session %s: %w", sessionID, err)
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
	_, err = tx.ExecContext(ctx, `
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
	if err := tx.Commit(); err != nil {
		return zero, fmt.Errorf(
			"commit extract progress upsert for session %s: %w",
			sessionID, err)
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
	AND strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ', s.ended_at) <= ?`

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
	// The failed-retry arm is a separate UNION member, not an OR term:
	// inside an OR the planner falls back to the bare fingerprint prefix
	// of idx_recall_extract_progress_retry, fetching every failed row —
	// backoff included — on each pass. As its own arm it gets the tight
	// (fingerprint, state, updated_at <= cutoff) range.
	sb.WriteString(`
		UNION
		SELECT s.id AS id, s.ended_at AS ended_at
		FROM recall_extract_progress p
		JOIN sessions s ON s.id = p.session_id
		WHERE p.generation_fingerprint = ?
			AND p.state IN (?, ?)
			AND ` + eligible + `
		UNION
		SELECT s.id AS id, s.ended_at AS ended_at
		FROM recall_extract_progress p
		JOIN sessions s ON s.id = p.session_id
		WHERE p.generation_fingerprint = ?
			AND p.state = ?
			AND p.updated_at <= ?
			AND ` + eligible)
	args = append(args,
		q.Fingerprint,
		ExtractProgressPending, ExtractProgressPartial,
	)
	args = append(args, eligibleArgs...)
	args = append(args,
		q.Fingerprint,
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
	inserted, err := insertExtractedRecallEntriesTx(ctx, tx, entries)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit extracted entries insert: %w", err)
	}
	return inserted, nil
}

func insertExtractedRecallEntriesTx(
	ctx context.Context, tx *sql.Tx, entries []RecallEntry,
) (int, error) {
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
	return inserted, nil
}

// ExtractUnitCommit is one distilled unit's guarded write: the entries to
// insert plus the transcript state (digest, cursor, snapshot fields) they
// were derived from. CommitExtractedUnit verifies the guard inside the same
// transaction that persists the entries, so output distilled from a stale or
// newly ineligible view can never land.
type ExtractUnitCommit struct {
	SessionID    string
	Fingerprint  string
	Digest       string
	Cursor       int
	ScanVersions []string
	// Snapshot guard: the session-row state the unit was derived from.
	MessageCount       int
	TranscriptRevision *string
	LocalModifiedAt    *string
	EndedAt            *string
	Entries            []RecallEntry
}

// CommitExtractedUnit atomically re-verifies the session (eligibility,
// absence of secret findings, and an unchanged snapshot), binds each
// entry's evidence to the host transcript (content digest and stable
// endpoint UUIDs — without them the evidence reconciler revokes provenance
// on the first transcript write), inserts the entries, and advances the
// progress cursor. A guard failure returns ErrExtractSessionDrifted with
// nothing persisted; a cursor conflict returns ErrStaleExtractProgress.
func (db *DB) CommitExtractedUnit(
	ctx context.Context, u ExtractUnitCommit,
) (int, error) {
	if err := db.requireWritable(); err != nil {
		return 0, err
	}
	if len(u.ScanVersions) == 0 {
		return 0, fmt.Errorf(
			"committing unit for session %s requires the current "+
				"secret-scan versions", u.SessionID)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := db.getWriter().Begin()
	if err != nil {
		return 0, fmt.Errorf("begin extracted unit commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := verifyExtractSessionGuardTx(ctx, tx, ExtractSessionGuard{
		SessionID:          u.SessionID,
		ScanVersions:       u.ScanVersions,
		MessageCount:       u.MessageCount,
		TranscriptRevision: u.TranscriptRevision,
		LocalModifiedAt:    u.LocalModifiedAt,
		EndedAt:            u.EndedAt,
	}); err != nil {
		return 0, err
	}
	entries, err := bindExtractedEvidenceTx(ctx, tx, u)
	if err != nil {
		return 0, err
	}
	inserted, err := insertExtractedRecallEntriesTx(ctx, tx, entries)
	if err != nil {
		return 0, err
	}
	// The stored cursor must be exactly the one this unit was derived
	// from: a same-digest reopen resets the row to zero after deleting its
	// entries, and a merely-monotonic guard would let a stale worker
	// fast-forward past units that no longer have output.
	next := u.Cursor + 1
	result, err := tx.ExecContext(ctx, `
		UPDATE recall_extract_progress
		SET unit_cursor = ?,
			state = CASE WHEN ? >= units_total THEN ? ELSE ? END,
			last_error = '',
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE session_id = ? AND generation_fingerprint = ?
		  AND content_digest = ?
		  AND unit_cursor = ?
		  AND ? <= units_total`,
		next, next, ExtractProgressDone, ExtractProgressPartial,
		u.SessionID, u.Fingerprint, u.Digest, u.Cursor, next,
	)
	if err != nil {
		return 0, fmt.Errorf(
			"advancing cursor for session %s: %w", u.SessionID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf(
			"advancing cursor for session %s: %w", u.SessionID, err)
	}
	if affected == 0 {
		return 0, fmt.Errorf(
			"advancing cursor for session %s to %d under digest %s: %w",
			u.SessionID, next, u.Digest, ErrStaleExtractProgress)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit extracted unit: %w", err)
	}
	return inserted, nil
}

// ExtractSessionGuard is the session-row snapshot a guarded write was
// derived from; verification refuses the write when the stored row has
// moved or the session is no longer eligible.
type ExtractSessionGuard struct {
	SessionID          string
	ScanVersions       []string
	MessageCount       int
	TranscriptRevision *string
	LocalModifiedAt    *string
	EndedAt            *string
}

func verifyExtractSessionGuardTx(
	ctx context.Context, tx *sql.Tx, u ExtractSessionGuard,
) error {
	var (
		deletedAt, revision, localModified, endedAt sql.NullString
		isAutomated                                 bool
		leakCount, messageCount                     int
		rulesVersion                                string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT deleted_at, is_automated, secret_leak_count,
		       secrets_rules_version, message_count,
		       transcript_revision, local_modified_at, ended_at
		FROM sessions WHERE id = ?`, u.SessionID,
	).Scan(&deletedAt, &isAutomated, &leakCount, &rulesVersion,
		&messageCount, &revision, &localModified, &endedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return extractDriftErrorf("session %s vanished", u.SessionID)
	}
	if err != nil {
		return fmt.Errorf(
			"reading session %s for unit commit: %w", u.SessionID, err)
	}
	versionOK := slices.Contains(u.ScanVersions, rulesVersion)
	switch {
	case deletedAt.Valid:
		return extractDriftErrorf("session %s was trashed", u.SessionID)
	case isAutomated:
		return extractDriftErrorf(
			"session %s was flagged automated", u.SessionID)
	case leakCount > 0:
		return extractDriftErrorf(
			"session %s gained %d secret leaks", u.SessionID, leakCount)
	case !versionOK:
		return extractDriftErrorf(
			"session %s lost its current secret scan", u.SessionID)
	case messageCount != u.MessageCount:
		return extractDriftErrorf(
			"session %s message count moved from %d to %d",
			u.SessionID, u.MessageCount, messageCount)
	case !nullableStringEqual(revision, u.TranscriptRevision):
		return extractDriftErrorf(
			"session %s transcript revision changed", u.SessionID)
	case !nullableStringEqual(localModified, u.LocalModifiedAt):
		return extractDriftErrorf(
			"session %s was written to during distillation", u.SessionID)
	case !nullableStringEqual(endedAt, u.EndedAt):
		// A bare session-row update can reopen or re-date a session
		// without moving any other guarded field; eligibility treats
		// ended_at as state, so the commit must too.
		return extractDriftErrorf(
			"session %s was reopened or re-dated during distillation",
			u.SessionID)
	}
	var findings int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM secret_findings WHERE session_id = ?",
		u.SessionID,
	).Scan(&findings); err != nil {
		return fmt.Errorf(
			"counting findings for session %s: %w", u.SessionID, err)
	}
	if findings > 0 {
		return extractDriftErrorf(
			"session %s gained %d secret findings", u.SessionID, findings)
	}
	return nil
}

func nullableStringEqual(stored sql.NullString, expected *string) bool {
	if !stored.Valid || expected == nil {
		return !stored.Valid && expected == nil
	}
	return stored.String == *expected
}

// bindExtractedEvidenceTx stamps host-derived provenance onto each entry's
// evidence rows: the content digest and stable endpoint UUIDs the evidence
// reconciler later re-verifies. Ranges are bound once and shared, since every
// entry of one unit cites the same transcript window.
func bindExtractedEvidenceTx(
	ctx context.Context, tx *sql.Tx, u ExtractUnitCommit,
) ([]RecallEntry, error) {
	type ordinalRange struct{ start, end int }
	bound := make(map[ordinalRange]RecallEvidenceSelectionMetadata)
	entries := make([]RecallEntry, len(u.Entries))
	for i, entry := range u.Entries {
		entry.Evidence = append([]RecallEvidence(nil), entry.Evidence...)
		for j, evidence := range entry.Evidence {
			key := ordinalRange{
				evidence.MessageStartOrdinal, evidence.MessageEndOrdinal,
			}
			metadata, ok := bound[key]
			if !ok {
				window, err := buildRecallEvidenceWindow(
					ctx, tx, u.SessionID, key.start, key.end)
				if err != nil {
					if isRecallEvidenceValidationError(err) {
						return nil, extractDriftErrorf(
							"evidence window %d-%d for session %s is "+
								"invalid (%v)", key.start, key.end,
							u.SessionID, err)
					}
					return nil, err
				}
				metadata, err = window.BindSelection(RecallEvidenceSelection{
					MessageStartOrdinal: key.start,
					MessageEndOrdinal:   key.end,
				})
				if err != nil {
					if isRecallEvidenceValidationError(err) {
						return nil, extractDriftErrorf(
							"evidence selection %d-%d for session %s is "+
								"invalid (%v)", key.start, key.end,
							u.SessionID, err)
					}
					return nil, err
				}
				bound[key] = metadata
			}
			entry.Evidence[j].ContentDigest = metadata.ContentDigest
			entry.Evidence[j].MessageStartSourceUUID =
				metadata.MessageStartSourceUUID
			entry.Evidence[j].MessageEndSourceUUID =
				metadata.MessageEndSourceUUID
		}
		entries[i] = entry
	}
	return entries, nil
}

// extractSessionIneligibleSQL matches sessions (aliased s) whose extraction
// output must be retracted: trashed, flagged automated, or carrying secret
// findings or leaks. Stale or missing scan versions deliberately do not
// qualify — they are transient (every transcript write clears the stamp
// until rescan) and retracting on them would rebuild the corpus on every
// sync.
const extractSessionIneligibleSQL = `s.deleted_at IS NOT NULL
	OR s.is_automated != 0
	OR s.secret_leak_count > 0
	OR EXISTS (SELECT 1 FROM secret_findings sf WHERE sf.session_id = s.id)`

// ReconcileIneligibleExtractSessions removes the generated corpus of
// sessions that lost extraction eligibility after extraction. It is
// generation-independent — a retired generation keeps serving until the next
// activation, so its entries must be retracted too. Ineligible sessions'
// unreviewed_auto entries under any registered generation are deleted and
// their progress rows removed across generations, so nothing keeps serving
// from an excluded session, a lingering pending or partial row cannot block
// activation forever, and a session that becomes eligible again is
// rediscovered and re-extracted from scratch. Both deletes are set-based —
// no per-session host parameters, so retraction cannot be blocked by
// SQLite's parameter limit however many sessions match. changedSince bounds
// the walk: every ineligibility write records a local write, so the zero
// value (fresh manager) is the only unbounded path. Returns how many
// progress rows were removed and how many entries were deleted.
func (db *DB) ReconcileIneligibleExtractSessions(
	ctx context.Context, changedSince time.Time,
) (int, int, error) {
	if err := db.requireWritable(); err != nil {
		return 0, 0, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := db.getWriter().Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("begin extract reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ineligible := `
		SELECT s.id FROM sessions s
		WHERE (` + extractSessionIneligibleSQL + `)`
	var bound []any
	if !changedSince.IsZero() {
		ineligible += `
		  AND s.local_modified_at >= ?`
		bound = append(bound,
			changedSince.UTC().Format(extractTimeLayout))
	}
	entryArgs := make([]any, 0, len(bound)+1)
	entryArgs = append(entryArgs, corerecall.ReviewStateUnreviewedAuto)
	entryArgs = append(entryArgs, bound...)
	result, err := tx.ExecContext(ctx, `
		DELETE FROM recall_entries
		WHERE review_state = ?
		  AND source_run_id IN
		      (SELECT fingerprint FROM recall_extract_generations)
		  AND source_session_id IN (`+ineligible+`)`, entryArgs...)
	if err != nil {
		return 0, 0, fmt.Errorf("deleting ineligible entries: %w", err)
	}
	entriesDeleted, err := result.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("counting reconciled entries: %w", err)
	}
	result, err = tx.ExecContext(ctx, `
		DELETE FROM recall_extract_progress
		WHERE session_id IN (`+ineligible+`)`, bound...)
	if err != nil {
		return 0, 0, fmt.Errorf("removing ineligible progress rows: %w", err)
	}
	rowsRemoved, err := result.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("counting reconciled progress rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit extract reconcile: %w", err)
	}
	return int(rowsRemoved), int(entriesDeleted), nil
}

// DiscardExtractedSessionOutput atomically deletes one session's generated
// entries under one generation and reopens its progress row as a retryable
// failure. The two must move together: deleting the entries while leaving
// the cursor past them would let a later resume skip units whose output no
// longer exists. A stale guard (digest or cursor moved, another writer took
// over) rolls the whole discard back and returns ErrStaleExtractProgress.
func (db *DB) DiscardExtractedSessionOutput(
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
	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("begin extract discard: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM recall_entries
		WHERE source_run_id = ? AND source_session_id = ?
			AND review_state = ?`,
		failure.Fingerprint, failure.SessionID,
		corerecall.ReviewStateUnreviewedAuto,
	); err != nil {
		return fmt.Errorf(
			"discarding entries for session %s: %w", failure.SessionID, err)
	}
	result, err := tx.ExecContext(ctx, `
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
			"reopening progress for session %s: %w", failure.SessionID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf(
			"reopening progress for session %s: %w", failure.SessionID, err)
	}
	if affected == 0 {
		return fmt.Errorf(
			"discarding session %s under digest %s at cursor %d: %w",
			failure.SessionID, failure.ExpectedDigest,
			failure.ExpectedCursor, ErrStaleExtractProgress)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit extract discard: %w", err)
	}
	return nil
}

// syncExtractedEntryContextTx refreshes the session-derived context fields
// (project, cwd, git branch, agent) on one session's generated entries under
// one generation. Entries copy those fields from the session row at insert
// time, and a metadata-only session update keeps the unit digest unchanged —
// without this sync a same-digest revisit would settle the coverage stamp
// while the entries kept matching Recall filters for the old context.
// Human-touched entries are left as they were, mirroring the delete path.
// Returns how many entries changed.
func syncExtractedEntryContextTx(
	ctx context.Context, tx *sql.Tx, fingerprint string, session *Session,
) (int, error) {
	result, err := tx.ExecContext(ctx, `
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

// rebindExtractedSessionEvidenceTx re-derives evidence provenance for one
// session's machine entries against the current transcript: each evidence
// range is rebuilt through the verifying window, its content digest and
// endpoint UUIDs are re-stamped, and revoked provenance is restored.
// Evidence digests cover rows the units digest ignores (system and empty
// messages), so the reconciler can revoke an entry whose extraction output
// never changed; a same-digest revisit repairs such entries instead of
// settling the coverage stamp over a permanently dark corpus. Returns how
// many entries had provenance restored.
func rebindExtractedSessionEvidenceTx(
	ctx context.Context, tx *sql.Tx, fingerprint, sessionID string,
) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT e.id, e.entry_id,
		       e.message_start_ordinal, e.message_end_ordinal
		FROM recall_evidence e
		JOIN recall_entries r ON r.id = e.entry_id
		WHERE r.source_run_id = ? AND r.source_session_id = ?
		  AND r.review_state = 'unreviewed_auto'
		  AND e.session_id = ?`,
		fingerprint, sessionID, sessionID)
	if err != nil {
		return 0, fmt.Errorf(
			"reading evidence for session %s: %w", sessionID, err)
	}
	type evidenceRow struct {
		id         int64
		entryID    string
		start, end int
	}
	var evidence []evidenceRow
	for rows.Next() {
		var row evidenceRow
		if err := rows.Scan(
			&row.id, &row.entryID, &row.start, &row.end,
		); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scanning evidence row: %w", err)
		}
		evidence = append(evidence, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf(
			"reading evidence for session %s: %w", sessionID, err)
	}
	type ordinalRange struct{ start, end int }
	bound := make(map[ordinalRange]RecallEvidenceSelectionMetadata)
	entryIDs := make(map[string]bool)
	for _, row := range evidence {
		key := ordinalRange{row.start, row.end}
		metadata, ok := bound[key]
		if !ok {
			window, err := buildRecallEvidenceWindow(
				ctx, tx, sessionID, key.start, key.end)
			if err != nil {
				return 0, fmt.Errorf(
					"rebinding evidence %d-%d for session %s: %w",
					key.start, key.end, sessionID, err)
			}
			metadata, err = window.BindSelection(RecallEvidenceSelection{
				MessageStartOrdinal: key.start,
				MessageEndOrdinal:   key.end,
			})
			if err != nil {
				return 0, fmt.Errorf(
					"rebinding evidence %d-%d for session %s: %w",
					key.start, key.end, sessionID, err)
			}
			bound[key] = metadata
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE recall_evidence
			SET content_digest = ?,
			    message_start_source_uuid = ?,
			    message_end_source_uuid = ?
			WHERE id = ?`,
			metadata.ContentDigest,
			metadata.MessageStartSourceUUID,
			metadata.MessageEndSourceUUID,
			row.id,
		); err != nil {
			return 0, fmt.Errorf(
				"re-stamping evidence for entry %s: %w", row.entryID, err)
		}
		entryIDs[row.entryID] = true
	}
	restored := 0
	for entryID := range entryIDs {
		result, err := tx.ExecContext(ctx, `
			UPDATE recall_entries
			SET provenance_ok = 1,
			    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			WHERE id = ? AND provenance_ok = 0`,
			entryID,
		)
		if err != nil {
			return 0, fmt.Errorf(
				"restoring provenance for entry %s: %w", entryID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf(
				"restoring provenance for entry %s: %w", entryID, err)
		}
		restored += int(affected)
	}
	return restored, nil
}

// ExtractCoverageRefresh is one same-digest revisit's guarded write: the
// bracketed session snapshot to verify, the digest the revisit re-derived,
// and the pre-read cutoff to stamp as coverage.
type ExtractCoverageRefresh struct {
	Fingerprint  string
	Digest       string
	StampedAt    time.Time
	ScanVersions []string
	Session      *Session
}

// RefreshExtractedSessionCoverage settles a same-digest revisit atomically:
// one transaction re-verifies the session snapshot and privacy predicates,
// confirms the progress row still carries the expected digest, synchronizes
// entry context, rebinds evidence provenance against the current
// transcript, and advances the coverage stamp. A snapshot mismatch returns
// ErrExtractSessionDrifted and a digest mismatch ErrStaleExtractProgress,
// with nothing persisted — a transcript write landing mid-refresh must not
// see stale entries rebound to it, marked verified, and stamped covered.
func (db *DB) RefreshExtractedSessionCoverage(
	ctx context.Context, u ExtractCoverageRefresh,
) (ExtractProgress, error) {
	var zero ExtractProgress
	if err := db.requireWritable(); err != nil {
		return zero, err
	}
	if u.Session == nil {
		return zero, fmt.Errorf(
			"refreshing coverage for generation %s requires the bracketed "+
				"session snapshot", u.Fingerprint)
	}
	if len(u.ScanVersions) == 0 {
		return zero, fmt.Errorf(
			"refreshing coverage for session %s requires the current "+
				"secret-scan versions", u.Session.ID)
	}
	if u.StampedAt.IsZero() {
		return zero, fmt.Errorf(
			"refreshing coverage for session %s requires the "+
				"transcript-read cutoff", u.Session.ID)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := db.getWriter().Begin()
	if err != nil {
		return zero, fmt.Errorf("begin coverage refresh: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := verifyExtractSessionGuardTx(ctx, tx, ExtractSessionGuard{
		SessionID:          u.Session.ID,
		ScanVersions:       u.ScanVersions,
		MessageCount:       u.Session.MessageCount,
		TranscriptRevision: u.Session.TranscriptRevision,
		LocalModifiedAt:    u.Session.LocalModifiedAt,
		EndedAt:            u.Session.EndedAt,
	}); err != nil {
		return zero, err
	}
	var storedDigest string
	err = tx.QueryRowContext(ctx, `
		SELECT content_digest FROM recall_extract_progress
		WHERE session_id = ? AND generation_fingerprint = ?`,
		u.Session.ID, u.Fingerprint,
	).Scan(&storedDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, fmt.Errorf(
			"no progress row for session %s: %w",
			u.Session.ID, ErrStaleExtractProgress)
	}
	if err != nil {
		return zero, fmt.Errorf(
			"reading progress for session %s: %w", u.Session.ID, err)
	}
	if storedDigest != u.Digest {
		return zero, fmt.Errorf(
			"progress digest for session %s moved: %w",
			u.Session.ID, ErrStaleExtractProgress)
	}
	if _, err := syncExtractedEntryContextTx(
		ctx, tx, u.Fingerprint, u.Session,
	); err != nil {
		return zero, err
	}
	if _, err := rebindExtractedSessionEvidenceTx(
		ctx, tx, u.Fingerprint, u.Session.ID,
	); err != nil {
		return zero, err
	}
	// The stamp advance mirrors the same-digest upsert: a failed row keeps
	// updated_at so the refresh does not reset the failure backoff clock.
	if _, err := tx.ExecContext(ctx, `
		UPDATE recall_extract_progress
		SET content_stamped_at = ?,
			updated_at = CASE WHEN state = ? THEN updated_at
				ELSE strftime('%Y-%m-%dT%H:%M:%fZ','now') END
		WHERE session_id = ? AND generation_fingerprint = ?`,
		u.StampedAt.UTC().Format(extractTimeLayout), ExtractProgressFailed,
		u.Session.ID, u.Fingerprint,
	); err != nil {
		return zero, fmt.Errorf(
			"stamping coverage for session %s: %w", u.Session.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return zero, fmt.Errorf("commit coverage refresh: %w", err)
	}
	progress, ok, err := db.ExtractProgress(ctx, u.Session.ID, u.Fingerprint)
	if err != nil {
		return zero, err
	}
	if !ok {
		return zero, fmt.Errorf(
			"extract progress for session %s vanished after refresh",
			u.Session.ID)
	}
	return progress, nil
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
