package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

func pgNullIntPtr(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	return new(int(v.Int64))
}

// ListFrictionFindings mirrors internal/db/friction_list.go.
func (s *Store) ListFrictionFindings(
	ctx context.Context, f db.FrictionFindingFilter,
) ([]db.FrictionFinding, string, error) {
	limit := db.NormalizeFrictionLimit(f.Limit)
	offset, err := db.DecodeFrictionCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	pb := &paramBuilder{}
	preds := []string{"s.deleted_at IS NULL"}
	if f.Date != "" {
		preds = append(preds, `ff.session_id IN (SELECT subject_id FROM friction_digest_sessions
			WHERE date = `+pb.add(f.Date)+` AND subject_kind = 'session')`)
	}
	if f.Kind != "" {
		preds = append(preds, "ff.kind = "+pb.add(f.Kind))
	}
	if f.SessionID != "" {
		preds = append(preds, "ff.session_id = "+pb.add(f.SessionID))
	}
	if f.Fingerprint != "" {
		preds = append(preds, "ff.fingerprint = "+pb.add(f.Fingerprint))
	}
	limitParam := pb.add(limit + 1)
	offsetParam := pb.add(offset)
	rows, err := s.pg.QueryContext(ctx, `
		SELECT ff.session_id, ff.kind, ff.detector, ff.message_ordinal,
			ff.call_index, ff.tool_name, ff.label, ff.text, ff.evidence,
			ff.title, ff.fingerprint, ff.occurred_at, ff.seq, ff.rules_version
		FROM friction_findings ff JOIN sessions s ON s.id = ff.session_id
		WHERE `+strings.Join(preds, " AND ")+`
		ORDER BY ff.session_id, ff.seq, ff.id
		LIMIT `+limitParam+` OFFSET `+offsetParam, pb.args...)
	if err != nil {
		return nil, "", fmt.Errorf("listing friction findings: %w", err)
	}
	defer rows.Close()
	out := make([]db.FrictionFinding, 0, limit+1)
	for rows.Next() {
		var (
			r                  db.FrictionFinding
			ordinal, callIndex sql.NullInt64
			occurred           sql.NullTime
		)
		if err := rows.Scan(&r.SessionID, &r.Kind, &r.Detector, &ordinal,
			&callIndex, &r.ToolName, &r.Label, &r.Text, &r.Evidence,
			&r.Title, &r.Fingerprint, &occurred, &r.Seq, &r.RulesVersion); err != nil {
			return nil, "", fmt.Errorf("scanning friction finding: %w", err)
		}
		r.MessageOrdinal = pgNullIntPtr(ordinal)
		r.CallIndex = pgNullIntPtr(callIndex)
		if occurred.Valid {
			r.OccurredAt = new(occurred.Time.UTC())
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("listing friction findings: %w", err)
	}
	if len(out) > limit {
		return out[:limit], db.EncodeFrictionCursor(offset + limit), nil
	}
	return out, "", nil
}

// ListFrictionDigests mirrors internal/db/friction_list.go.
func (s *Store) ListFrictionDigests(
	ctx context.Context, from, to string,
) ([]db.FrictionDigest, error) {
	pb := &paramBuilder{}
	var preds []string
	if from != "" {
		preds = append(preds, "date >= "+pb.add(from))
	}
	if to != "" {
		preds = append(preds, "date <= "+pb.add(to))
	}
	where := ""
	if len(preds) > 0 {
		where = "WHERE " + strings.Join(preds, " AND ")
	}
	rows, err := s.pg.QueryContext(ctx, `
		SELECT date, timezone, rules_version, built_at, revision,
			sessions_scanned, markdown_sha256, run_id
		FROM friction_digests `+where+`
		ORDER BY date DESC`, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("listing friction digests: %w", err)
	}
	defer rows.Close()
	out := []db.FrictionDigest{}
	for rows.Next() {
		var d db.FrictionDigest
		if err := rows.Scan(&d.Date, &d.Timezone, &d.RulesVersion, &d.BuiltAt,
			&d.Revision, &d.SessionsScanned, &d.MarkdownSHA256, &d.RunID); err != nil {
			return nil, fmt.Errorf("scanning friction digest: %w", err)
		}
		d.BuiltAt = d.BuiltAt.UTC()
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing friction digests: %w", err)
	}
	return out, nil
}

// ListFrictionPatterns mirrors internal/db/friction_list.go.
func (s *Store) ListFrictionPatterns(
	ctx context.Context, f db.FrictionPatternFilter,
) ([]db.FrictionPattern, string, error) {
	limit := db.NormalizeFrictionLimit(f.Limit)
	offset, err := db.DecodeFrictionCursor(f.Cursor)
	if err != nil {
		return nil, "", err
	}
	pb := &paramBuilder{}
	var preds []string
	if f.Kind != "" {
		preds = append(preds, "kind = "+pb.add(f.Kind))
	}
	if f.Since != "" {
		preds = append(preds, "last_seen_date >= "+pb.add(f.Since))
	}
	clause, err := db.FrictionLinkStateClause(f.LinkState, "friction_patterns.fingerprint", pb.add)
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
	limitParam := pb.add(limit + 1)
	offsetParam := pb.add(offset)
	rows, err := s.pg.QueryContext(ctx, `
		SELECT fingerprint, kind, title, first_seen_date, last_seen_date,
			occurrence_count, session_count, last_subject_id, last_ordinal
		FROM friction_patterns `+where+`
		ORDER BY occurrence_count DESC, last_seen_date DESC, fingerprint ASC
		LIMIT `+limitParam+` OFFSET `+offsetParam, pb.args...)
	if err != nil {
		return nil, "", fmt.Errorf("listing friction patterns: %w", err)
	}
	defer rows.Close()
	out := make([]db.FrictionPattern, 0, limit+1)
	for rows.Next() {
		var (
			p       db.FrictionPattern
			ordinal sql.NullInt64
		)
		if err := rows.Scan(&p.Fingerprint, &p.Kind, &p.Title, &p.FirstSeenDate,
			&p.LastSeenDate, &p.OccurrenceCount, &p.SessionCount,
			&p.LastSubjectID, &ordinal); err != nil {
			return nil, "", fmt.Errorf("scanning friction pattern: %w", err)
		}
		p.LastOrdinal = pgNullIntPtr(ordinal)
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
		next = db.EncodeFrictionCursor(offset + limit)
	}
	fingerprints := make([]string, len(out))
	for i := range out {
		fingerprints[i] = out[i].Fingerprint
	}
	links, err := s.GetFrictionIssueLinks(ctx, fingerprints)
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
