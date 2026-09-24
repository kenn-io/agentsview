package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
)

// FrictionDigestedSubjects returns the digest date of each known subject ID.
func (s *Store) FrictionDigestedSubjects(ctx context.Context, subjectIDs []string) (map[string]string, error) {
	out := map[string]string{}
	for start := 0; start < len(subjectIDs); start += 500 {
		chunk := subjectIDs[start:min(start+500, len(subjectIDs))]
		if err := s.frictionDigestedSubjectsChunk(ctx, chunk, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) frictionDigestedSubjectsChunk(ctx context.Context, chunk []string, out map[string]string) error {
	pb := &paramBuilder{}
	in := pgInPlaceholders(chunk, pb)
	rows, err := s.pg.QueryContext(ctx,
		`SELECT subject_id, date FROM friction_digest_sessions WHERE subject_id IN `+in, pb.args...)
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
}

// FrictionPatternsByFingerprint returns stored state before a digest updates
// recurrence counts.
func (s *Store) FrictionPatternsByFingerprint(ctx context.Context, fingerprints []string) (map[string]db.FrictionPattern, error) {
	out := map[string]db.FrictionPattern{}
	for start := 0; start < len(fingerprints); start += 500 {
		chunk := fingerprints[start:min(start+500, len(fingerprints))]
		if err := s.frictionPatternsChunk(ctx, chunk, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) frictionPatternsChunk(ctx context.Context, chunk []string, out map[string]db.FrictionPattern) error {
	pb := &paramBuilder{}
	in := pgInPlaceholders(chunk, pb)
	rows, err := s.pg.QueryContext(ctx, `
		SELECT fingerprint, kind, title, first_seen_date, last_seen_date,
			occurrence_count, session_count, last_subject_id, last_ordinal
		FROM friction_patterns WHERE fingerprint IN `+in, pb.args...)
	if err != nil {
		return fmt.Errorf("querying friction patterns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p db.FrictionPattern
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
}

// FrictionAvailable reports whether startup proved this role can write the
// friction review tables. pg serve runs the friction-review job only then.
func (s *Store) FrictionAvailable() bool { return s.frictionAvailable.Load() }

// DetectFrictionAvailability probes the review tables inside a rolled-back
// transaction. A missing table or a read-only role disables the job; any
// other failure is logged and also disables it, so startup never fails on
// an optional feature (spec §20).
func (s *Store) DetectFrictionAvailability(ctx context.Context) {
	for _, table := range []string{"friction_digests", "friction_digest_sessions", "friction_patterns"} {
		if !pgHasTable(ctx, s.pg, table) {
			s.frictionAvailable.Store(false)
			return
		}
	}
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("friction: capability probe: %v", err)
		s.frictionAvailable.Store(false)
		return
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO friction_digests (date, timezone, rules_version, built_at,
			sessions_scanned, snapshot_json, summary_json, markdown,
			markdown_sha256, run_id)
		VALUES ('0000-00-00', 'UTC', '', NOW(), 0, '', '', '', '', '')
		ON CONFLICT (date) DO NOTHING`)
	if err == nil {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO friction_digest_sessions (subject_id, date, subject_kind)
			VALUES ('friction-capability-probe', '0000-00-00', 'diagnostic')
			ON CONFLICT (subject_id) DO NOTHING`)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO friction_patterns (fingerprint, kind, title,
				first_seen_date, last_seen_date, occurrence_count, session_count,
				last_subject_id)
			VALUES ('friction-capability-probe', 'pattern', '',
				'0000-00-00', '0000-00-00', 0, 0, 'friction-capability-probe')
			ON CONFLICT (fingerprint) DO NOTHING`)
	}
	if err == nil {
		for _, query := range []string{
			"UPDATE friction_digests SET revision = revision WHERE date = '0000-00-00'",
			"UPDATE friction_patterns SET title = title WHERE fingerprint = 'friction-capability-probe'",
		} {
			if _, err = tx.ExecContext(ctx, query); err != nil {
				break
			}
		}
	}
	if err != nil {
		if !IsReadOnlyError(err) {
			log.Printf("friction: capability probe: %v", err)
		}
		s.frictionAvailable.Store(false)
		return
	}
	s.frictionAvailable.Store(true)
}

func (s *Store) FrictionSubjectsForDate(
	ctx context.Context, date string, loc *time.Location, includeDigested bool,
) ([]db.FrictionSubject, error) {
	from, to, err := db.FrictionDayBounds(date, loc)
	if err != nil {
		return nil, err
	}
	labels, err := s.GetMachineLabels(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pg.QueryContext(ctx, `
		SELECT s.id, s.machine, COALESCE(s.file_path, ''), s.agent,
			(s.parent_session_id IS NOT NULL AND s.relationship_type = 'subagent'),
			COALESCE(s.ended_at, s.started_at), s.friction_rules_version,
			COALESCE(d.seat, ''), COALESCE(d.persona, ''), COALESCE(d.channel, ''),
			COALESCE(d.dims_source, ''), COALESCE(d.review_excluded, FALSE),
			COALESCE(ds.date, '')
		FROM sessions s
		LEFT JOIN friction_session_dims d ON d.session_id = s.id
		LEFT JOIN friction_digest_sessions ds ON ds.subject_id = s.id
		WHERE s.deleted_at IS NULL
		  AND COALESCE(d.review_excluded, FALSE) = FALSE
		  AND ((ds.subject_id IS NULL
		        AND COALESCE(s.ended_at, s.started_at) >= $1
		        AND COALESCE(s.ended_at, s.started_at) < $2)
		    OR ($3 AND ds.date = $4))`,
		from.UTC(), to.UTC(), includeDigested, date)
	if err != nil {
		return nil, fmt.Errorf("listing friction subjects: %w", err)
	}
	defer rows.Close()
	out := []db.FrictionSubject{}
	for rows.Next() {
		var (
			sub        db.FrictionSubject
			machine    string
			last       sql.NullTime
			digestDate string
		)
		if err := rows.Scan(&sub.SubjectID, &machine, &sub.FilePath, &sub.Agent,
			&sub.IsSubAgent, &last, &sub.RulesVersion, &sub.Dims.Seat,
			&sub.Dims.Persona, &sub.Dims.Channel, &sub.Dims.DimsSource,
			&sub.Dims.ReviewExcluded, &digestDate); err != nil {
			return nil, fmt.Errorf("scanning friction subject: %w", err)
		}
		if last.Valid {
			sub.LastActivity = last.Time.UTC()
		}
		sub.SubjectKind = friction.SubjectSession
		sub.AlreadyDigested = digestDate != ""
		sub.Dims.SessionID = sub.SubjectID
		sub.Machine = machine
		if label := labels[machine]; label != "" {
			sub.Machine = label
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (s *Store) FrictionFindingsForSubjects(
	ctx context.Context, subjectIDs []string,
) ([]db.FrictionFinding, error) {
	out := []db.FrictionFinding{}
	for start := 0; start < len(subjectIDs); start += 500 {
		chunk := subjectIDs[start:min(start+500, len(subjectIDs))]
		findings, err := s.frictionFindingsChunk(ctx, chunk)
		if err != nil {
			return nil, err
		}
		out = append(out, findings...)
	}
	// PG has no rowid tiebreak; seq is unique within a session's findings.
	// Sort in Go with byte order so collation never differs from SQLite.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SessionID != out[j].SessionID {
			return out[i].SessionID < out[j].SessionID
		}
		return out[i].Seq < out[j].Seq
	})
	return out, nil
}

func (s *Store) frictionFindingsChunk(
	ctx context.Context, subjectIDs []string,
) ([]db.FrictionFinding, error) {
	pb := &paramBuilder{}
	in := pgInPlaceholders(subjectIDs, pb)
	rows, err := s.pg.QueryContext(ctx, `
		SELECT session_id, kind, detector, message_ordinal, call_index,
			tool_name, label, text, evidence, title, fingerprint,
			occurred_at, seq, rules_version
		FROM friction_findings
		WHERE session_id IN `+in, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("loading friction findings: %w", err)
	}
	defer rows.Close()
	out := []db.FrictionFinding{}
	for rows.Next() {
		var (
			f         db.FrictionFinding
			ord, call sql.NullInt64
			occurred  sql.NullTime
		)
		if err := rows.Scan(&f.SessionID, &f.Kind, &f.Detector, &ord, &call,
			&f.ToolName, &f.Label, &f.Text, &f.Evidence, &f.Title, &f.Fingerprint,
			&occurred, &f.Seq, &f.RulesVersion); err != nil {
			return nil, fmt.Errorf("scanning friction finding: %w", err)
		}
		if ord.Valid {
			f.MessageOrdinal = new(int(ord.Int64))
		}
		if call.Valid {
			f.CallIndex = new(int(call.Int64))
		}
		if occurred.Valid {
			f.OccurredAt = new(occurred.Time.UTC())
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) SaveFrictionDigest(
	ctx context.Context, d db.FrictionDigest,
	subjects []db.FrictionDigestSubject, patterns []db.FrictionPatternUpdate,
) error {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning friction digest tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var res sql.Result
	if d.Revision <= 1 {
		res, err = tx.ExecContext(ctx, `
			INSERT INTO friction_digests (date, timezone, rules_version, built_at,
				revision, sessions_scanned, snapshot_json, summary_json, markdown,
				markdown_sha256, run_id)
			VALUES ($1, $2, $3, $4, 1, $5, $6, $7, $8, $9, $10)
			ON CONFLICT (date) DO NOTHING`,
			d.Date, d.Timezone, d.RulesVersion, d.BuiltAt.UTC(), d.SessionsScanned,
			string(d.SnapshotJSON), string(d.SummaryJSON), string(d.Markdown),
			d.MarkdownSHA256, d.RunID)
	} else {
		res, err = tx.ExecContext(ctx, `
			UPDATE friction_digests SET timezone = $1, rules_version = $2,
				built_at = $3, revision = $4, sessions_scanned = $5,
				snapshot_json = $6, summary_json = $7, markdown = $8,
				markdown_sha256 = $9, run_id = $10
			WHERE date = $11 AND revision = $12`,
			d.Timezone, d.RulesVersion, d.BuiltAt.UTC(), d.Revision, d.SessionsScanned,
			string(d.SnapshotJSON), string(d.SummaryJSON), string(d.Markdown),
			d.MarkdownSHA256, d.RunID, d.Date, d.Revision-1)
	}
	if err != nil {
		return fmt.Errorf("writing friction digest %s: %w", d.Date, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("writing friction digest %s: %w", d.Date, err)
	} else if n == 0 {
		return db.ErrFrictionDigestConflict
	}
	for _, sub := range subjects {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO friction_digest_sessions (subject_id, date, subject_kind)
			VALUES ($1, $2, $3) ON CONFLICT (subject_id) DO NOTHING`,
			sub.SubjectID, sub.Date, sub.SubjectKind); err != nil {
			return fmt.Errorf("recording friction subject %s: %w", sub.SubjectID, err)
		}
	}
	for _, p := range patterns {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO friction_patterns (fingerprint, kind, title,
				first_seen_date, last_seen_date, occurrence_count, session_count,
				last_subject_id, last_ordinal)
			VALUES ($1, $2, $3, $4, $4, $5, 1, $6, $7)
			ON CONFLICT (fingerprint) DO UPDATE SET
				kind = EXCLUDED.kind,
				title = EXCLUDED.title,
				occurrence_count = friction_patterns.occurrence_count + EXCLUDED.occurrence_count,
				session_count = friction_patterns.session_count + 1,
				first_seen_date = LEAST(friction_patterns.first_seen_date, EXCLUDED.first_seen_date),
				last_subject_id = CASE WHEN EXCLUDED.last_seen_date >= friction_patterns.last_seen_date
					THEN EXCLUDED.last_subject_id ELSE friction_patterns.last_subject_id END,
				last_ordinal = CASE WHEN EXCLUDED.last_seen_date >= friction_patterns.last_seen_date
					THEN EXCLUDED.last_ordinal ELSE friction_patterns.last_ordinal END,
				last_seen_date = GREATEST(friction_patterns.last_seen_date, EXCLUDED.last_seen_date)`,
			p.Fingerprint, p.Kind, p.Title, p.Date, p.Occurrences, p.SubjectID, p.Ordinal); err != nil {
			return fmt.Errorf("updating friction pattern %s: %w", p.Fingerprint, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing friction digest %s: %w", d.Date, err)
	}
	return nil
}

func (s *Store) GetFrictionDigest(ctx context.Context, date string) (*db.FrictionDigest, error) {
	var (
		d                     db.FrictionDigest
		snapshot, summary, md string
	)
	err := s.pg.QueryRowContext(ctx, `
		SELECT date, timezone, rules_version, built_at, revision,
			sessions_scanned, snapshot_json, summary_json, markdown,
			markdown_sha256, run_id
		FROM friction_digests WHERE date = $1`, date,
	).Scan(&d.Date, &d.Timezone, &d.RulesVersion, &d.BuiltAt, &d.Revision,
		&d.SessionsScanned, &snapshot, &summary, &md, &d.MarkdownSHA256, &d.RunID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading friction digest %s: %w", date, err)
	}
	d.BuiltAt = d.BuiltAt.UTC()
	d.SnapshotJSON, d.SummaryJSON, d.Markdown = []byte(snapshot), []byte(summary), []byte(md)
	return &d, nil
}

func (s *Store) LatestFrictionDigestDate(ctx context.Context) (string, error) {
	var date string
	if err := s.pg.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(date), '') FROM friction_digests`,
	).Scan(&date); err != nil {
		return "", fmt.Errorf("reading latest friction digest date: %w", err)
	}
	return date, nil
}

func (s *Store) EarliestSessionDate(ctx context.Context, loc *time.Location) (string, error) {
	var earliest sql.NullTime
	if err := s.pg.QueryRowContext(ctx, `
		SELECT MIN(COALESCE(ended_at, started_at)) FROM sessions
		WHERE deleted_at IS NULL`,
	).Scan(&earliest); err != nil {
		return "", fmt.Errorf("reading earliest session date: %w", err)
	}
	if !earliest.Valid {
		return "", nil
	}
	return earliest.Time.In(loc).Format("2006-01-02"), nil
}

func (s *Store) UpdateFrictionDigestRender(
	ctx context.Context, date string, markdown, summaryJSON []byte, revision int,
) error {
	sum := sha256.Sum256(markdown)
	res, err := s.pg.ExecContext(ctx, `
		UPDATE friction_digests
		SET markdown = $1, summary_json = $2, markdown_sha256 = $3, revision = $4
		WHERE date = $5 AND revision = $6`,
		string(markdown), string(summaryJSON), hex.EncodeToString(sum[:]),
		revision, date, revision-1)
	if err != nil {
		return fmt.Errorf("re-rendering friction digest %s: %w", date, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("re-rendering friction digest %s: %w", date, err)
	} else if n == 0 {
		return db.ErrFrictionDigestConflict
	}
	return nil
}

func (s *Store) FrictionUsageForSessions(
	ctx context.Context, sessionIDs []string,
) (map[string]friction.SessionUsage, error) {
	return db.FrictionUsageForSessionsFrom(ctx, s, sessionIDs)
}

func (s *Store) FrictionArchiveSpend(
	ctx context.Context, from, to string, loc *time.Location,
) (*friction.ArchiveSpend, error) {
	return db.FrictionArchiveSpendFrom(ctx, s, from, to, loc)
}
