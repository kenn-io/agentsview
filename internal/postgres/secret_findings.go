package postgres

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// ListSecretFindings returns findings joined to their sessions, filtered.
// It mirrors internal/db/secret_findings_list.go using PostgreSQL idioms.
func (s *Store) ListSecretFindings(
	ctx context.Context, f db.SecretFindingFilter,
) (db.SecretFindingPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxContentSearchLimit {
		f.Limit = db.DefaultContentSearchLimit
	}

	pb := &paramBuilder{}
	var preds []string
	add := func(p string, v any) {
		preds = append(preds, p+" "+pb.add(v))
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
		var versionParams []string
		for _, v := range f.RulesVersions {
			if v == "" {
				continue
			}
			versionParams = append(versionParams, pb.add(v))
		}
		if len(versionParams) > 0 {
			preds = append(preds,
				"sf.rules_version IN ("+strings.Join(versionParams, ",")+")")
		}
	}
	if f.DateFrom != "" {
		preds = append(preds,
			"DATE(COALESCE(s.started_at, s.created_at) AT TIME ZONE 'UTC') >= "+
				pb.add(f.DateFrom)+"::date")
	}
	if f.DateTo != "" {
		preds = append(preds,
			"DATE(COALESCE(s.started_at, s.created_at) AT TIME ZONE 'UTC') <= "+
				pb.add(f.DateTo)+"::date")
	}

	where := "s.deleted_at IS NULL AND NOT s.rollout_hidden"
	if len(preds) > 0 {
		where += " AND " + strings.Join(preds, " AND ")
	}

	limitParam := pb.add(f.Limit + 1)
	offsetParam := pb.add(f.Cursor)

	query := `
		SELECT sf.session_id, sf.rule_name, sf.confidence, sf.location_kind,
			sf.message_ordinal, sf.call_index, sf.event_index,
			sf.match_start, sf.match_end, sf.match_index,
			sf.redacted_match, sf.rules_version, s.project, s.agent
		FROM secret_findings sf JOIN sessions s ON s.id = sf.session_id
		WHERE ` + where + `
		ORDER BY COALESCE(s.ended_at, s.started_at, s.created_at) DESC,
			sf.session_id, sf.message_ordinal, sf.match_start,
			sf.match_index, sf.id
		LIMIT ` + limitParam + ` OFFSET ` + offsetParam

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return db.SecretFindingPage{},
			fmt.Errorf("listing findings: %w", err)
	}
	defer rows.Close()

	out := make([]db.SecretFindingRow, 0, f.Limit+1)
	for rows.Next() {
		var r db.SecretFindingRow
		if err := rows.Scan(
			&r.SessionID, &r.RuleName, &r.Confidence,
			&r.LocationKind, &r.MessageOrdinal,
			&r.CallIndex, &r.EventIndex,
			&r.MatchStart, &r.MatchEnd, &r.MatchIndex,
			&r.RedactedMatch, &r.RulesVersion,
			&r.Project, &r.Agent,
		); err != nil {
			return db.SecretFindingPage{},
				fmt.Errorf("scan finding row: %w", err)
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

// SecretFindingSource returns the full source text that finding f points at,
// loading messages via the PG store then delegating to the shared helper.
func (s *Store) SecretFindingSource(
	ctx context.Context, f db.SecretFinding,
) (string, bool, error) {
	msgs, err := s.GetAllMessages(ctx, f.SessionID)
	if err != nil {
		return "", false,
			fmt.Errorf("loading messages for finding source: %w", err)
	}
	text, ok := db.FindingSourceFromMessages(msgs, f)
	return text, ok, nil
}
