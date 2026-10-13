package db

import (
	"context"
	"fmt"
)

// PlanUsageSessionCounts reports the sessions a usage report's token and
// cost totals leave out or undercount because their usage arrived as plan
// usage. Every backend computes them at query time from its copy of
// session_plan_usage, with the same usage filter as the totals.
type PlanUsageSessionCounts struct {
	// PlanOnly counts sessions with a plan usage row and no usage events:
	// their usage is billed as plan usage and never priced.
	PlanOnly int
	// PlanOnlyByModel counts those sessions per model of their matching
	// assistant messages. A session counts once under each model.
	PlanOnlyByModel map[string]int
	// PartlyCaptured counts sessions with at least one usage event in the
	// report and at least one finished turn live capture missed, so their
	// token counts are low.
	PartlyCaptured int
}

// Row kinds in the per-backend plan usage count queries.
const (
	PlanUsageCountKindPlanOnly       = "plan_only"
	PlanUsageCountKindPartlyCaptured = "partly_captured"
)

// PlanUsageCountTally collects the rows of a per-backend plan usage count
// query and applies the report's date window, as the matching-session
// count does: a row counts when its local date falls in the window, and
// every row counts when the report has no window.
type PlanUsageCountTally struct {
	filter   UsageFilter
	planOnly map[string]map[string]struct{}
	partly   map[string]struct{}
}

// NewPlanUsageCountTally starts a tally for one usage report.
func NewPlanUsageCountTally(f UsageFilter) *PlanUsageCountTally {
	return &PlanUsageCountTally{
		filter:   f,
		planOnly: map[string]map[string]struct{}{},
		partly:   map[string]struct{}{},
	}
}

// Add records one row. date is the row's calendar date in the report's
// timezone, "" when the row has no usable time.
func (t *PlanUsageCountTally) Add(kind, sessionID, model, date string) error {
	if t.filter.From != "" || t.filter.To != "" {
		if date == "" ||
			(t.filter.From != "" && date < t.filter.From) ||
			(t.filter.To != "" && date > t.filter.To) {
			return nil
		}
	}
	switch kind {
	case PlanUsageCountKindPlanOnly:
		models := t.planOnly[sessionID]
		if models == nil {
			models = map[string]struct{}{}
			t.planOnly[sessionID] = models
		}
		if model != "" {
			models[model] = struct{}{}
		}
	case PlanUsageCountKindPartlyCaptured:
		t.partly[sessionID] = struct{}{}
	default:
		return fmt.Errorf("unknown plan usage count row kind %q", kind)
	}
	return nil
}

// Counts returns the tallied counts.
func (t *PlanUsageCountTally) Counts() PlanUsageSessionCounts {
	out := PlanUsageSessionCounts{
		PlanOnly:        len(t.planOnly),
		PlanOnlyByModel: map[string]int{},
		PartlyCaptured:  len(t.partly),
	}
	for _, models := range t.planOnly {
		for model := range models {
			out.PlanOnlyByModel[model]++
		}
	}
	return out
}

// planUsageCountSQL is the SQLite plan usage count query: plan-only rows
// from matching assistant messages, plan-only rows from sessions with no
// assistant message (dated by started_at, dropped under an include-model
// filter), and partly-captured rows from usage events. Messages and events
// use the same eligibility, model filters, and timestamp fallback as the
// matching-session count and the totals.
func planUsageCountSQL(f UsageFilter) (string, []any) {
	b := usageBoundsForFilter(f)
	var args []any

	messageWhere := usageMatchingMessageEligibility + `
	AND NOT EXISTS (SELECT 1 FROM usage_events ue WHERE ue.session_id = s.id)`
	messageWhere, args = f.appendUsageBranchFilterClauses(messageWhere, args, "m.model")
	messageWhere, args = appendUsageColumnBounds(
		messageWhere, "COALESCE(NULLIF(m.timestamp, ''), s.started_at)", b, args)
	query := `
SELECT '` + PlanUsageCountKindPlanOnly + `', s.id, m.model,
	COALESCE(NULLIF(m.timestamp, ''), s.started_at, '')
FROM session_plan_usage p
JOIN sessions s ON s.id = p.session_id
JOIN messages m ON m.session_id = s.id
WHERE ` + messageWhere

	if len(CSVFilterValues(f.Model)) == 0 {
		sessionWhere := usageSessionEligibility + `
	AND NOT EXISTS (SELECT 1 FROM usage_events ue WHERE ue.session_id = s.id)
	AND NOT EXISTS (
		SELECT 1 FROM messages m
		WHERE m.session_id = s.id AND ` + usageMatchingMessageSourceEligibility + `
	)`
		sessionWhere, args = f.appendUsageSessionFilterClauses(sessionWhere, args)
		sessionWhere, args = appendUsageColumnBounds(sessionWhere, "s.started_at", b, args)
		query += `
UNION ALL
SELECT '` + PlanUsageCountKindPlanOnly + `', s.id, '', COALESCE(s.started_at, '')
FROM session_plan_usage p
JOIN sessions s ON s.id = p.session_id
WHERE ` + sessionWhere
	}

	eventWhere := usageEventEligibility + `
	AND p.uncaptured_turns > 0`
	eventWhere, args = f.appendUsageBranchFilterClauses(eventWhere, args, "ue.model")
	eventWhere, args = appendUsageColumnBounds(
		eventWhere, "COALESCE(ue.occurred_at, s.started_at)", b, args)
	query += `
UNION ALL
SELECT '` + PlanUsageCountKindPartlyCaptured + `', s.id, ue.model,
	COALESCE(ue.occurred_at, s.started_at, '')
FROM session_plan_usage p
JOIN sessions s ON s.id = p.session_id
JOIN usage_events ue ON ue.session_id = s.id
WHERE ` + eventWhere
	return query, args
}

// GetPlanUsageSessionCounts counts the sessions a usage report leaves out
// as plan-only and the sessions it undercounts as partly captured.
func (db *DB) GetPlanUsageSessionCounts(
	ctx context.Context, f UsageFilter,
) (PlanUsageSessionCounts, error) {
	query, args := planUsageCountSQL(f)
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return PlanUsageSessionCounts{}, fmt.Errorf("querying plan usage session counts: %w", err)
	}
	defer rows.Close()
	tally := NewPlanUsageCountTally(f)
	loc := f.Location()
	for rows.Next() {
		var kind, sessionID, model, ts string
		if err := rows.Scan(&kind, &sessionID, &model, &ts); err != nil {
			return PlanUsageSessionCounts{}, fmt.Errorf("scanning plan usage session count: %w", err)
		}
		if err := tally.Add(kind, sessionID, model, LocalDate(ts, loc)); err != nil {
			return PlanUsageSessionCounts{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return PlanUsageSessionCounts{}, fmt.Errorf("iterating plan usage session counts: %w", err)
	}
	return tally.Counts(), nil
}
