package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"go.kenn.io/agentsview/internal/friction"
)

// ErrFrictionDigestConflict reports that another builder wrote the digest
// row first (or the expected revision moved). The caller treats it as
// "already built", never as a failure to retry blindly.
var ErrFrictionDigestConflict = errors.New("friction digest was written concurrently")

// FrictionSubject is one reviewable subject for a digest date.
type FrictionSubject struct {
	SubjectID, SubjectKind, Machine, FilePath, Agent string
	IsSubAgent                                       bool
	AlreadyDigested                                  bool
	LastActivity                                     time.Time
	Dims                                             FrictionSessionDims
	RulesVersion                                     string
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
type FrictionDigestSubject struct{ SubjectID, Date, SubjectKind string }

// FrictionPatternUpdate is one (fingerprint, subject) contribution to local
// recurrence counts for a digest date.
type FrictionPatternUpdate struct {
	Fingerprint, Kind, Title, Date, SubjectID string
	Ordinal                                   *int
	Occurrences                               int
}

const frictionLookupChunk = 500

// FrictionDigestedSubjects returns the digest date of each known subject ID.
func (db *DB) FrictionDigestedSubjects(ctx context.Context, subjectIDs []string) (map[string]string, error) {
	out := map[string]string{}
	err := queryChunkedSize(subjectIDs, frictionLookupChunk, func(chunk []string) error {
		ph, args := inPlaceholders(chunk)
		rows, err := db.getReader().QueryContext(ctx,
			`SELECT subject_id, date FROM friction_digest_sessions WHERE subject_id IN `+ph, args...)
		if err != nil {
			return fmt.Errorf("querying digested friction subjects: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, date string
			if err := rows.Scan(&id, &date); err != nil {
				return fmt.Errorf("scanning digested friction subject: %w", err)
			}
			out[id] = date
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FrictionPatternsByFingerprint returns the stored state before a digest
// updates recurrence counts.
func (db *DB) FrictionPatternsByFingerprint(ctx context.Context, fingerprints []string) (map[string]FrictionPattern, error) {
	out := map[string]FrictionPattern{}
	err := queryChunkedSize(fingerprints, frictionLookupChunk, func(chunk []string) error {
		ph, args := inPlaceholders(chunk)
		rows, err := db.getReader().QueryContext(ctx, `
			SELECT fingerprint, kind, title, first_seen_date, last_seen_date,
				occurrence_count, session_count, last_subject_id, last_ordinal
			FROM friction_patterns WHERE fingerprint IN `+ph, args...)
		if err != nil {
			return fmt.Errorf("querying friction patterns: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var p FrictionPattern
			var ordinal sql.NullInt64
			if err := rows.Scan(&p.Fingerprint, &p.Kind, &p.Title, &p.FirstSeenDate,
				&p.LastSeenDate, &p.OccurrenceCount, &p.SessionCount, &p.LastSubjectID,
				&ordinal); err != nil {
				return fmt.Errorf("scanning friction pattern: %w", err)
			}
			if ordinal.Valid {
				p.LastOrdinal = new(int(ordinal.Int64))
			}
			out[p.Fingerprint] = p
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FrictionDayBounds returns [local midnight of date, next local midnight).
// AddDate keeps DST days at their real 23 or 25 hours.
func FrictionDayBounds(date string, loc *time.Location) (time.Time, time.Time, error) {
	day, err := time.ParseInLocation("2006-01-02", date, loc)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("parsing friction date %q: %w", date, err)
	}
	return day, day.AddDate(0, 0, 1), nil
}

func frictionMarkdownSHA(md []byte) string {
	sum := sha256.Sum256(md)
	return hex.EncodeToString(sum[:])
}

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
	rows, err := db.getReader().QueryContext(ctx, `
		SELECT s.id, s.machine, COALESCE(s.file_path, ''), s.agent,
			CASE WHEN s.parent_session_id IS NOT NULL
			      AND s.relationship_type = 'subagent' THEN 1 ELSE 0 END,
			COALESCE(s.ended_at, s.started_at, ''), s.friction_rules_version,
			COALESCE(d.seat, ''), COALESCE(d.persona, ''), COALESCE(d.channel, ''),
			COALESCE(d.dims_source, ''), COALESCE(d.review_excluded, 0),
			COALESCE(ds.date, '')
		FROM sessions s
		LEFT JOIN friction_session_dims d ON d.session_id = s.id
		LEFT JOIN friction_digest_sessions ds ON ds.subject_id = s.id
		WHERE s.deleted_at IS NULL
		  AND COALESCE(d.review_excluded, 0) = 0
		  AND ((ds.subject_id IS NULL
		        AND COALESCE(s.ended_at, s.started_at) >= ?
		        AND COALESCE(s.ended_at, s.started_at) < ?)
		    OR (? = 1 AND ds.date = ?))`,
		lo, hi, boolInt(includeDigested), date,
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
			sub, excluded int
			digestDate    string
		)
		if err := rows.Scan(&s.SubjectID, &machine, &s.FilePath, &s.Agent, &sub,
			&last, &s.RulesVersion, &s.Dims.Seat, &s.Dims.Persona, &s.Dims.Channel,
			&s.Dims.DimsSource, &excluded, &digestDate); err != nil {
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
		s.Dims.SessionID = s.SubjectID
		s.Dims.ReviewExcluded = excluded != 0
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
		ph, args := inPlaceholders(chunk)
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
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO friction_digest_sessions (subject_id, date, subject_kind)
			VALUES (?, ?, ?) ON CONFLICT(subject_id) DO NOTHING`,
			s.SubjectID, s.Date, s.SubjectKind); err != nil {
			return fmt.Errorf("recording friction subject %s: %w", s.SubjectID, err)
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
// activity (ended_at, else started_at), or "" for an empty archive.
func (db *DB) EarliestSessionDate(ctx context.Context, loc *time.Location) (string, error) {
	var raw string
	err := db.getReader().QueryRowContext(ctx, `
		SELECT COALESCE(COALESCE(ended_at, started_at), '')
		FROM sessions
		WHERE deleted_at IS NULL AND COALESCE(ended_at, started_at) IS NOT NULL
		ORDER BY julianday(COALESCE(ended_at, started_at)) ASC
		LIMIT 1`,
	).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading earliest session date: %w", err)
	}
	if raw == "" {
		return "", nil
	}
	ts, err := parseTimestamp(raw)
	if err != nil {
		return "", fmt.Errorf("parsing earliest session time %q: %w", raw, err)
	}
	return ts.In(loc).Format("2006-01-02"), nil
}

// UpdateFrictionDigestRender replaces the rendered forms of a digest and
// bumps its revision (spec §8.6). revision is the new revision.
func (db *DB) UpdateFrictionDigestRender(
	ctx context.Context, date string, markdown, summaryJSON []byte, revision int,
) error {
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
		{"friction_issue_links", frictionLinkCols},
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
