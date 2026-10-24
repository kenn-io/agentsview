package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/friction"
)

// ErrFrictionDigestConflict reports that another builder wrote the digest
// row first (or the expected revision moved). The caller treats it as
// "already built", never as a failure to retry blindly.
var ErrFrictionDigestConflict = errors.New("friction digest was written concurrently")

// ErrFrictionMembershipConflict means another date claimed a selected subject.
// The complete save rolls back; a new build must select subjects again.
var ErrFrictionMembershipConflict = errors.New("friction subject belongs to another digest date")

// ErrFrictionSubjectsChanged requires reloading inputs after a concurrent push.
var ErrFrictionSubjectsChanged = errors.New("friction subjects changed during the build")

// FrictionSubject is one reviewable subject for a digest date.
type FrictionSubject struct {
	SubjectID, SubjectKind, Machine, FilePath, Agent string
	IsSubAgent                                       bool
	AlreadyDigested                                  bool
	LastActivity                                     time.Time
	RulesVersion                                     string
	// SessionVersion is the PostgreSQL row version captured before reading
	// findings and usage. SQLite builds use the caller's sync barrier instead.
	SessionVersion string
}

// FrictionDigest is one stored digest row.
type FrictionDigest struct {
	Date, Timezone, RulesVersion        string
	BuiltAt                             time.Time
	Revision, SessionsScanned           int
	SnapshotJSON, SummaryJSON, Markdown []byte
	MarkdownSHA256, RunID               string
}

// FrictionDigestSubject records that a subject was reviewed into a date.
type FrictionDigestSubject struct {
	SubjectID, Date, SubjectKind string
	SessionVersion               string
}

// FrictionPatternUpdate is one (fingerprint, subject) contribution to local
// recurrence counts for a digest date.
type FrictionPatternUpdate struct {
	Fingerprint, Kind, Title, Date, SubjectID string
	Ordinal                                   *int
	Occurrences                               int
}

// FrictionDayBounds returns the first instant of date and the following date.
// A skipped date has an empty interval; a repeated midnight uses its first occurrence.
func FrictionDayBounds(date string, loc *time.Location) (time.Time, time.Time, error) {
	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parsing friction date %q: %w", date, err)
	}
	return frictionDateStart(day, loc), frictionDateStart(day.AddDate(0, 0, 1), loc), nil
}

func frictionDateStart(day time.Time, loc *time.Location) time.Time {
	// Search offset intervals in time order so gaps and repeated midnights
	// resolve to the earliest valid instant. Two days cover all IANA UTC offsets.
	for cursor := day.Add(-48 * time.Hour).In(loc); ; {
		_, offset := cursor.Zone()
		_, end := cursor.ZoneBounds()
		candidate := day.Add(-time.Duration(offset) * time.Second).In(loc)
		if candidate.Before(cursor) {
			candidate = cursor
		}
		if end.IsZero() || candidate.Before(end) {
			return candidate
		}
		cursor = end
	}
}

const frictionPendingDatesSQL = `
  SELECT COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, ''))
  FROM sessions s
  LEFT JOIN friction_digest_sessions ds ON ds.subject_id = s.id
  WHERE s.deleted_at IS NULL AND ds.subject_id IS NULL
   AND s.friction_rules_version <> ?
   AND COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, '')) >= ?
   AND COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, '')) < ?`

// FrictionPendingDates finds unrecorded sessions across the backfill window,
// including dates whose digest already exists.
func (db *DB) FrictionPendingDates(ctx context.Context, first, last string, loc *time.Location) ([]string, error) {
	from, _, err := FrictionDayBounds(first, loc)
	if err != nil {
		return nil, err
	}
	_, to, err := FrictionDayBounds(last, loc)
	if err != nil {
		return nil, err
	}
	release, err := db.acquireBulkRead(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := db.getReader().QueryContext(ctx, frictionPendingDatesSQL,
		friction.SkippedRulesVersion,
		from.Add(-24*time.Hour).UTC().Format(time.RFC3339),
		to.Add(24*time.Hour).UTC().Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("listing pending friction dates: %w", err)
	}
	defer rows.Close()
	dates := map[string]bool{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		ts, err := parseTimestamp(raw)
		if err != nil {
			continue
		}
		date := ts.In(loc).Format("2006-01-02")
		if date >= first && date <= last {
			dates[date] = true
		}
	}
	return slices.Sorted(maps.Keys(dates)), rows.Err()
}

func frictionMarkdownSHA(md []byte) string {
	sum := sha256.Sum256(md)
	return hex.EncodeToString(sum[:])
}

const frictionSubjectsSQL = `
 WITH candidates AS (
  SELECT s.id, '' AS digest_date
  FROM sessions s
  WHERE s.deleted_at IS NULL
   AND COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, '')) >= ?2
   AND COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, '')) < ?3
   AND NOT EXISTS (SELECT 1 FROM friction_digest_sessions ds WHERE ds.subject_id = s.id)
  UNION ALL
  SELECT subject_id, date FROM friction_digest_sessions WHERE ?4 = 1 AND date = ?5
 )
 SELECT s.id, s.machine, COALESCE(s.file_path, ''), s.agent,
  CASE WHEN s.parent_session_id IS NOT NULL AND s.relationship_type = 'subagent' THEN 1 ELSE 0 END,
  COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, ''), ''), s.friction_rules_version,
  c.digest_date
 FROM candidates c JOIN sessions s ON s.id = c.id
 WHERE s.deleted_at IS NULL AND s.friction_rules_version <> ?1`

// FrictionSubjectsForDate returns sessions whose last activity falls on the
// local date and that are not yet in any digest; with includeDigested it
// also returns the subjects already recorded for date (rebuild keeps
// membership, spec §8.5). Trashed sessions are never subjects.
func (db *DB) FrictionSubjectsForDate(
	ctx context.Context, date string, loc *time.Location, includeDigested bool,
) ([]FrictionSubject, error) {
	from, to, err := FrictionDayBounds(date, loc)
	if err != nil {
		return nil, err
	}
	labels, err := db.GetMachineLabels(ctx)
	if err != nil {
		return nil, err
	}
	// Stored RFC3339 timestamps can retain their source offset. Widen the
	// text range by a day, then compare instants after parsing each row.
	lo := from.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	hi := to.Add(24 * time.Hour).UTC().Format(time.RFC3339)
	rows, err := db.getReader().QueryContext(ctx, frictionSubjectsSQL,
		friction.SkippedRulesVersion, lo, hi, boolInt(includeDigested), date,
	)
	if err != nil {
		return nil, fmt.Errorf("listing friction subjects: %w", err)
	}
	defer rows.Close()
	out := []FrictionSubject{}
	for rows.Next() {
		var (
			s             FrictionSubject
			machine, last string
			sub           int
			digestDate    string
		)
		if err := rows.Scan(&s.SubjectID, &machine, &s.FilePath, &s.Agent, &sub,
			&last, &s.RulesVersion, &digestDate); err != nil {
			return nil, fmt.Errorf("scanning friction subject: %w", err)
		}
		if last != "" {
			ts, err := parseTimestamp(last)
			if err != nil {
				log.Printf("friction: session %s has unparseable activity time %q", s.SubjectID, last)
				continue
			}
			s.LastActivity = ts.UTC()
		}
		if digestDate == "" && (s.LastActivity.IsZero() ||
			s.LastActivity.Before(from) || !s.LastActivity.Before(to)) {
			continue
		}
		s.SubjectKind = friction.SubjectSession
		s.IsSubAgent = sub != 0
		s.AlreadyDigested = digestDate != ""
		s.Machine = machine
		if label := labels[machine]; label != "" {
			s.Machine = label
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// FrictionFindingsForSubjects returns stored findings for the subjects,
// ordered by (session_id, seq). Callers impose subject order.
func (db *DB) FrictionFindingsForSubjects(
	ctx context.Context, subjectIDs []string,
) ([]FrictionFinding, error) {
	out := []FrictionFinding{}
	err := queryChunked(subjectIDs, func(chunk []string) error {
		ph, args := InPlaceholders(chunk)
		rows, err := db.getReader().QueryContext(ctx, `
			SELECT session_id, kind, detector, message_ordinal, call_index,
				tool_name, label, text, evidence, title, fingerprint,
				occurred_at, seq, rules_version
			FROM friction_findings
			WHERE session_id IN `+ph+`
			ORDER BY session_id, seq, id`, args...)
		if err != nil {
			return fmt.Errorf("loading friction findings: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				f         FrictionFinding
				ord, call sql.NullInt64
				occurred  sql.NullString
			)
			if err := rows.Scan(&f.SessionID, &f.Kind, &f.Detector, &ord, &call,
				&f.ToolName, &f.Label, &f.Text, &f.Evidence, &f.Title, &f.Fingerprint,
				&occurred, &f.Seq, &f.RulesVersion); err != nil {
				return fmt.Errorf("scanning friction finding: %w", err)
			}
			if ord.Valid {
				f.MessageOrdinal = new(int(ord.Int64))
			}
			if call.Valid {
				f.CallIndex = new(int(call.Int64))
			}
			if occurred.Valid && occurred.String != "" {
				ts, err := parseTimestamp(occurred.String)
				if err == nil {
					f.OccurredAt = new(ts.UTC())
				}
			}
			out = append(out, f)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SaveFrictionDigest writes a digest, its membership and pattern counts in
// one transaction (spec §8.3 step 9).
func (db *DB) SaveFrictionDigest(
	ctx context.Context, d FrictionDigest,
	subjects []FrictionDigestSubject, patterns []FrictionPatternUpdate,
) error {
	if err := db.requireDerivedTextStorage("friction digests"); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning friction digest tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	builtAt := d.BuiltAt.UTC().Format(time.RFC3339Nano)
	var res sql.Result
	if d.Revision <= 1 {
		res, err = tx.ExecContext(ctx, `
			INSERT INTO friction_digests (date, timezone, rules_version, built_at,
				revision, sessions_scanned, snapshot_json, summary_json, markdown,
				markdown_sha256, run_id)
			VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(date) DO NOTHING`,
			d.Date, d.Timezone, d.RulesVersion, builtAt, d.SessionsScanned,
			string(d.SnapshotJSON), string(d.SummaryJSON), string(d.Markdown),
			d.MarkdownSHA256, d.RunID)
	} else {
		res, err = tx.ExecContext(ctx, `
			UPDATE friction_digests SET timezone = ?, rules_version = ?,
				built_at = ?, revision = ?, sessions_scanned = ?,
				snapshot_json = ?, summary_json = ?, markdown = ?,
				markdown_sha256 = ?, run_id = ?
			WHERE date = ? AND revision = ?`,
			d.Timezone, d.RulesVersion, builtAt, d.Revision, d.SessionsScanned,
			string(d.SnapshotJSON), string(d.SummaryJSON), string(d.Markdown),
			d.MarkdownSHA256, d.RunID, d.Date, d.Revision-1)
	}
	if err != nil {
		return fmt.Errorf("writing friction digest %s: %w", d.Date, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("writing friction digest %s: %w", d.Date, err)
	} else if n == 0 {
		return ErrFrictionDigestConflict
	}
	for _, s := range subjects {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO friction_digest_sessions (subject_id, date, subject_kind)
			VALUES (?, ?, ?) ON CONFLICT(subject_id) DO NOTHING`,
			s.SubjectID, s.Date, s.SubjectKind)
		if err != nil {
			return fmt.Errorf("recording friction subject %s: %w", s.SubjectID, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("recording friction subject %s: %w", s.SubjectID, err)
		} else if n == 0 {
			var date string
			if err := tx.QueryRowContext(ctx, `SELECT date FROM friction_digest_sessions WHERE subject_id = ?`, s.SubjectID).Scan(&date); err != nil {
				return fmt.Errorf("checking friction membership: %w", err)
			}
			if date != d.Date {
				return ErrFrictionMembershipConflict
			}
		}
	}
	for _, p := range patterns {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO friction_patterns (fingerprint, kind, title,
				first_seen_date, last_seen_date, occurrence_count, session_count,
				last_subject_id, last_ordinal)
			VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
			ON CONFLICT(fingerprint) DO UPDATE SET
				kind = excluded.kind,
				title = excluded.title,
				occurrence_count = friction_patterns.occurrence_count + excluded.occurrence_count,
				session_count = friction_patterns.session_count + 1,
				first_seen_date = MIN(friction_patterns.first_seen_date, excluded.first_seen_date),
				last_subject_id = CASE WHEN excluded.last_seen_date >= friction_patterns.last_seen_date
					THEN excluded.last_subject_id ELSE friction_patterns.last_subject_id END,
				last_ordinal = CASE WHEN excluded.last_seen_date >= friction_patterns.last_seen_date
					THEN excluded.last_ordinal ELSE friction_patterns.last_ordinal END,
				last_seen_date = MAX(friction_patterns.last_seen_date, excluded.last_seen_date)`,
			p.Fingerprint, p.Kind, p.Title, p.Date, p.Date, p.Occurrences,
			p.SubjectID, p.Ordinal); err != nil {
			return fmt.Errorf("updating friction pattern %s: %w", p.Fingerprint, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing friction digest %s: %w", d.Date, err)
	}
	return nil
}

// GetFrictionDigest returns the digest for date, or nil when none exists.
func (db *DB) GetFrictionDigest(ctx context.Context, date string) (*FrictionDigest, error) {
	var (
		d                     FrictionDigest
		builtAt               string
		snapshot, summary, md string
	)
	err := db.getReader().QueryRowContext(ctx, `
		SELECT date, timezone, rules_version, built_at, revision,
			sessions_scanned, snapshot_json, summary_json, markdown,
			markdown_sha256, run_id
		FROM friction_digests WHERE date = ?`, date,
	).Scan(&d.Date, &d.Timezone, &d.RulesVersion, &builtAt, &d.Revision,
		&d.SessionsScanned, &snapshot, &summary, &md, &d.MarkdownSHA256, &d.RunID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading friction digest %s: %w", date, err)
	}
	ts, err := parseTimestamp(builtAt)
	if err != nil {
		return nil, fmt.Errorf("parsing friction digest built_at %q: %w", builtAt, err)
	}
	d.BuiltAt = ts.UTC()
	d.SnapshotJSON, d.SummaryJSON, d.Markdown = []byte(snapshot), []byte(summary), []byte(md)
	return &d, nil
}

// LatestFrictionDigestDate returns the newest digest date, or "".
func (db *DB) LatestFrictionDigestDate(ctx context.Context) (string, error) {
	var date string
	if err := db.getReader().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(date), '') FROM friction_digests`,
	).Scan(&date); err != nil {
		return "", fmt.Errorf("reading latest friction digest date: %w", err)
	}
	return date, nil
}

// EarliestSessionDate returns the local date of the earliest session
// activity (nonempty ended_at, else started_at). Unparseable times are ignored.
func (db *DB) EarliestSessionDate(ctx context.Context, loc *time.Location) (string, error) {
	release, err := db.acquireBulkRead(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, ''))
		FROM sessions
		WHERE deleted_at IS NULL`)
	if err != nil {
		return "", fmt.Errorf("reading earliest session date: %w", err)
	}
	defer rows.Close()
	// Parse before comparing: source offsets defeat text ordering, and SQLite's
	// date functions accept some dates that the archive's RFC3339 parser rejects.
	var earliest time.Time
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return "", fmt.Errorf("reading session activity: %w", err)
		}
		if !raw.Valid {
			continue
		}
		ts, err := parseTimestamp(raw.String)
		if err == nil && (earliest.IsZero() || ts.Before(earliest)) {
			earliest = ts
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("reading session activity: %w", err)
	}
	if earliest.IsZero() {
		return "", nil
	}
	return earliest.In(loc).Format("2006-01-02"), nil
}

// UpdateFrictionDigestRender replaces the rendered forms of a digest and
// bumps its revision (spec §8.6). revision is the new revision.
func (db *DB) UpdateFrictionDigestRender(
	ctx context.Context, date string, markdown, summaryJSON []byte, revision int,
) error {
	if err := db.requireDerivedTextStorage("friction digests"); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	res, err := db.getWriter().Exec(ctx, `
		UPDATE friction_digests
		SET markdown = ?, summary_json = ?, markdown_sha256 = ?, revision = ?
		WHERE date = ? AND revision = ?`,
		string(markdown), string(summaryJSON), frictionMarkdownSHA(markdown),
		revision, date, revision-1)
	if err != nil {
		return fmt.Errorf("re-rendering friction digest %s: %w", date, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("re-rendering friction digest %s: %w", date, err)
	} else if n == 0 {
		return ErrFrictionDigestConflict
	}
	return nil
}

// CopyFrictionStateFrom carries friction review state (digests, membership
// and recurrence) across a full resync, like CopyInsightsFrom. Tables absent
// from an older source archive are skipped.
func (db *DB) CopyFrictionStateFrom(sourcePath string) error {
	if err := db.requireDerivedTextStorage("friction review state"); err != nil {
		return err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	ctx := context.Background()
	conn, err := db.getWriter().Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "ATTACH DATABASE ? AS old_db", sourcePath); err != nil {
		return fmt.Errorf("attaching source db: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "DETACH DATABASE old_db") }()

	copies := []struct{ table, columns string }{
		{"friction_digests", "date, timezone, rules_version, built_at, revision, sessions_scanned, snapshot_json, summary_json, markdown, markdown_sha256, run_id"},
		{"friction_digest_sessions", "subject_id, date, subject_kind"},
		{"friction_patterns", "fingerprint, kind, title, first_seen_date, last_seen_date, occurrence_count, session_count, last_subject_id, last_ordinal"},
	}
	for _, c := range copies {
		var present int
		if err := conn.QueryRowContext(ctx,
			`SELECT count(*) FROM old_db.sqlite_master WHERE type = 'table' AND name = ?`,
			c.table).Scan(&present); err != nil {
			return fmt.Errorf("probing source %s: %w", c.table, err)
		}
		if present == 0 {
			continue
		}
		if _, err := conn.ExecContext(ctx,
			"INSERT OR IGNORE INTO "+c.table+" ("+c.columns+") SELECT "+c.columns+" FROM old_db."+c.table,
		); err != nil {
			return fmt.Errorf("copying %s: %w", c.table, err)
		}
	}
	return nil
}

// FrictionDigestDates lists stored dates without loading their snapshots.
func (db *DB) FrictionDigestDates(ctx context.Context, first, last string) ([]string, error) {
	rows, err := db.getReader().QueryContext(ctx, `SELECT date FROM friction_digests WHERE date >= ? AND date <= ? ORDER BY date`, first, last)
	if err != nil {
		return nil, fmt.Errorf("listing friction digest dates: %w", err)
	}
	defer rows.Close()
	var dates []string
	for rows.Next() {
		var date string
		if err := rows.Scan(&date); err != nil {
			return nil, err
		}
		dates = append(dates, date)
	}
	return dates, rows.Err()
}
