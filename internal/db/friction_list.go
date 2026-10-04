package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Friction list paging defaults. The API clamps to the same bounds.
const (
	DefaultFrictionListLimit = 100
	MaxFrictionListLimit     = 1000
)

// Pattern link-state filter values.
const (
	FrictionLinkStateLinked   = "linked"
	FrictionLinkStateUnlinked = "unlinked"
)

// FrictionPattern is one local recurrence row from friction_patterns.
type FrictionPattern struct {
	Fingerprint     string             `json:"fingerprint"`
	Kind            string             `json:"kind"`
	Title           string             `json:"title"`
	FirstSeenDate   string             `json:"first_seen_date"`
	LastSeenDate    string             `json:"last_seen_date"`
	OccurrenceCount int                `json:"occurrence_count"`
	SessionCount    int                `json:"session_count"`
	LastSubjectID   string             `json:"last_subject_id"`
	LastOrdinal     *int               `json:"last_ordinal"`
	Link            *FrictionIssueLink `json:"link,omitempty"`
}

// FrictionFindingFilter narrows ListFrictionFindings. Date selects the
// sessions recorded in that digest's membership (friction_digest_sessions).
type FrictionFindingFilter struct {
	Date        string
	Kind        string
	SessionID   string
	Fingerprint string
	Limit       int
	Cursor      string
}

// FrictionPatternFilter narrows ListFrictionPatterns. Since keeps patterns
// whose last_seen_date is on or after the given date.
type FrictionPatternFilter struct {
	Kind      string
	LinkState string
	Since     string
	Limit     int
	Cursor    string
}

// NormalizeFrictionLimit maps an unset or out-of-range page size to the default.
func NormalizeFrictionLimit(n int) int {
	if n <= 0 || n > MaxFrictionListLimit {
		return DefaultFrictionListLimit
	}
	return n
}

// DecodeFrictionCursor parses the opaque offset cursor used by friction lists.
func DecodeFrictionCursor(c string) (int, error) {
	if c == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(c)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: friction cursor %q", ErrInvalidCursor, c)
	}
	return n, nil
}

// EncodeFrictionCursor returns the cursor for offset, or "" at the start.
func EncodeFrictionCursor(offset int) string {
	if offset <= 0 {
		return ""
	}
	return strconv.Itoa(offset)
}

func parseFrictionTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing friction timestamp %q: %w", s, err)
	}
	return t, nil
}

func nullIntPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	return new(int(v.Int64))
}

// ListFrictionFindings returns stored findings for live sessions.
func (db *DB) ListFrictionFindings(
	ctx context.Context, f FrictionFindingFilter,
) ([]FrictionFinding, string, error) {
	limit := NormalizeFrictionLimit(f.Limit)
	offset, err := DecodeFrictionCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	preds := []string{"s.deleted_at IS NULL"}
	var args []any
	add := func(p string, v any) { preds = append(preds, p); args = append(args, v) }
	if f.Date != "" {
		add(`ff.session_id IN (SELECT subject_id FROM friction_digest_sessions
			WHERE date = ? AND subject_kind = 'session')`, f.Date)
	}
	if f.Kind != "" {
		add("ff.kind = ?", f.Kind)
	}
	if f.SessionID != "" {
		add("ff.session_id = ?", f.SessionID)
	}
	if f.Fingerprint != "" {
		add("ff.fingerprint = ?", f.Fingerprint)
	}
	query := `
		SELECT ff.session_id, ff.kind, ff.detector, ff.message_ordinal,
			ff.call_index, ff.tool_name, ff.label, ff.text, ff.evidence,
			ff.title, ff.fingerprint, ff.occurred_at, ff.seq, ff.rules_version
		FROM friction_findings ff JOIN sessions s ON s.id = ff.session_id
		WHERE ` + strings.Join(preds, " AND ") + `
		ORDER BY ff.session_id, ff.seq, ff.id
		LIMIT ? OFFSET ?`
	args = append(args, limit+1, offset)
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", fmt.Errorf("listing friction findings: %w", err)
	}
	defer rows.Close()
	out := make([]FrictionFinding, 0, limit+1)
	for rows.Next() {
		var (
			r                  FrictionFinding
			ordinal, callIndex sql.NullInt64
			occurred           sql.NullString
		)
		if err := rows.Scan(&r.SessionID, &r.Kind, &r.Detector, &ordinal,
			&callIndex, &r.ToolName, &r.Label, &r.Text, &r.Evidence,
			&r.Title, &r.Fingerprint, &occurred, &r.Seq, &r.RulesVersion); err != nil {
			return nil, "", fmt.Errorf("scanning friction finding: %w", err)
		}
		r.MessageOrdinal = nullIntPtr(ordinal)
		r.CallIndex = nullIntPtr(callIndex)
		if occurred.Valid && occurred.String != "" {
			ts, err := parseFrictionTime(occurred.String)
			if err != nil {
				return nil, "", err
			}
			r.OccurredAt = &ts
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("listing friction findings: %w", err)
	}
	if len(out) > limit {
		return out[:limit], EncodeFrictionCursor(offset + limit), nil
	}
	return out, "", nil
}

// ListFrictionDigests returns digest metadata, newest first, without the
// snapshot, summary and Markdown blobs.
func (db *DB) ListFrictionDigests(
	ctx context.Context, from, to string,
) ([]FrictionDigest, error) {
	var preds []string
	var args []any
	if from != "" {
		preds = append(preds, "date >= ?")
		args = append(args, from)
	}
	if to != "" {
		preds = append(preds, "date <= ?")
		args = append(args, to)
	}
	where := ""
	if len(preds) > 0 {
		where = "WHERE " + strings.Join(preds, " AND ")
	}
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT date, timezone, rules_version, built_at, revision,
			sessions_scanned, markdown_sha256, run_id
		FROM friction_digests `+where+`
		ORDER BY date DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("listing friction digests: %w", err)
	}
	defer rows.Close()
	out := []FrictionDigest{}
	for rows.Next() {
		var (
			d     FrictionDigest
			built string
		)
		if err := rows.Scan(&d.Date, &d.Timezone, &d.RulesVersion, &built,
			&d.Revision, &d.SessionsScanned, &d.MarkdownSHA256, &d.RunID); err != nil {
			return nil, fmt.Errorf("scanning friction digest: %w", err)
		}
		if d.BuiltAt, err = parseFrictionTime(built); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing friction digests: %w", err)
	}
	return out, nil
}

// ListFrictionPatterns returns local recurrence rows ranked by occurrences.
func (db *DB) ListFrictionPatterns(
	ctx context.Context, f FrictionPatternFilter,
) ([]FrictionPattern, string, error) {
	limit := NormalizeFrictionLimit(f.Limit)
	offset, err := DecodeFrictionCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	var preds []string
	var args []any
	if f.Kind != "" {
		preds = append(preds, "kind = ?")
		args = append(args, f.Kind)
	}
	if f.Since != "" {
		preds = append(preds, "last_seen_date >= ?")
		args = append(args, f.Since)
	}
	clause, err := FrictionLinkStateClause(f.LinkState, "friction_patterns.fingerprint", func(value any) string {
		args = append(args, value)
		return "?"
	})
	if err != nil {
		return nil, "", err
	}
	if clause != "" {
		preds = append(preds, clause)
	}
	where := ""
	if len(preds) > 0 {
		where = "WHERE " + strings.Join(preds, " AND ")
	}
	args = append(args, limit+1, offset)
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT fingerprint, kind, title, first_seen_date, last_seen_date,
			occurrence_count, session_count, last_subject_id, last_ordinal
		FROM friction_patterns `+where+`
		ORDER BY occurrence_count DESC, last_seen_date DESC, fingerprint ASC
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("listing friction patterns: %w", err)
	}
	defer rows.Close()
	out := make([]FrictionPattern, 0, limit+1)
	for rows.Next() {
		var (
			p       FrictionPattern
			ordinal sql.NullInt64
		)
		if err := rows.Scan(&p.Fingerprint, &p.Kind, &p.Title, &p.FirstSeenDate,
			&p.LastSeenDate, &p.OccurrenceCount, &p.SessionCount,
			&p.LastSubjectID, &ordinal); err != nil {
			return nil, "", fmt.Errorf("scanning friction pattern: %w", err)
		}
		p.LastOrdinal = nullIntPtr(ordinal)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("listing friction patterns: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, "", fmt.Errorf("closing friction patterns: %w", err)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = EncodeFrictionCursor(offset + limit)
	}
	fingerprints := make([]string, len(out))
	for i := range out {
		fingerprints[i] = out[i].Fingerprint
	}
	links, err := db.GetFrictionIssueLinks(ctx, fingerprints)
	if err != nil {
		return nil, "", err
	}
	for i := range out {
		if link, ok := links[out[i].Fingerprint]; ok {
			out[i].Link = &link
		}
	}
	return out, next, nil
}
