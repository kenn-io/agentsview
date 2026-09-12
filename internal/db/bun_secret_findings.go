package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

type bunSecretFindingRow struct {
	SessionID      string `bun:"session_id"`
	RuleName       string `bun:"rule_name"`
	Confidence     string `bun:"confidence"`
	LocationKind   string `bun:"location_kind"`
	MessageOrdinal int    `bun:"message_ordinal"`
	CallIndex      *int   `bun:"call_index"`
	EventIndex     *int   `bun:"event_index"`
	MatchStart     int    `bun:"match_start"`
	MatchEnd       int    `bun:"match_end"`
	MatchIndex     int    `bun:"match_index"`
	RedactedMatch  string `bun:"redacted_match"`
	RulesVersion   string `bun:"rules_version"`
	Project        string `bun:"project"`
	Agent          string `bun:"agent"`
}

// ListSecretFindings applies the canonical finding filters, ordering, and
// offset pagination through the backend's guarded Bun view.
func (s *BunStore) ListSecretFindings(
	ctx context.Context, filter SecretFindingFilter,
) (SecretFindingPage, error) {
	if filter.Limit <= 0 || filter.Limit > MaxContentSearchLimit {
		filter.Limit = DefaultContentSearchLimit
	}
	var page SecretFindingPage
	err := s.view(ctx, func(store bun.IDB) error {
		query := store.NewSelect().TableExpr("secret_findings AS finding").
			ColumnExpr(`finding.session_id, finding.rule_name,
				finding.confidence, finding.location_kind, finding.message_ordinal,
				finding.call_index, finding.event_index, finding.match_start,
				finding.match_end, finding.match_index, finding.redacted_match,
				finding.rules_version, session.project, session.agent`).
			Join("JOIN sessions AS session ON session.id = finding.session_id").
			Where("session.deleted_at IS NULL")
		if filter.Project != "" {
			query = query.Where("session.project = ?", filter.Project)
		}
		if filter.Agent != "" {
			query = query.Where("session.agent = ?", filter.Agent)
		}
		if filter.Rule != "" {
			query = query.Where("finding.rule_name = ?", filter.Rule)
		}
		if filter.Confidence != "" && filter.Confidence != "all" {
			query = query.Where("finding.confidence = ?", filter.Confidence)
		}
		versions := make([]string, 0, len(filter.RulesVersions))
		for _, version := range filter.RulesVersions {
			if version != "" {
				versions = append(versions, version)
			}
		}
		if len(versions) > 0 {
			query = query.Where("finding.rules_version IN (?)", bun.List(versions))
		}
		timestampOrderExpr := s.backend.TimestampOrderExpr
		started := timestampOrderExpr("COALESCE(" +
			bunNullableTimestamp("session.started_at") + ", session.created_at)")
		if filter.DateFrom != "" {
			query = query.Where(started+" >= "+timestampOrderExpr("?"),
				sessionDateBoundary(filter.DateFrom, "UTC", false))
		}
		if filter.DateTo != "" {
			query = query.Where(started+" < "+timestampOrderExpr("?"),
				sessionDateBoundary(filter.DateTo, "UTC", true))
		}
		activity := "COALESCE(" + timestampOrderExpr("session.ended_at") + ", " +
			timestampOrderExpr("session.started_at") + ", " +
			timestampOrderExpr("session.created_at") + ")"
		var rows []bunSecretFindingRow
		if err := query.OrderExpr(activity+" DESC").
			OrderExpr(`finding.session_id, finding.message_ordinal,
				finding.match_start, finding.match_index, finding.id`).
			Limit(filter.Limit+1).Offset(filter.Cursor).Scan(ctx, &rows); err != nil {
			return fmt.Errorf("listing Bun secret findings: %w", err)
		}
		findings := make([]SecretFindingRow, len(rows))
		for i, row := range rows {
			findings[i] = SecretFindingRow{
				SecretFinding: SecretFinding{
					SessionID: row.SessionID, RuleName: row.RuleName,
					Confidence: row.Confidence, LocationKind: row.LocationKind,
					MessageOrdinal: row.MessageOrdinal, CallIndex: row.CallIndex,
					EventIndex: row.EventIndex, MatchStart: row.MatchStart,
					MatchEnd: row.MatchEnd, MatchIndex: row.MatchIndex,
					RedactedMatch: row.RedactedMatch, RulesVersion: row.RulesVersion,
				},
				Project: row.Project, Agent: row.Agent,
			}
		}
		page = SecretFindingPage{Findings: findings}
		if len(findings) > filter.Limit {
			page.Findings = findings[:filter.Limit]
			page.NextCursor = filter.Cursor + filter.Limit
		}
		return nil
	})
	return page, err
}

// SecretFindingSource reads only the exact canonical source coordinate stored
// with a finding. A missing or stale coordinate returns ok=false.
func (s *BunStore) SecretFindingSource(
	ctx context.Context, finding SecretFinding,
) (string, bool, error) {
	query, args, valid := secretFindingSourceQuery(finding)
	if !valid {
		return "", false, nil
	}
	var source string
	var found bool
	err := s.view(ctx, func(store bun.IDB) error {
		var row struct {
			Content string `bun:"content"`
		}
		if err := store.NewRaw(query, args...).Scan(ctx, &row); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("reading Bun secret finding source: %w", err)
		}
		source = row.Content
		found = true
		return nil
	})
	return source, found, err
}

func secretFindingSourceQuery(finding SecretFinding) (string, []any, bool) {
	baseArgs := []any{finding.SessionID, finding.MessageOrdinal}
	switch finding.LocationKind {
	case "message":
		return `SELECT content FROM messages
			WHERE session_id = ? AND ordinal = ?`, baseArgs, true
	case "tool_input":
		if finding.CallIndex == nil || *finding.CallIndex < 0 {
			return "", nil, false
		}
		return `SELECT COALESCE(input_json, '') AS content FROM tool_calls
			WHERE session_id = ? AND message_ordinal = ? AND call_index = ?`,
			append(baseArgs, *finding.CallIndex), true
	case "tool_result":
		if finding.CallIndex == nil || *finding.CallIndex < 0 {
			return "", nil, false
		}
		return `SELECT COALESCE(call.result_content, '') AS content
			FROM tool_calls AS call
			WHERE call.session_id = ? AND call.message_ordinal = ?
				AND call.call_index = ?
				AND NOT EXISTS (
					SELECT 1 FROM tool_result_events AS event
					WHERE event.session_id = call.session_id
						AND event.tool_call_message_ordinal = call.message_ordinal
						AND event.call_index = call.call_index
				)`, append(baseArgs, *finding.CallIndex), true
	case "tool_result_event":
		if finding.CallIndex == nil || *finding.CallIndex < 0 ||
			finding.EventIndex == nil || *finding.EventIndex < 0 {
			return "", nil, false
		}
		return `SELECT content FROM tool_result_events
			WHERE session_id = ? AND tool_call_message_ordinal = ?
				AND call_index = ? AND event_index = ?`,
			append(baseArgs, *finding.CallIndex, *finding.EventIndex), true
	default:
		return "", nil, false
	}
}
