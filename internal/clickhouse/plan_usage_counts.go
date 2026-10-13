package clickhouse

import (
	"context"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
)

// GetPlanUsageSessionCounts counts the sessions a usage report leaves out
// as plan-only and the sessions it undercounts as partly captured, with
// the same rules as the SQLite query (db.GetPlanUsageSessionCounts).
// ClickHouse has no correlated subqueries, so the session tests use IN.
func (s *Store) GetPlanUsageSessionCounts(
	ctx context.Context, f db.UsageFilter,
) (db.PlanUsageSessionCounts, error) {
	b := chUsageBoundsForFilter(f)
	var args []any

	messageWhere, args := appendChUsageSourceFilterClauses(
		chUsageMatchingMessageEligibility+`
			AND s.id NOT IN (SELECT session_id FROM usage_events)`,
		args, "m.model", f)
	messageWhere, args = appendChUsageSessionFilterClauses(messageWhere, args, f, "")
	messageWhere, args = appendChUsageColumnBounds(
		messageWhere, "COALESCE(m.timestamp, s.started_at)", b, args)
	query := `
		SELECT '` + db.PlanUsageCountKindPlanOnly + `' AS kind, s.id AS session_id,
			m.model AS model, COALESCE(m.timestamp, s.started_at) AS ts
		FROM session_plan_usage p
		JOIN sessions s ON s.id = p.session_id
		JOIN messages m ON m.session_id = s.id
		WHERE ` + messageWhere

	if len(db.CSVFilterValues(f.Model)) == 0 {
		var sessionWhere string
		sessionWhere, args = appendChUsageSessionFilterClauses(`s.deleted_at IS NULL
			AND s.id NOT IN (SELECT session_id FROM usage_events)
			AND s.id NOT IN (
				SELECT m.session_id FROM messages m
				WHERE `+chUsageMatchingMessageSourceEligibility+`
			)`, args, f, "")
		sessionWhere, args = appendChUsageColumnBounds(sessionWhere, "s.started_at", b, args)
		query += `
		UNION ALL
		SELECT '` + db.PlanUsageCountKindPlanOnly + `', s.id, '', s.started_at
		FROM session_plan_usage p
		JOIN sessions s ON s.id = p.session_id
		WHERE ` + sessionWhere
	}

	eventWhere, args := appendChUsageSourceFilterClauses(
		chUsageEventEligibility+`
			AND p.uncaptured_turns > 0`, args, "ue.model", f)
	eventWhere, args = appendChUsageSessionFilterClauses(eventWhere, args, f, "")
	eventWhere, args = appendChUsageColumnBounds(
		eventWhere, "COALESCE(ue.occurred_at, s.started_at)", b, args)
	query += `
		UNION ALL
		SELECT '` + db.PlanUsageCountKindPartlyCaptured + `', s.id, ue.model,
			COALESCE(ue.occurred_at, s.started_at)
		FROM session_plan_usage p
		JOIN sessions s ON s.id = p.session_id
		JOIN usage_events ue ON ue.session_id = s.id
		WHERE ` + eventWhere

	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return db.PlanUsageSessionCounts{}, fmt.Errorf("querying clickhouse plan usage session counts: %w", err)
	}
	defer rows.Close()
	tally := db.NewPlanUsageCountTally(f)
	loc := f.Location()
	for rows.Next() {
		var kind, sessionID, model string
		var ts any
		if err := rows.Scan(&kind, &sessionID, &model, &ts); err != nil {
			return db.PlanUsageSessionCounts{}, fmt.Errorf("scanning clickhouse plan usage session count: %w", err)
		}
		date := ""
		if t, ok := readbase.ParseAnalyticsTime(formatDBTime(ts)); ok {
			date = t.In(loc).Format("2006-01-02")
		}
		if err := tally.Add(kind, sessionID, model, date); err != nil {
			return db.PlanUsageSessionCounts{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return db.PlanUsageSessionCounts{}, fmt.Errorf("iterating clickhouse plan usage session counts: %w", err)
	}
	return tally.Counts(), nil
}
