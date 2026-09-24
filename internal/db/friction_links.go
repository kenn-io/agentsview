package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// FrictionIssueLink is the Kata linkage and outbox row for one pattern
// (spec §5.4). Issue status, title and closed reason are never stored.
type FrictionIssueLink struct {
	Fingerprint        string     `json:"fingerprint"`
	State              string     `json:"state"`
	KataInstanceUID    string     `json:"kata_instance_uid"`
	KataProjectUID     string     `json:"kata_project_uid"`
	IssueUID           string     `json:"issue_uid"`
	QualifiedID        string     `json:"qualified_id"`
	WebURL             string     `json:"web_url"`
	LinkSource         string     `json:"link_source"`
	Attempts           int        `json:"attempts"`
	FirstFailedAt      *time.Time `json:"first_failed_at,omitempty"`
	NextAttemptAt      *time.Time `json:"next_attempt_at,omitempty"`
	LastErrorCode      string     `json:"last_error_code"`
	LastError          string     `json:"last_error"`
	CandidatesJSON     string     `json:"candidates_json"`
	LastRecurrenceDate string     `json:"last_recurrence_date"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// Link states beyond PR 6's FrictionLinkStateLinked/FrictionLinkStateUnlinked.
const (
	FrictionLinkStatePending    = "pending"
	FrictionLinkStateFailed     = "failed"
	FrictionLinkStateNeedsHuman = "needs_human"
	FrictionLinkStateAbandoned  = "abandoned"

	FrictionLinkSourceCreated         = "created"
	FrictionLinkSourceFound           = "found"
	FrictionLinkSourceIdempotentReuse = "idempotent_reuse"
	FrictionLinkSourceManual          = "manual"
)

func ValidFrictionLinkState(s string) bool {
	switch s {
	case FrictionLinkStatePending, FrictionLinkStateLinked, FrictionLinkStateFailed, FrictionLinkStateNeedsHuman, FrictionLinkStateAbandoned:
		return true
	}
	return false
}

const frictionLinkCols = `fingerprint, state, kata_instance_uid, kata_project_uid,
	issue_uid, qualified_id, web_url, link_source, attempts, first_failed_at,
	next_attempt_at, last_error_code, last_error, candidates_json,
	last_recurrence_date, updated_at`

// linkTime is the fixed-width UTC text form, so SQLite string comparison in
// DueFrictionFilings orders correctly.
func linkTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func linkTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return linkTime(*t)
}

func parseLinkTime(ns sql.NullString) (*time.Time, error) {
	if !ns.Valid || ns.String == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, ns.String)
	if err != nil {
		return nil, fmt.Errorf("parsing friction link time %q: %w", ns.String, err)
	}
	return &t, nil
}

func scanFrictionLink(r rowScanner) (FrictionIssueLink, error) {
	var l FrictionIssueLink
	var first, next sql.NullString
	var updated string
	if err := r.Scan(&l.Fingerprint, &l.State, &l.KataInstanceUID, &l.KataProjectUID,
		&l.IssueUID, &l.QualifiedID, &l.WebURL, &l.LinkSource, &l.Attempts, &first,
		&next, &l.LastErrorCode, &l.LastError, &l.CandidatesJSON,
		&l.LastRecurrenceDate, &updated); err != nil {
		return l, err
	}
	var err error
	if l.FirstFailedAt, err = parseLinkTime(first); err != nil {
		return l, err
	}
	if l.NextAttemptAt, err = parseLinkTime(next); err != nil {
		return l, err
	}
	u, err := parseLinkTime(sql.NullString{String: updated, Valid: true})
	if err != nil {
		return l, err
	}
	if u != nil {
		l.UpdatedAt = *u
	}
	return l, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func stringArgs(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// GetFrictionIssueLinks returns the stored links keyed by fingerprint.
func (db *DB) GetFrictionIssueLinks(ctx context.Context, fingerprints []string) (map[string]FrictionIssueLink, error) {
	out := make(map[string]FrictionIssueLink, len(fingerprints))
	for start := 0; start < len(fingerprints); start += 500 {
		chunk := fingerprints[start:min(start+500, len(fingerprints))]
		rows, err := db.getReader().QueryContext(ctx,
			"SELECT "+frictionLinkCols+" FROM friction_issue_links WHERE fingerprint IN ("+placeholders(len(chunk))+")",
			stringArgs(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("querying friction issue links: %w", err)
		}
		for rows.Next() {
			l, err := scanFrictionLink(rows)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("scanning friction issue link: %w", err)
			}
			out[l.Fingerprint] = l
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading friction issue links: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// UpsertFrictionIssueLink replaces the row for l.Fingerprint.
func (db *DB) UpsertFrictionIssueLink(ctx context.Context, l FrictionIssueLink) error {
	if !ValidFrictionLinkState(l.State) {
		return fmt.Errorf("unknown friction link state %q", l.State)
	}
	if l.UpdatedAt.IsZero() {
		l.UpdatedAt = time.Now()
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(ctx, `
		INSERT INTO friction_issue_links (`+frictionLinkCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET
			state = excluded.state,
			kata_instance_uid = excluded.kata_instance_uid,
			kata_project_uid = excluded.kata_project_uid,
			issue_uid = excluded.issue_uid,
			qualified_id = excluded.qualified_id,
			web_url = excluded.web_url,
			link_source = excluded.link_source,
			attempts = excluded.attempts,
			first_failed_at = excluded.first_failed_at,
			next_attempt_at = excluded.next_attempt_at,
			last_error_code = excluded.last_error_code,
			last_error = excluded.last_error,
			candidates_json = excluded.candidates_json,
			last_recurrence_date = excluded.last_recurrence_date,
			updated_at = excluded.updated_at`,
		l.Fingerprint, l.State, l.KataInstanceUID, l.KataProjectUID,
		l.IssueUID, l.QualifiedID, l.WebURL, l.LinkSource, l.Attempts, linkTimePtr(l.FirstFailedAt),
		linkTimePtr(l.NextAttemptAt), l.LastErrorCode, l.LastError, l.CandidatesJSON,
		l.LastRecurrenceDate, linkTime(l.UpdatedAt))
	if err != nil {
		return fmt.Errorf("upserting friction issue link %s: %w", l.Fingerprint, err)
	}
	return nil
}

// DeleteFrictionIssueLink removes local linkage only; Kata is untouched.
func (db *DB) DeleteFrictionIssueLink(ctx context.Context, fingerprint string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, err := db.getWriter().Exec(ctx, `DELETE FROM friction_issue_links WHERE fingerprint = ?`, fingerprint); err != nil {
		return fmt.Errorf("deleting friction issue link %s: %w", fingerprint, err)
	}
	return nil
}

// DueFrictionFilings returns pending and failed rows whose next attempt has
// passed (NULL = immediately), oldest first, bounded by limit.
func (db *DB) DueFrictionFilings(ctx context.Context, now time.Time, limit int) ([]FrictionIssueLink, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT `+frictionLinkCols+` FROM friction_issue_links
		WHERE state IN ('pending', 'failed')
		  AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
		ORDER BY next_attempt_at IS NOT NULL, next_attempt_at, fingerprint
		LIMIT ?`, linkTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("querying due friction filings: %w", err)
	}
	defer rows.Close()
	var out []FrictionIssueLink
	for rows.Next() {
		l, err := scanFrictionLink(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning due friction filing: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DigestDatesForFingerprints returns the digest dates whose session subjects
// produced any of the fingerprints, ascending.
func (db *DB) DigestDatesForFingerprints(ctx context.Context, fingerprints []string) ([]string, error) {
	if len(fingerprints) == 0 {
		return nil, nil
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT DISTINCT ds.date
		FROM friction_digest_sessions ds
		JOIN friction_findings f ON f.session_id = ds.subject_id
		WHERE f.fingerprint IN (`+placeholders(len(fingerprints))+`)
		ORDER BY ds.date`, stringArgs(fingerprints)...)
	if err != nil {
		return nil, fmt.Errorf("querying digest dates for fingerprints: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// FrictionLinkStateClause builds the ListFrictionPatterns predicate for
// FrictionPatternFilter.LinkState. "unlinked" means no linked row. bind
// records a value and returns its placeholder ("?" on SQLite, pb.add on PG).
func FrictionLinkStateClause(state, patternFingerprintCol string, bind func(v any) string) (string, error) {
	switch {
	case state == "":
		return "", nil
	case state == FrictionLinkStateUnlinked:
		return "NOT EXISTS (SELECT 1 FROM friction_issue_links l WHERE l.fingerprint = " + patternFingerprintCol + " AND l.state = " + bind(FrictionLinkStateLinked) + ")", nil
	case ValidFrictionLinkState(state):
		return "EXISTS (SELECT 1 FROM friction_issue_links l WHERE l.fingerprint = " + patternFingerprintCol + " AND l.state = " + bind(state) + ")", nil
	default:
		return "", fmt.Errorf("invalid link_state %q", state)
	}
}
