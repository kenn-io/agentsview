package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const pgFrictionLinkCols = `fingerprint, state, kata_instance_uid, kata_project_uid,
	issue_uid, qualified_id, web_url, link_source, attempts, first_failed_at,
	next_attempt_at, last_error_code, last_error, candidates_json,
	last_recurrence_date, updated_at`

func scanPGFrictionLink(r pgRowScanner) (db.FrictionIssueLink, error) {
	var l db.FrictionIssueLink
	var first, next sql.NullTime
	if err := r.Scan(&l.Fingerprint, &l.State, &l.KataInstanceUID, &l.KataProjectUID,
		&l.IssueUID, &l.QualifiedID, &l.WebURL, &l.LinkSource, &l.Attempts, &first,
		&next, &l.LastErrorCode, &l.LastError, &l.CandidatesJSON,
		&l.LastRecurrenceDate, &l.UpdatedAt); err != nil {
		return l, err
	}
	if first.Valid {
		t := first.Time.UTC()
		l.FirstFailedAt = &t
	}
	if next.Valid {
		t := next.Time.UTC()
		l.NextAttemptAt = &t
	}
	l.UpdatedAt = l.UpdatedAt.UTC()
	return l, nil
}

func pgTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Truncate(time.Second)
}

func (s *Store) GetFrictionIssueLinks(ctx context.Context, fingerprints []string) (map[string]db.FrictionIssueLink, error) {
	out := make(map[string]db.FrictionIssueLink, len(fingerprints))
	if len(fingerprints) == 0 {
		return out, nil
	}
	rows, err := s.pg.QueryContext(ctx,
		"SELECT "+pgFrictionLinkCols+" FROM friction_issue_links WHERE fingerprint = ANY($1)", fingerprints)
	if err != nil {
		return nil, fmt.Errorf("querying friction issue links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		l, err := scanPGFrictionLink(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning friction issue link: %w", err)
		}
		out[l.Fingerprint] = l
	}
	return out, rows.Err()
}

func (s *Store) UpsertFrictionIssueLink(ctx context.Context, l db.FrictionIssueLink) error {
	if !db.ValidFrictionLinkState(l.State) {
		return fmt.Errorf("unknown friction link state %q", l.State)
	}
	if l.UpdatedAt.IsZero() {
		l.UpdatedAt = time.Now()
	}
	_, err := s.pg.ExecContext(ctx, `
		INSERT INTO friction_issue_links (`+pgFrictionLinkCols+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (fingerprint) DO UPDATE SET
			state = EXCLUDED.state,
			kata_instance_uid = EXCLUDED.kata_instance_uid,
			kata_project_uid = EXCLUDED.kata_project_uid,
			issue_uid = EXCLUDED.issue_uid,
			qualified_id = EXCLUDED.qualified_id,
			web_url = EXCLUDED.web_url,
			link_source = EXCLUDED.link_source,
			attempts = EXCLUDED.attempts,
			first_failed_at = EXCLUDED.first_failed_at,
			next_attempt_at = EXCLUDED.next_attempt_at,
			last_error_code = EXCLUDED.last_error_code,
			last_error = EXCLUDED.last_error,
			candidates_json = EXCLUDED.candidates_json,
			last_recurrence_date = EXCLUDED.last_recurrence_date,
			updated_at = EXCLUDED.updated_at`,
		l.Fingerprint, l.State, l.KataInstanceUID, l.KataProjectUID,
		l.IssueUID, l.QualifiedID, l.WebURL, l.LinkSource, l.Attempts, pgTimePtr(l.FirstFailedAt),
		pgTimePtr(l.NextAttemptAt), l.LastErrorCode, l.LastError, l.CandidatesJSON,
		l.LastRecurrenceDate, l.UpdatedAt.UTC().Truncate(time.Second))
	if err != nil {
		return mapPGWriteError("upserting friction issue link", err)
	}
	return nil
}

func (s *Store) DeleteFrictionIssueLink(ctx context.Context, fingerprint string) error {
	if _, err := s.pg.ExecContext(ctx, `DELETE FROM friction_issue_links WHERE fingerprint = $1`, fingerprint); err != nil {
		return mapPGWriteError("deleting friction issue link", err)
	}
	return nil
}

func (s *Store) DueFrictionFilings(ctx context.Context, now time.Time, limit int) ([]db.FrictionIssueLink, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pg.QueryContext(ctx, `
		SELECT `+pgFrictionLinkCols+` FROM friction_issue_links
		WHERE state IN ('pending', 'failed')
		  AND (next_attempt_at IS NULL OR next_attempt_at <= $1)
		ORDER BY next_attempt_at IS NOT NULL, next_attempt_at, fingerprint
		LIMIT $2`, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("querying due friction filings: %w", err)
	}
	defer rows.Close()
	var out []db.FrictionIssueLink
	for rows.Next() {
		l, err := scanPGFrictionLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) DigestDatesForFingerprints(ctx context.Context, fingerprints []string) ([]string, error) {
	if len(fingerprints) == 0 {
		return nil, nil
	}
	rows, err := s.pg.QueryContext(ctx, `
		SELECT DISTINCT ds.date
		FROM friction_digest_sessions ds
		JOIN friction_findings f ON f.session_id = ds.subject_id
		WHERE f.fingerprint = ANY($1)
		UNION
		SELECT first_seen_date FROM friction_patterns
		WHERE fingerprint = ANY($1)
		ORDER BY 1`, fingerprints)
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
