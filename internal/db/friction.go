package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/stringutil"
)

// FrictionFinding is one persisted friction detection. Rows
// are derived from the session's stored messages and replaced per
// session; natural coordinates let them survive the resync orphan copy.
type FrictionFinding struct {
	SessionID      string
	Kind           string
	Detector       string
	MessageOrdinal *int
	CallIndex      *int
	ToolName       string
	Label          string
	Text           string
	Evidence       string
	Title          string
	Fingerprint    string
	OccurredAt     *time.Time
	Seq            int
	RulesVersion   string
}

// SessionFrictionUpdate is a session's complete friction output. It rides
// on SessionSignalUpdate so every signal write path persists it in the
// same transaction. A nil *SessionFrictionUpdate leaves stored friction
// untouched.
type SessionFrictionUpdate struct {
	Findings     []FrictionFinding
	RulesVersion string
	Hash         string
}

// FrictionHash is the canonical change fingerprint stored in
// sessions.friction_hash and folded into the PG push fingerprint.
// Findings must be in seq order. The field order is part of the storage
// contract, so changing it re-pushes every session.
func FrictionHash(
	findings []FrictionFinding, rulesVersion string,
) string {
	h := sha256.New()
	buf := make([]byte, 0, 256)
	write := func(f string) {
		buf = strconv.AppendInt(buf[:0], int64(len(f)), 10)
		buf = append(buf, ':')
		buf = append(buf, f...)
		h.Write(buf)
	}
	write("friction-hash-v1")
	write(rulesVersion)
	write(strconv.Itoa(len(findings)))
	for _, f := range findings {
		write(f.Kind)
		write(f.Detector)
		write(optFrictionInt(f.MessageOrdinal))
		write(optFrictionInt(f.CallIndex))
		write(f.ToolName)
		write(f.Label)
		write(f.Text)
		write(f.Evidence)
		write(f.Title)
		write(f.Fingerprint)
		write(formatFrictionTime(f.OccurredAt))
		write(strconv.Itoa(f.Seq))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func optFrictionInt(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

// formatFrictionTime is the single text form used for occurred_at in
// SQLite and in FrictionHash, so a recompute from stored rows matches.
func formatFrictionTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func frictionTimeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatFrictionTime(t)
}

// replaceSessionFrictionTx deletes a session's findings, inserts
// the new set, and stamps the sessions summary columns. Caller owns the
// lock and transaction lifecycle.
func replaceSessionFrictionTx(
	tx transactionQueries, sessionID string, u SessionFrictionUpdate,
) error {
	changed, err := replaceSessionFrictionRowsTx(tx, sessionID, u)
	if err != nil || !changed {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE sessions
		SET friction_count = ?,
		    friction_rules_version = ?,
		    friction_hash = ?,
		    local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`,
		len(u.Findings), u.RulesVersion, u.Hash, sessionID,
	); err != nil {
		return fmt.Errorf("updating session friction columns %s: %w", sessionID, err)
	}
	return nil
}

// replaceSessionFrictionRowsTx replaces a session's finding rows and reports
// whether they changed; the caller stamps the sessions summary columns.
func replaceSessionFrictionRowsTx(
	tx transactionQueries, sessionID string, u SessionFrictionUpdate,
) (bool, error) {
	var storedCount int
	var storedVersion, storedHash string
	if err := tx.QueryRow(`
		SELECT friction_count, friction_rules_version, friction_hash
		FROM sessions WHERE id = ?`, sessionID,
	).Scan(&storedCount, &storedVersion, &storedHash); err != nil {
		return false, fmt.Errorf("checking session friction %s: %w", sessionID, err)
	}
	if storedCount == len(u.Findings) && storedVersion == u.RulesVersion && storedHash == u.Hash {
		return false, nil
	}
	// A session that stored no findings has no rows to delete, which saves
	// a statement per session on bulk imports.
	if storedCount > 0 {
		if _, err := tx.Exec(
			"DELETE FROM friction_findings WHERE session_id = ?", sessionID,
		); err != nil {
			return false, fmt.Errorf("deleting friction findings for %s: %w", sessionID, err)
		}
	}
	// Fourteen parameters per row keep each statement below 999 variables.
	const rowsPerStmt = 70
	sessionArg, rulesArg := any(sessionID), any(u.RulesVersion)
	for start := 0; start < len(u.Findings); start += rowsPerStmt {
		batch := u.Findings[start:min(start+rowsPerStmt, len(u.Findings))]
		args := make([]any, 0, len(batch)*14)
		for i := range batch {
			f := &batch[i]
			args = append(args,
				sessionArg, f.Kind, f.Detector, f.MessageOrdinal, f.CallIndex,
				f.ToolName, f.Label, f.Text, f.Evidence, f.Title, f.Fingerprint,
				frictionTimeArg(f.OccurredAt), f.Seq, rulesArg,
			)
		}
		if _, err := tx.Exec(`
			INSERT INTO friction_findings (
				session_id, kind, detector, message_ordinal, call_index,
				tool_name, label, text, evidence, title, fingerprint,
				occurred_at, seq, rules_version
			) VALUES `+multiRowPlaceholders(len(batch), 14), args...,
		); err != nil {
			return false, fmt.Errorf("inserting friction finding for %s: %w", sessionID, err)
		}
	}
	return true, nil
}

// SettledFriction is the terminal friction state for a usage-only archive,
// which stores no reviewable text: no rows, current version, so backfill
// does not revisit it.
func SettledFriction() SessionFrictionUpdate {
	return SessionFrictionUpdate{
		RulesVersion: friction.RulesVersion,
		Hash:         FrictionHash(nil, friction.RulesVersion),
	}
}

// SkippedFriction records a session over the review budget so backfill stops
// revisiting it while readers can still tell it was never reviewed.
func SkippedFriction() SessionFrictionUpdate {
	return SessionFrictionUpdate{
		RulesVersion: friction.SkippedRulesVersion,
		Hash:         FrictionHash(nil, friction.SkippedRulesVersion),
	}
}

// clearSessionFrictionTx drops a session's friction and marks it for
// recompute with an empty rules version after copied content is projected.
func clearSessionFrictionTx(tx transactionQueries, sessionID string) error {
	return replaceSessionFrictionTx(tx, sessionID, SessionFrictionUpdate{})
}

// ReplaceSessionFrictionAtRevision publishes a friction snapshot only when
// the stored transcript, hierarchy, detector, and pressure
// inputs still match the session snapshot used to compute it. It reports
// whether the write applied. Recompute paths that load stored rows outside
// the content transaction use it so concurrent input changes cannot stamp
// stale findings as current.
// A rejected snapshot leaves findings untouched and clears the rules-version
// marker so a later friction backfill retries the session.
func (db *DB) ReplaceSessionFrictionAtRevision(
	ctx context.Context, expected Session,
	u SessionFrictionUpdate,
) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning friction tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var (
		currentRevision, currentParent    sql.NullString
		currentRelationship, currentAgent string
		currentPressure                   sql.NullFloat64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT transcript_revision, parent_session_id, relationship_type,
		        agent, context_pressure_max
		 FROM sessions WHERE id = ?`, expected.ID,
	).Scan(&currentRevision, &currentParent, &currentRelationship,
		&currentAgent, &currentPressure)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading friction inputs %s: %w", expected.ID, err)
	}
	expectedRevision := ""
	if expected.TranscriptRevision != nil {
		expectedRevision = *expected.TranscriptRevision
	}
	parentMatches := !currentParent.Valid && expected.ParentSessionID == nil
	if expected.ParentSessionID != nil {
		parentMatches = currentParent.Valid && currentParent.String == *expected.ParentSessionID
	}
	pressureMatches := !currentPressure.Valid && expected.ContextPressureMax == nil
	if expected.ContextPressureMax != nil {
		pressureMatches = currentPressure.Valid &&
			currentPressure.Float64 == *expected.ContextPressureMax
	}
	if currentRevision.String != expectedRevision || !parentMatches ||
		currentRelationship != expected.RelationshipType ||
		currentAgent != expected.Agent || !pressureMatches {
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET friction_rules_version = '',
			    local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
			WHERE id = ? AND friction_rules_version <> ''`, expected.ID,
		); err != nil {
			return false, fmt.Errorf(
				"marking stale friction snapshot %s: %w", expected.ID, err,
			)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf(
				"committing stale friction marker %s: %w", expected.ID, err,
			)
		}
		return false, nil
	}
	if err := replaceSessionFrictionTx(tx, expected.ID, u); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// SessionFrictionFindings returns a session's findings in seq order. It is
// the PG push source.
func (db *DB) SessionFrictionFindings(
	ctx context.Context, sessionID string,
) ([]FrictionFinding, error) {
	out := make([]FrictionFinding, 0)
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT session_id, kind, detector, message_ordinal, call_index,
		       tool_name, label, text, evidence, title, fingerprint,
		       occurred_at, seq, rules_version
		FROM friction_findings
		WHERE session_id = ?
		ORDER BY seq, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("querying friction findings for %s: %w", sessionID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var f FrictionFinding
		var occurred sql.NullString
		if err := rows.Scan(
			&f.SessionID, &f.Kind, &f.Detector, &f.MessageOrdinal,
			&f.CallIndex, &f.ToolName, &f.Label, &f.Text, &f.Evidence,
			&f.Title, &f.Fingerprint, &occurred, &f.Seq, &f.RulesVersion,
		); err != nil {
			return nil, fmt.Errorf("scanning friction finding: %w", err)
		}
		if occurred.Valid && occurred.String != "" {
			t, err := time.Parse(time.RFC3339Nano, occurred.String)
			if err != nil {
				return nil, fmt.Errorf("parsing friction occurred_at %q: %w", occurred.String, err)
			}
			f.OccurredAt = &t
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// StaleFrictionSessions returns up to limit sessions whose findings predate
// the current rules. A session skipped under those rules counts as current.
func (db *DB) StaleFrictionSessions(ctx context.Context, limit int) ([]string, error) {
	rows, err := db.getReader().QueryContext(ctx, staleFrictionSessionsSQL,
		friction.RulesVersion, friction.SkippedRulesVersion, limit)
	if err != nil {
		return nil, fmt.Errorf("querying stale friction sessions: %w", err)
	}
	return scanStrings(rows)
}

// Separate version ranges let SQLite seek the version index past current
// sessions instead of scanning the archive.
const staleFrictionSessionsWhere = `friction_rules_version < ?1
	   OR (friction_rules_version > ?1 AND friction_rules_version < ?2)
	   OR friction_rules_version > ?2`

const staleFrictionSessionsSQL = `SELECT id FROM sessions
	WHERE ` + staleFrictionSessionsWhere + `
	LIMIT ?3`

const countStaleFrictionSessionsSQL = `SELECT COUNT(*) FROM sessions
	WHERE ` + staleFrictionSessionsWhere

// CountStaleFrictionSessions counts sessions that need the current rules.
func (db *DB) CountStaleFrictionSessions(ctx context.Context) (int, error) {
	var count int
	if err := db.getReader().QueryRow(ctx, countStaleFrictionSessionsSQL,
		friction.RulesVersion, friction.SkippedRulesVersion,
	).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting stale friction sessions: %w", err)
	}
	return count, nil
}

// FrictionBackfillName identifies the resumable migration record.
const FrictionBackfillName = "friction_backfill_v1"

// MarkFrictionBackfill records the current backfill pass and progress.
func (db *DB) MarkFrictionBackfill(
	ctx context.Context, state string, total, completed int, lastErr string,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	lastErr = stringutil.SafeTruncate(lastErr, 1024)
	_, err := db.getWriter().Exec(ctx, `
		INSERT INTO background_migrations (
			name, state, total_items, completed_items, last_error,
			started_at, completed_at
		) VALUES (?, ?, ?, ?, ?,
			strftime('%Y-%m-%dT%H:%M:%fZ','now'),
			CASE WHEN ? = 'completed'
				THEN strftime('%Y-%m-%dT%H:%M:%fZ','now') END)
		ON CONFLICT(name) DO UPDATE SET
			state = excluded.state,
			total_items = excluded.total_items,
			completed_items = excluded.completed_items,
			last_error = excluded.last_error,
			started_at = CASE WHEN background_migrations.state = 'running'
				THEN background_migrations.started_at
				ELSE excluded.started_at END,
			completed_at = excluded.completed_at,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')`,
		FrictionBackfillName, state, total, completed, lastErr, state,
	)
	if err != nil {
		return fmt.Errorf("recording friction backfill state: %w", err)
	}
	return nil
}

// FrictionBackfillState returns the recorded state, or empty when none exists.
func (db *DB) FrictionBackfillState(ctx context.Context) (string, error) {
	var state string
	err := db.getReader().QueryRow(ctx,
		`SELECT state FROM background_migrations WHERE name = ?`,
		FrictionBackfillName,
	).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading friction backfill state: %w", err)
	}
	return state, nil
}
