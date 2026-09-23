package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/friction"
)

// FrictionFinding is one persisted friction detection (spec §5.2). Rows
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

// FrictionSessionDims holds per-session friction dimensions (spec §5.3).
type FrictionSessionDims struct {
	SessionID      string
	Seat           string
	Persona        string
	Channel        string
	DimsSource     string
	ReviewExcluded bool
}

// SessionFrictionUpdate is a session's complete friction output. It rides
// on SessionSignalUpdate so every signal write path persists it in the
// same transaction. A nil *SessionFrictionUpdate leaves stored friction
// untouched.
type SessionFrictionUpdate struct {
	Findings     []FrictionFinding
	Dims         *FrictionSessionDims
	RulesVersion string
	Hash         string
}

// FrictionHash is the canonical change fingerprint stored in
// sessions.friction_hash and folded into the PG push fingerprint.
// Findings must be in seq order. See the plan for the field order; it is
// part of the storage contract, so changing it re-pushes every session.
func FrictionHash(
	findings []FrictionFinding, dims *FrictionSessionDims, rulesVersion string,
) string {
	h := sha256.New()
	write := func(f string) { writeFrictionField(h, f) }
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
	if dims == nil {
		write("nodims")
	} else {
		write("dims")
		write(dims.Seat)
		write(dims.Persona)
		write(dims.Channel)
		write(dims.DimsSource)
		write(strconv.FormatBool(dims.ReviewExcluded))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeFrictionField(h hash.Hash, f string) {
	fmt.Fprintf(h, "%d:%s", len(f), f)
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

// replaceSessionFrictionTx deletes a session's findings and dims, inserts
// the new set, and stamps the sessions summary columns. Caller owns the
// lock and transaction lifecycle.
func replaceSessionFrictionTx(
	tx transactionQueries, sessionID string, u SessionFrictionUpdate,
) error {
	if _, err := tx.Exec(
		"DELETE FROM friction_findings WHERE session_id = ?", sessionID,
	); err != nil {
		return fmt.Errorf("deleting friction findings for %s: %w", sessionID, err)
	}
	if _, err := tx.Exec(
		"DELETE FROM friction_session_dims WHERE session_id = ?", sessionID,
	); err != nil {
		return fmt.Errorf("deleting friction dims for %s: %w", sessionID, err)
	}
	for i := range u.Findings {
		f := &u.Findings[i]
		if _, err := tx.Exec(`
			INSERT INTO friction_findings (
				session_id, kind, detector, message_ordinal, call_index,
				tool_name, label, text, evidence, title, fingerprint,
				occurred_at, seq, rules_version
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sessionID, f.Kind, f.Detector, f.MessageOrdinal, f.CallIndex,
			f.ToolName, f.Label, f.Text, f.Evidence, f.Title, f.Fingerprint,
			frictionTimeArg(f.OccurredAt), f.Seq, u.RulesVersion,
		); err != nil {
			return fmt.Errorf("inserting friction finding for %s: %w", sessionID, err)
		}
	}
	if u.Dims != nil {
		if _, err := tx.Exec(`
			INSERT INTO friction_session_dims (
				session_id, seat, persona, channel, dims_source,
				review_excluded
			) VALUES (?, ?, ?, ?, ?, ?)`,
			sessionID, u.Dims.Seat, u.Dims.Persona, u.Dims.Channel,
			u.Dims.DimsSource, u.Dims.ReviewExcluded,
		); err != nil {
			return fmt.Errorf("inserting friction dims for %s: %w", sessionID, err)
		}
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

// settledUsageOnlyFriction is the terminal friction state for an archive
// that stores no transcript text: no rows, current version, so the
// backfill does not revisit the session on every tick.
func settledUsageOnlyFriction() SessionFrictionUpdate {
	return SessionFrictionUpdate{
		RulesVersion: friction.RulesVersion,
		Hash:         FrictionHash(nil, nil, friction.RulesVersion),
	}
}

// clearSessionFrictionTx drops a session's friction and marks it for
// recompute with an empty rules version after copied content is projected.
func clearSessionFrictionTx(tx transactionQueries, sessionID string) error {
	return replaceSessionFrictionTx(tx, sessionID, SessionFrictionUpdate{})
}

// ReplaceSessionFriction atomically replaces a session's friction findings
// and dims and updates friction_count, friction_rules_version and
// friction_hash. Under a usage-only archive policy it stores the settled
// empty state instead.
func (db *DB) ReplaceSessionFriction(
	ctx context.Context, sessionID string,
	findings []FrictionFinding, dims *FrictionSessionDims,
	rulesVersion, hash string,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning friction tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	u := SessionFrictionUpdate{
		Findings: findings, Dims: dims,
		RulesVersion: rulesVersion, Hash: hash,
	}
	if db.usageOnlyStorage() {
		u = settledUsageOnlyFriction()
	}
	if err := replaceSessionFrictionTx(tx, sessionID, u); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceSessionFrictionAtRevision publishes a friction snapshot only when
// the session's transcript_revision still equals the revision the snapshot
// was computed from. It reports whether the write applied. Recompute paths
// that load stored rows outside the content transaction use it so a
// concurrent transcript write is never overwritten with stale findings.
func (db *DB) ReplaceSessionFrictionAtRevision(
	ctx context.Context, sessionID, transcriptRevision string,
	u SessionFrictionUpdate,
) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("beginning friction tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var current sql.NullString
	err = tx.QueryRowContext(ctx,
		"SELECT transcript_revision FROM sessions WHERE id = ?", sessionID,
	).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading transcript revision %s: %w", sessionID, err)
	}
	if current.String != transcriptRevision {
		return false, nil
	}
	if db.usageOnlyStorage() {
		u = settledUsageOnlyFriction()
	}
	if err := replaceSessionFrictionTx(tx, sessionID, u); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// hasFrictionTables reports whether this archive has the friction tables.
// A read-only archive that predates them opens anyway (they are not in
// readOnlyRequiredTables), and its friction reads return empty results.
func (db *DB) hasFrictionTables(ctx context.Context) (bool, error) {
	var n int
	err := db.getReader().QueryRow(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table'
		 AND name IN ('friction_findings', 'friction_session_dims')`,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("checking friction tables: %w", err)
	}
	return n == 2, nil
}

// SessionFrictionFindings returns a session's findings in seq order. It is
// the PG push source.
func (db *DB) SessionFrictionFindings(
	ctx context.Context, sessionID string,
) ([]FrictionFinding, error) {
	out := make([]FrictionFinding, 0)
	present, err := db.hasFrictionTables(ctx)
	if err != nil {
		return nil, err
	}
	if !present {
		return out, nil
	}
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

// SessionFrictionDims returns a session's dims row, or nil when none.
func (db *DB) SessionFrictionDims(
	ctx context.Context, sessionID string,
) (*FrictionSessionDims, error) {
	present, err := db.hasFrictionTables(ctx)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	var d FrictionSessionDims
	err = db.getReader().QueryRow(ctx, `
		SELECT session_id, seat, persona, channel, dims_source,
		       review_excluded
		FROM friction_session_dims WHERE session_id = ?`, sessionID,
	).Scan(&d.SessionID, &d.Seat, &d.Persona, &d.Channel,
		&d.DimsSource, &d.ReviewExcluded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying friction dims for %s: %w", sessionID, err)
	}
	return &d, nil
}

// StaleFrictionSessions returns up to limit session ids whose stored
// friction_rules_version differs from rulesVersion, in id order. Empty
// sessions are included so they settle once at the current version.
func (db *DB) StaleFrictionSessions(
	ctx context.Context, rulesVersion string, limit int,
) ([]string, error) {
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT id FROM sessions
		WHERE friction_rules_version <> ?
		ORDER BY id
		LIMIT ?`, rulesVersion, limit)
	if err != nil {
		return nil, fmt.Errorf("querying stale friction sessions: %w", err)
	}
	return scanStrings(rows)
}
