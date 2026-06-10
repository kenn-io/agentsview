package duckdb

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) ListSecretFindings(
	ctx context.Context, f db.SecretFindingFilter,
) (db.SecretFindingPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxContentSearchLimit {
		f.Limit = db.DefaultContentSearchLimit
	}
	var preds []string
	args := []any{}
	add := func(pred string, value any) {
		preds = append(preds, pred+" ?")
		args = append(args, value)
	}
	if f.Project != "" {
		add("s.project =", f.Project)
	}
	if f.Agent != "" {
		add("s.agent =", f.Agent)
	}
	if f.Rule != "" {
		add("sf.rule_name =", f.Rule)
	}
	if f.Confidence != "" && f.Confidence != "all" {
		add("sf.confidence =", f.Confidence)
	}
	if len(f.RulesVersions) > 0 {
		var ph []string
		for _, v := range f.RulesVersions {
			if v == "" {
				continue
			}
			ph = append(ph, "?")
			args = append(args, v)
		}
		if len(ph) > 0 {
			preds = append(preds, "sf.rules_version IN ("+strings.Join(ph, ",")+")")
		}
	}
	if f.DateFrom != "" {
		preds = append(preds, "CAST(COALESCE(s.started_at, s.created_at) AS DATE) >= CAST(? AS DATE)")
		args = append(args, f.DateFrom)
	}
	if f.DateTo != "" {
		preds = append(preds, "CAST(COALESCE(s.started_at, s.created_at) AS DATE) <= CAST(? AS DATE)")
		args = append(args, f.DateTo)
	}
	where := "s.deleted_at IS NULL"
	if len(preds) > 0 {
		where += " AND " + strings.Join(preds, " AND ")
	}
	args = append(args, f.Limit+1, f.Cursor)
	rows, err := s.duck.QueryContext(ctx, `
		SELECT sf.session_id, sf.rule_name, sf.confidence,
			sf.location_kind, sf.message_ordinal, sf.call_index,
			sf.event_index, sf.match_start, sf.match_end,
			sf.match_index, sf.redacted_match, sf.rules_version,
			s.project, s.agent
		FROM secret_findings sf
		JOIN sessions s ON s.id = sf.session_id
		WHERE `+where+`
		ORDER BY COALESCE(s.ended_at, s.started_at, s.created_at) DESC,
			sf.session_id, sf.message_ordinal, sf.match_start,
			sf.match_index, sf.id
		LIMIT ? OFFSET ?`,
		args...,
	)
	if err != nil {
		return db.SecretFindingPage{}, fmt.Errorf("listing duckdb findings: %w", err)
	}
	defer rows.Close()
	var out []db.SecretFindingRow
	for rows.Next() {
		var r db.SecretFindingRow
		if err := rows.Scan(&r.SessionID, &r.RuleName, &r.Confidence,
			&r.LocationKind, &r.MessageOrdinal, &r.CallIndex,
			&r.EventIndex, &r.MatchStart, &r.MatchEnd, &r.MatchIndex,
			&r.RedactedMatch, &r.RulesVersion, &r.Project, &r.Agent); err != nil {
			return db.SecretFindingPage{}, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return db.SecretFindingPage{}, err
	}
	page := db.SecretFindingPage{Findings: out}
	if len(out) > f.Limit {
		page.Findings = out[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	return page, nil
}

func (s *Store) SecretFindingSource(
	ctx context.Context, f db.SecretFinding,
) (string, bool, error) {
	msgs, err := s.GetAllMessages(ctx, f.SessionID)
	if err != nil {
		return "", false, err
	}
	text, ok := db.FindingSourceFromMessages(msgs, f)
	return text, ok, nil
}
