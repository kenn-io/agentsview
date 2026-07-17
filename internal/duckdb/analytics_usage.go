package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/signals"
)

const (
	duckActiveWindow = 10 * time.Minute
	duckStaleWindow  = 60 * time.Minute
)

type duckAnalyticsSession struct {
	id                          string
	project                     string
	machine                     string
	agent                       string
	firstMessage                *string
	displayName                 *string
	startedAt                   string
	endedAt                     string
	createdAt                   string
	messageCount                int
	userMessageCount            int
	totalOutputTokens           int
	hasTotalOutputTokens        bool
	isAutomated                 bool
	terminationStatus           *string
	healthScore                 *int
	healthGrade                 *string
	outcome                     string
	outcomeConfidence           string
	toolFailures                int
	toolRetries                 int
	editChurn                   int
	compactions                 int
	midTaskCompactions          int
	contextPressureMax          *float64
	qualitySignalVersion        int
	shortPromptCount            int
	unstructuredStart           bool
	missingSuccessCriteriaCount int
	missingVerificationCount    int
	duplicatePromptCount        int
	noCodeContextCount          int
	runawayToolLoopCount        int
	frustrationMarkerCount      int
}

func (s *Store) analyticsSessions(
	ctx context.Context, f db.AnalyticsFilter,
) ([]duckAnalyticsSession, error) {
	return s.analyticsSessionsFiltered(ctx, f, true, true)
}

// analyticsSessionsFiltered loads candidate sessions, optionally applying
// the date and hour/day-of-week predicates at the session level. Skill
// analytics passes false for both so those filters can be applied to each
// call's own message timestamp instead. With a model filter and an active
// hour/dow filter it pairs through the shared scope reducer (see
// analyticsSessionsModelTimeFiltered) so an empty-model user turn at the
// selected hour keeps its session, matching how the model-scoped panels count.
func (s *Store) analyticsSessionsFiltered(
	ctx context.Context, f db.AnalyticsFilter,
	includeDate, includeTime bool,
) ([]duckAnalyticsSession, error) {
	if includeTime && f.HasTimeFilter() && strings.TrimSpace(f.Model) != "" {
		return s.analyticsSessionsModelTimeFiltered(ctx, f, includeDate)
	}
	where, args := duckBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.",
		includeDate, includeTime)
	rows, err := s.queryContext(ctx, `
		SELECT id, project, machine, agent, first_message,
			COALESCE(display_name, session_name) AS display_name,
			started_at, ended_at, created_at, message_count,
			user_message_count, total_output_tokens,
			has_total_output_tokens, is_automated,
			termination_status, health_score, health_grade, outcome,
			outcome_confidence, tool_failure_signal_count,
			tool_retry_count, edit_churn_count, compaction_count,
			mid_task_compaction_count, context_pressure_max,
			quality_signal_version, short_prompt_count,
			unstructured_start, missing_success_criteria_count,
			missing_verification_count, duplicate_prompt_count,
			no_code_context_count, runaway_tool_loop_count
		FROM sessions s
		WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb analytics sessions: %w", err)
	}
	defer rows.Close()

	var out []duckAnalyticsSession
	for rows.Next() {
		var r duckAnalyticsSession
		var startedAt, endedAt, createdAt any
		if err := rows.Scan(
			&r.id, &r.project, &r.machine, &r.agent,
			&r.firstMessage, &r.displayName,
			&startedAt, &endedAt, &createdAt,
			&r.messageCount, &r.userMessageCount,
			&r.totalOutputTokens, &r.hasTotalOutputTokens,
			&r.isAutomated, &r.terminationStatus,
			&r.healthScore, &r.healthGrade, &r.outcome,
			&r.outcomeConfidence, &r.toolFailures, &r.toolRetries,
			&r.editChurn, &r.compactions, &r.midTaskCompactions,
			&r.contextPressureMax, &r.qualitySignalVersion,
			&r.shortPromptCount, &r.unstructuredStart,
			&r.missingSuccessCriteriaCount, &r.missingVerificationCount,
			&r.duplicatePromptCount, &r.noCodeContextCount,
			&r.runawayToolLoopCount,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb analytics session: %w", err)
		}
		r.startedAt = formatDBTime(startedAt)
		r.endedAt = formatDBTime(endedAt)
		r.createdAt = formatDBTime(createdAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// analyticsSessionsModelTimeFiltered loads the date- and model-scoped sessions
// (without the in-SQL day/hour predicate) and keeps only those with at least
// one scoped message matching the hour/dow filter. Running the shared reducer
// instead of the direct m.model time predicate keeps sessions whose matching
// message is an empty-model user turn paired with the selected-model assistant.
func (s *Store) analyticsSessionsModelTimeFiltered(
	ctx context.Context, f db.AnalyticsFilter, includeDate bool,
) ([]duckAnalyticsSession, error) {
	sessions, err := s.analyticsSessionsFiltered(ctx, f, includeDate, false)
	if err != nil {
		return nil, err
	}
	candidateIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		candidateIDs = append(candidateIDs, session.id)
	}
	scope, err := s.resolveAnalyticsMessageScope(ctx, candidateIDs, f, false)
	if err != nil {
		return nil, err
	}
	matched := make(map[string]struct{})
	if scope != nil {
		for id := range scope.MessagesBySession() {
			matched[id] = struct{}{}
		}
	}
	out := make([]duckAnalyticsSession, 0, len(sessions))
	for _, session := range sessions {
		if _, ok := matched[session.id]; ok {
			out = append(out, session)
		}
	}
	return out, nil
}

func duckBuildAnalyticsWhere(
	f db.AnalyticsFilter,
	dateCol string,
	tablePrefix string,
	includeDate bool,
	includeTime bool,
) (string, []any) {
	q := func(col string) string { return tablePrefix + col }
	preds := []string{
		q("message_count") + " > 0",
		// Mirror the SQLite analytics filter: subagent and fork rows are
		// excluded unless the filter opts in (sum/count surfaces for
		// subagents, the activity report for both). The shared helper
		// qualifies the column with tablePrefix directly.
		db.RelationshipExclusionSQL(f.IncludeSubagents, f.IncludeForks, tablePrefix),
		q("deleted_at") + " IS NULL",
	}
	var args []any

	if includeDate {
		if f.From != "" {
			preds = append(preds, dateCol+" >= CAST(? AS TIMESTAMP)")
			args = append(args, duckUsagePaddedUTCBound(f.From+"T00:00:00Z", -14))
		}
		if f.To != "" {
			preds = append(preds, dateCol+" <= CAST(? AS TIMESTAMP)")
			args = append(args, duckUsagePaddedUTCBound(f.To+"T23:59:59Z", 14))
		}
		localDate, localDateArgs := duckAnalyticsLocalDateExpr(dateCol, f)
		if f.From != "" {
			preds = append(preds, localDate+" >= ?")
			args = append(args, append(localDateArgs, f.From)...)
		}
		if f.To != "" {
			preds = append(preds, localDate+" <= ?")
			args = append(args, append(localDateArgs, f.To)...)
		}
	}

	if f.Machine != "" {
		preds, args = appendDuckAnalyticsCSVFilter(preds, args, q("machine"), f.Machine)
	}
	if f.Project != "" {
		preds = append(preds, q("project")+" = ?")
		args = append(args, f.Project)
	}
	if f.GitBranch != "" {
		var clause string
		clause, args = db.BranchPairClauseArgs(q("project"), q("git_branch"), f.GitBranch, args)
		preds = append(preds, clause)
	}
	if f.Agent != "" {
		preds, args = appendDuckAnalyticsCSVFilter(preds, args, q("agent"), f.Agent)
	}
	if modelPred, modelArgs := duckAnalyticsCSVPredicate("m.model", f.Model); modelPred != "" {
		preds = append(preds,
			"EXISTS (SELECT 1 FROM messages m WHERE m.session_id = "+q("id")+" AND "+modelPred+")")
		args = append(args, modelArgs...)
	}
	if f.MinUserMessages > 0 {
		preds = append(preds, q("user_message_count")+" >= ?")
		args = append(args, f.MinUserMessages)
	}
	scope := duckNormalizeAutomatedScope(
		f.AutomatedScope, f.ExcludeAutomated)
	if f.ExcludeOneShot {
		// Exempt subagents from one-shot exclusion when counting them,
		// mirroring db.AnalyticsFilter.OneShotExclusionSQL. Workflow
		// subagents are inherently one-shot but represent real work.
		oneShot := func(base string) string {
			if f.IncludeSubagents {
				return "(" + base + " OR " +
					q("relationship_type") + " = 'subagent')"
			}
			return base
		}
		if scope != "human" {
			preds = append(preds, oneShot("("+q("user_message_count")+" > 1 OR "+q("is_automated")+" = TRUE)"))
		} else {
			preds = append(preds, oneShot(q("user_message_count")+" > 1"))
		}
	}
	if pred := duckAutomatedScopePredicate(
		scope, q("is_automated")); pred != "" {
		preds = append(preds, pred)
	}
	if f.ExcludeInteractive {
		preds = append(preds, q("is_automated")+" = TRUE")
	}
	if f.ActiveSince != "" {
		activeSince := f.ActiveSince
		if parsed, ok := parseAnalyticsTime(f.ActiveSince); ok {
			activeSince = parsed.Format(time.RFC3339)
		}
		preds = append(preds,
			"COALESCE("+q("ended_at")+", "+q("started_at")+", "+q("created_at")+") >= CAST(? AS TIMESTAMP)")
		args = append(args, activeSince)
	}
	if pred, predArgs := duckAnalyticsTerminationPred(
		f.Termination,
		"COALESCE("+q("ended_at")+", "+q("started_at")+", "+q("created_at")+")",
		q("termination_status"),
	); pred != "" {
		preds = append(preds, pred)
		args = append(args, predArgs...)
	}
	if includeTime && (f.DayOfWeek != nil || f.Hour != nil) {
		pred, predArgs := duckAnalyticsMessageTimeExists(f, q("id"))
		preds = append(preds, pred)
		args = append(args, predArgs...)
	}

	return strings.Join(preds, " AND "), args
}

func duckNormalizeAutomatedScope(
	scope string,
	excludeAutomated bool,
) string {
	switch strings.TrimSpace(scope) {
	case "human", "all", "automated":
		return strings.TrimSpace(scope)
	}
	if excludeAutomated {
		return "human"
	}
	return "all"
}

func duckAutomatedScopePredicate(scope, col string) string {
	switch scope {
	case "human":
		return col + " = FALSE"
	case "automated":
		return col + " = TRUE"
	default:
		return ""
	}
}

func appendDuckAnalyticsCSVFilter(
	preds []string, args []any, col string, raw string,
) ([]string, []any) {
	pred, predArgs := duckAnalyticsCSVPredicate(col, raw)
	if pred != "" {
		preds = append(preds, pred)
		args = append(args, predArgs...)
	}
	return preds, args
}

func duckAnalyticsCSVValues(raw string) []string {
	values := strings.Split(raw, ",")
	out := values[:0]
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func duckAnalyticsCSVPredicate(
	col string, raw string,
) (string, []any) {
	values := duckAnalyticsCSVValues(raw)
	if len(values) == 0 {
		return "", nil
	}
	if len(values) == 1 {
		return col + " = ?", []any{values[0]}
	}
	placeholders := make([]string, len(values))
	args := make([]any, 0, len(values))
	for i, value := range values {
		placeholders[i] = "?"
		args = append(args, value)
	}
	return col + " IN (" + strings.Join(placeholders, ",") + ")", args
}

func duckAnalyticsLocalDateExpr(
	tsExpr string, f db.AnalyticsFilter,
) (string, []any) {
	if f.Timezone != "" {
		return "strftime(timezone(?, timezone('UTC', " + tsExpr + ")), '%Y-%m-%d')",
			[]any{f.Timezone}
	}
	return "strftime(" + tsExpr + ", '%Y-%m-%d')", nil
}

func duckAnalyticsLocalTimeExpr(
	tsExpr string, f db.AnalyticsFilter,
) (string, []any) {
	if f.Timezone != "" {
		return "timezone(?, timezone('UTC', " + tsExpr + "))", []any{f.Timezone}
	}
	return tsExpr, nil
}

func duckAnalyticsMessageTimeExists(
	f db.AnalyticsFilter, sessionIDExpr string,
) (string, []any) {
	preds := []string{
		"m.session_id = " + sessionIDExpr,
		"m.timestamp IS NOT NULL",
	}
	var args []any
	if modelPred, modelArgs := duckAnalyticsCSVPredicate("m.model", f.Model); modelPred != "" {
		preds = append(preds, modelPred)
		args = append(args, modelArgs...)
	}
	if f.DayOfWeek != nil {
		local, localArgs := duckAnalyticsLocalTimeExpr("m.timestamp", f)
		preds = append(preds,
			"((CAST(strftime("+local+", '%w') AS INTEGER) + 6) % 7) = ?")
		args = append(args, append(localArgs, *f.DayOfWeek)...)
	}
	if f.Hour != nil {
		local, localArgs := duckAnalyticsLocalTimeExpr("m.timestamp", f)
		preds = append(preds,
			"CAST(strftime("+local+", '%H') AS INTEGER) = ?")
		args = append(args, append(localArgs, *f.Hour)...)
	}
	return "EXISTS (SELECT 1 FROM messages m WHERE " +
		strings.Join(preds, " AND ") + ")", args
}

func duckAnalyticsTerminationPred(
	status string,
	activityExpr string,
	statusExpr string,
) (string, []any) {
	if status == "" || status == "all" {
		return "", nil
	}
	now := time.Now().UTC()
	activeCutoff := now.Add(-duckActiveWindow)
	staleCutoff := now.Add(-duckStaleWindow)
	flagged := statusExpr + " IN ('tool_call_pending', 'truncated')"
	var parts []string
	var args []any
	for part := range strings.SplitSeq(status, ",") {
		switch strings.TrimSpace(part) {
		case "active":
			parts = append(parts, activityExpr+" > CAST(? AS TIMESTAMP)")
			args = append(args, activeCutoff.Format(time.RFC3339))
		case "stale":
			parts = append(parts, "("+flagged+
				" AND "+activityExpr+" > CAST(? AS TIMESTAMP)"+
				" AND "+activityExpr+" <= CAST(? AS TIMESTAMP))")
			args = append(args,
				staleCutoff.Format(time.RFC3339),
				activeCutoff.Format(time.RFC3339),
			)
		case "unclean":
			parts = append(parts, "("+flagged+
				" AND "+activityExpr+" <= CAST(? AS TIMESTAMP))")
			args = append(args, staleCutoff.Format(time.RFC3339))
		case "clean":
			parts = append(parts, statusExpr+" = 'clean'")
		case "awaiting_user":
			parts = append(parts, statusExpr+" = 'awaiting_user'")
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

func duckAnalyticsTimeMatches(t time.Time, f db.AnalyticsFilter) bool {
	if f.DayOfWeek != nil {
		dow := (int(t.Weekday()) + 6) % 7
		if dow != *f.DayOfWeek {
			return false
		}
	}
	if f.Hour != nil && t.Hour() != *f.Hour {
		return false
	}
	return true
}

func analyticsDateTime(r duckAnalyticsSession) string {
	if r.startedAt != "" {
		return r.startedAt
	}
	return r.createdAt
}

func analyticsLocalDate(ts, tz string) string {
	t, ok := parseAnalyticsTime(ts)
	if !ok {
		return ""
	}
	return t.In(analyticsLocation(tz)).Format("2006-01-02")
}

func analyticsLocation(tz string) *time.Location {
	if tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

func parseAnalyticsTime(ts string) (time.Time, bool) {
	if t, ok := parseTimestamp(ts); ok {
		return t, true
	}
	layouts := []string{
		"2006-01-02 15:04:05.999999-07",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, ts); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func median(values []int) int {
	if len(values) == 0 {
		return 0
	}
	n := len(values)
	if n%2 == 0 {
		return (values[n/2-1] + values[n/2]) / 2
	}
	return values[n/2]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func (s *Store) getAnalyticsModelsForSessionIDs(
	ctx context.Context, sessionIDs []string,
) ([]string, error) {
	if len(sessionIDs) == 0 {
		return []string{}, nil
	}
	models := map[string]bool{}
	err := duckQueryChunked(sessionIDs, func(chunk []string) error {
		ph, args := duckInPlaceholders(chunk)
		rows, err := s.queryContext(ctx, `
			SELECT DISTINCT model
			FROM messages
			WHERE session_id IN `+ph+`
				AND COALESCE(model, '') <> ''
			ORDER BY model`, args...)
		if err != nil {
			return fmt.Errorf("querying duckdb analytics models: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var model string
			if err := rows.Scan(&model); err != nil {
				return fmt.Errorf("scanning duckdb analytics model: %w", err)
			}
			models[model] = true
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return sortedBoolKeys(models), nil
}

func (s *Store) getAnalyticsModelsForSessionIDsFiltered(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
) ([]string, error) {
	if len(sessionIDs) == 0 {
		return []string{}, nil
	}
	seen := make(map[string]struct{}, len(sessionIDs))
	unique := make([]string, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		if _, ok := seen[sessionID]; ok {
			continue
		}
		seen[sessionID] = struct{}{}
		unique = append(unique, sessionID)
	}

	filterModels := duckAnalyticsCSVValues(f.Model)
	allowedModels := make(map[string]struct{}, len(filterModels))
	for _, model := range filterModels {
		allowedModels[model] = struct{}{}
	}
	loc := analyticsLocation(f.Timezone)
	models := map[string]bool{}
	err := duckQueryChunked(unique, func(chunk []string) error {
		ph, args := duckInPlaceholders(chunk)
		rows, err := s.queryContext(ctx, `
			SELECT model, timestamp
			FROM messages
			WHERE session_id IN `+ph+`
				AND COALESCE(model, '') <> ''`, args...)
		if err != nil {
			return fmt.Errorf("querying duckdb filtered analytics models: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var model string
			var ts any
			if err := rows.Scan(&model, &ts); err != nil {
				return fmt.Errorf("scanning duckdb filtered analytics model: %w", err)
			}
			if len(allowedModels) > 0 {
				if _, ok := allowedModels[model]; !ok {
					continue
				}
			}
			if f.HasTimeFilter() {
				t, ok := parseAnalyticsTime(formatDBTime(ts))
				if !ok || !duckAnalyticsTimeMatches(t.In(loc), f) {
					continue
				}
			}
			models[model] = true
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return sortedBoolKeys(models), nil
}

func (s *Store) getAnalyticsFilteredMessageStats(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
) (map[string]db.MessageStats, error) {
	scope, err := s.resolveAnalyticsMessageScope(ctx, sessionIDs, f, false)
	if err != nil {
		return nil, err
	}
	if scope == nil {
		return map[string]db.MessageStats{}, nil
	}
	return scope.StatsBySession(), nil
}

func (s *Store) analyticsSessionsWithModelMessageCounts(
	ctx context.Context, f db.AnalyticsFilter,
) ([]duckAnalyticsSession, error) {
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil || strings.TrimSpace(f.Model) == "" || len(sessions) == 0 {
		return sessions, err
	}

	sessionIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		sessionIDs = append(sessionIDs, session.id)
	}
	stats, err := s.getAnalyticsFilteredMessageStats(
		ctx, sessionIDs, f,
	)
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		stat := stats[sessions[i].id]
		sessions[i].messageCount = stat.Messages
		sessions[i].totalOutputTokens = stat.OutputTokens
		sessions[i].hasTotalOutputTokens = stat.HasOutputTokens
	}
	return sessions, nil
}

func (s *Store) getAnalyticsSummaryWithModelCounts(
	ctx context.Context, f db.AnalyticsFilter,
) (db.AnalyticsSummary, error) {
	sessions, err := s.analyticsSessionsWithModelMessageCounts(ctx, f)
	if err != nil {
		return db.AnalyticsSummary{}, err
	}

	resp := db.AnalyticsSummary{
		Agents: map[string]*db.AgentSummary{},
		Models: []string{},
	}
	if len(sessions) == 0 {
		return resp, nil
	}

	days := map[string]bool{}
	projects := map[string]int{}
	msgCounts := make([]int, 0, len(sessions))
	sessionIDs := make([]string, 0, len(sessions))

	for _, session := range sessions {
		date := analyticsLocalDate(analyticsDateTime(session), f.Timezone)
		resp.TotalSessions++
		resp.TotalMessages += session.messageCount
		if session.hasTotalOutputTokens {
			resp.TotalOutputTokens += session.totalOutputTokens
			resp.TokenReportingSessions++
		}
		days[date] = true
		projects[session.project] += session.messageCount
		msgCounts = append(msgCounts, session.messageCount)
		sessionIDs = append(sessionIDs, session.id)

		if resp.Agents[session.agent] == nil {
			resp.Agents[session.agent] = &db.AgentSummary{}
		}
		resp.Agents[session.agent].Sessions++
		resp.Agents[session.agent].Messages += session.messageCount
	}

	var models []string
	if strings.TrimSpace(f.Model) != "" {
		models, err = s.getAnalyticsModelsForSessionIDsFiltered(
			ctx, sessionIDs, f,
		)
	} else {
		models, err = s.getAnalyticsModelsForSessionIDs(ctx, sessionIDs)
	}
	if err != nil {
		return db.AnalyticsSummary{}, err
	}
	resp.Models = models
	resp.ActiveProjects = len(projects)
	resp.ActiveDays = len(days)
	resp.AvgMessages = round1(float64(resp.TotalMessages) / float64(resp.TotalSessions))

	sort.Ints(msgCounts)
	resp.MedianMessages = median(msgCounts)
	if n := len(msgCounts); n > 0 {
		resp.P90Messages = msgCounts[min(int(math.Floor(float64(n)*0.9))+1, n)-1]
	}

	maxMsgs := -1
	for _, name := range sortedKeys(projects) {
		if projects[name] > maxMsgs {
			maxMsgs = projects[name]
			resp.MostActive = name
		}
	}

	if resp.TotalMessages > 0 {
		counts := make([]int, 0, len(projects))
		for _, count := range projects {
			counts = append(counts, count)
		}
		sort.Sort(sort.Reverse(sort.IntSlice(counts)))
		topSum := 0
		for _, count := range counts[:min(3, len(counts))] {
			topSum += count
		}
		resp.Concentration = math.Round(
			float64(topSum)/float64(resp.TotalMessages)*1000,
		) / 1000
	}
	return resp, nil
}

func (s *Store) GetAnalyticsSummary(
	ctx context.Context, f db.AnalyticsFilter,
) (db.AnalyticsSummary, error) {
	// Sum/count aggregate: count subagent sessions (mirrors SQLite).
	f.IncludeSubagents = true
	if strings.TrimSpace(f.Model) != "" {
		return s.getAnalyticsSummaryWithModelCounts(ctx, f)
	}
	where, args := duckBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	localDate, localDateArgs := duckAnalyticsLocalDateExpr(
		"COALESCE(s.started_at, s.created_at)", f)
	queryArgs := append([]any{}, localDateArgs...)
	queryArgs = append(queryArgs, args...)
	query := `
		WITH filtered AS (
			SELECT s.id, s.project, s.agent, s.message_count,
				s.total_output_tokens, s.has_total_output_tokens,
				` + localDate + ` AS local_date
			FROM sessions s
			WHERE ` + where + `
		),
		ranked AS (
			SELECT message_count,
				ROW_NUMBER() OVER (ORDER BY message_count ASC) AS rn,
				COUNT(*) OVER () AS n
			FROM filtered
		),
		project_totals AS (
			SELECT project, SUM(message_count) AS messages
			FROM filtered
			GROUP BY project
		)
		SELECT
			COUNT(*) AS total_sessions,
			COALESCE(SUM(message_count), 0) AS total_messages,
			COALESCE(SUM(CASE WHEN has_total_output_tokens
				THEN total_output_tokens ELSE 0 END), 0) AS total_output_tokens,
			COALESCE(SUM(CASE WHEN has_total_output_tokens
				THEN 1 ELSE 0 END), 0) AS token_reporting_sessions,
			COUNT(DISTINCT project) AS active_projects,
			COUNT(DISTINCT local_date) AS active_days,
			COALESCE(ROUND(AVG(message_count), 1), 0) AS avg_messages,
			COALESCE((
				SELECT CAST(FLOOR(AVG(message_count)) AS INTEGER)
				FROM ranked
				WHERE rn IN (
					CAST(FLOOR((n + 1) / 2.0) AS BIGINT),
					CAST(FLOOR((n + 2) / 2.0) AS BIGINT)
				)
			), 0) AS median_messages,
			COALESCE((
				SELECT message_count
				FROM ranked
				WHERE rn = LEAST(CAST(FLOOR(n * 0.9) AS BIGINT) + 1, n)
				LIMIT 1
			), 0) AS p90_messages,
			COALESCE((
				SELECT project
				FROM project_totals
				ORDER BY messages DESC, project ASC
				LIMIT 1
			), '') AS most_active,
			COALESCE(ROUND((
				SELECT SUM(messages)
				FROM (
					SELECT messages
					FROM project_totals
					ORDER BY messages DESC
					LIMIT 3
				) top_projects
			)::DOUBLE / NULLIF(SUM(message_count), 0), 3), 0) AS concentration
		FROM filtered`
	rows, err := s.queryContext(ctx, query, queryArgs...)
	if err != nil {
		return db.AnalyticsSummary{}, fmt.Errorf("querying duckdb analytics summary: %w", err)
	}
	resp := db.AnalyticsSummary{Agents: map[string]*db.AgentSummary{}}
	if !rows.Next() {
		rows.Close()
		return resp, nil
	}
	if err := rows.Scan(
		&resp.TotalSessions,
		&resp.TotalMessages,
		&resp.TotalOutputTokens,
		&resp.TokenReportingSessions,
		&resp.ActiveProjects,
		&resp.ActiveDays,
		&resp.AvgMessages,
		&resp.MedianMessages,
		&resp.P90Messages,
		&resp.MostActive,
		&resp.Concentration,
	); err != nil {
		rows.Close()
		return db.AnalyticsSummary{}, fmt.Errorf("scanning duckdb analytics summary: %w", err)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return db.AnalyticsSummary{}, fmt.Errorf("iterating duckdb analytics summary: %w", err)
	}
	if err := rows.Close(); err != nil {
		return db.AnalyticsSummary{}, fmt.Errorf("closing duckdb analytics summary rows: %w", err)
	}

	agentRows, err := s.queryContext(ctx, `
		WITH filtered AS (
			SELECT s.agent, s.message_count
			FROM sessions s
			WHERE `+where+`
		)
		SELECT agent, COUNT(*), COALESCE(SUM(message_count), 0)
		FROM filtered
		GROUP BY agent`,
		args...,
	)
	if err != nil {
		return db.AnalyticsSummary{}, fmt.Errorf("querying duckdb analytics summary agents: %w", err)
	}
	defer agentRows.Close()
	for agentRows.Next() {
		var agent string
		var summary db.AgentSummary
		if err := agentRows.Scan(&agent, &summary.Sessions, &summary.Messages); err != nil {
			return db.AnalyticsSummary{}, fmt.Errorf("scanning duckdb analytics summary agent: %w", err)
		}
		resp.Agents[agent] = &summary
	}
	if err := agentRows.Err(); err != nil {
		return db.AnalyticsSummary{}, fmt.Errorf("iterating duckdb analytics summary agents: %w", err)
	}
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.AnalyticsSummary{}, err
	}
	sessionIDs := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		sessionIDs = append(sessionIDs, sess.id)
	}
	var models []string
	if f.HasTimeFilter() {
		models, err = s.getAnalyticsModelsForSessionIDsFiltered(
			ctx, sessionIDs, f,
		)
	} else {
		models, err = s.getAnalyticsModelsForSessionIDs(
			ctx, sessionIDs,
		)
	}
	if err != nil {
		return db.AnalyticsSummary{}, err
	}
	resp.Models = models
	return resp, nil
}

func (s *Store) getAnalyticsFilteredToolCallCounts(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
) (map[string]int, error) {
	counts := make(map[string]int, len(sessionIDs))
	if len(sessionIDs) == 0 || strings.TrimSpace(f.Model) == "" {
		return counts, nil
	}

	allowedModels := make(map[string]struct{})
	for _, model := range duckAnalyticsCSVValues(f.Model) {
		allowedModels[model] = struct{}{}
	}
	loc := analyticsLocation(f.Timezone)
	err := duckQueryChunked(sessionIDs, func(chunk []string) error {
		ph, args := duckInPlaceholders(chunk)
		rows, err := s.queryContext(ctx, `
			SELECT tc.session_id, m.model, m.timestamp, COUNT(*)
			FROM tool_calls tc
			JOIN messages m
				ON m.session_id = tc.session_id
				AND m.id = tc.message_id
			WHERE tc.session_id IN `+ph+`
			GROUP BY tc.session_id, m.model, m.timestamp`, args...)
		if err != nil {
			return fmt.Errorf(
				"querying duckdb filtered analytics tool calls: %w",
				err,
			)
		}
		defer rows.Close()

		for rows.Next() {
			var sessionID, model string
			var ts any
			var count int
			if err := rows.Scan(&sessionID, &model, &ts, &count); err != nil {
				return fmt.Errorf(
					"scanning duckdb filtered analytics tool calls: %w",
					err,
				)
			}
			if _, ok := allowedModels[model]; !ok {
				continue
			}
			if f.HasTimeFilter() {
				t, ok := parseAnalyticsTime(formatDBTime(ts))
				if !ok || !duckAnalyticsTimeMatches(t.In(loc), f) {
					continue
				}
			}
			counts[sessionID] += count
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}

	return counts, nil
}

func (s *Store) getAnalyticsActivityFilteredByModelTime(
	ctx context.Context, f db.AnalyticsFilter, granularity string,
) (db.ActivityResponse, error) {
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.ActivityResponse{}, err
	}
	sessionIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		sessionIDs = append(sessionIDs, session.id)
	}
	messageStats, err := s.getAnalyticsFilteredMessageStats(
		ctx, sessionIDs, f,
	)
	if err != nil {
		return db.ActivityResponse{}, err
	}
	toolCounts, err := s.getAnalyticsFilteredToolCallCounts(
		ctx, sessionIDs, f,
	)
	if err != nil {
		return db.ActivityResponse{}, err
	}

	out := db.ActivityResponse{Granularity: granularity}
	buckets := map[string]*db.ActivityEntry{}
	for _, session := range sessions {
		date := bucketAnalyticsDate(
			analyticsLocalDate(analyticsDateTime(session), f.Timezone),
			granularity,
		)
		entry := buckets[date]
		if entry == nil {
			entry = &db.ActivityEntry{
				Date:    date,
				ByAgent: map[string]int{},
			}
			buckets[date] = entry
		}
		entry.Sessions++
		stat := messageStats[session.id]
		entry.Messages += stat.Messages
		entry.UserMessages += stat.UserMessages
		entry.AssistantMessages += stat.AssistantMessages
		entry.ThinkingMessages += stat.ThinkingMessages
		entry.ToolCalls += toolCounts[session.id]
		entry.ByAgent[session.agent] += stat.Messages
	}

	for _, key := range sortedKeys(buckets) {
		entry := buckets[key]
		if entry == nil {
			continue
		}
		out.Series = append(out.Series, *entry)
	}
	return out, nil
}

func (s *Store) GetAnalyticsActivity(
	ctx context.Context, f db.AnalyticsFilter, granularity string,
) (db.ActivityResponse, error) {
	if granularity == "" {
		granularity = "day"
	}
	if strings.TrimSpace(f.Model) != "" {
		return s.getAnalyticsActivityFilteredByModelTime(
			ctx, f, granularity,
		)
	}
	buckets, err := s.queryActivityBuckets(ctx, f, granularity)
	if err != nil {
		return db.ActivityResponse{}, err
	}
	if err := s.addActivityAgentCounts(ctx, f, granularity, buckets); err != nil {
		return db.ActivityResponse{}, err
	}
	out := db.ActivityResponse{Granularity: granularity}
	keys := sortedKeys(buckets)
	for _, key := range keys {
		entry, ok := buckets[key]
		if !ok || entry == nil {
			continue
		}
		out.Series = append(out.Series, *entry)
	}
	return out, nil
}

func (s *Store) queryActivityBuckets(
	ctx context.Context, f db.AnalyticsFilter, granularity string,
) (map[string]*db.ActivityEntry, error) {
	where, args := duckBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	localDate, localDateArgs := duckAnalyticsLocalDateExpr(
		"COALESCE(s.started_at, s.created_at)", f)
	bucketExpr := duckAnalyticsBucketExpr("local_date", granularity)
	queryArgs := append([]any{}, localDateArgs...)
	queryArgs = append(queryArgs, args...)
	if _, modelArgs := duckAnalyticsCSVPredicate("m.model", f.Model); len(modelArgs) > 0 {
		queryArgs = append(queryArgs, modelArgs...)
		queryArgs = append(queryArgs, modelArgs...)
	}
	rows, err := s.queryContext(ctx, `
		WITH filtered_sessions AS (
			SELECT s.id, s.message_count, `+localDate+` AS local_date
			FROM sessions s
			WHERE `+where+`
		),
		session_rows AS (
			SELECT `+bucketExpr+` AS bucket,
				COUNT(*) AS sessions
			FROM filtered_sessions
			GROUP BY bucket
		),
		message_rows AS (
			SELECT `+bucketExpr+` AS bucket,
				COUNT(*) AS messages,
				COUNT(*) FILTER (WHERE m.role = 'user' AND m.is_system = FALSE) AS user_messages,
				COUNT(*) FILTER (WHERE m.role = 'assistant') AS assistant_messages,
				COUNT(*) FILTER (WHERE m.has_thinking = TRUE) AS thinking_messages
			FROM filtered_sessions fs
			JOIN messages m ON m.session_id = fs.id
			`+duckAnalyticsMessageFilterClause("m.model", f.Model)+`
			GROUP BY bucket
		),
		tool_rows AS (
			SELECT `+bucketExpr+` AS bucket, COUNT(*) AS tool_calls
			FROM filtered_sessions fs
			JOIN tool_calls tc ON tc.session_id = fs.id
			`+duckAnalyticsToolMessageJoin("tc", f.Model)+`
			`+duckAnalyticsMessageFilterClause("m.model", f.Model)+`
			GROUP BY bucket
		)
		SELECT COALESCE(sr.bucket, mr.bucket, tr.bucket) AS bucket,
			COALESCE(sr.sessions, 0) AS sessions,
			COALESCE(mr.messages, 0) AS messages,
			COALESCE(mr.user_messages, 0) AS user_messages,
			COALESCE(mr.assistant_messages, 0) AS assistant_messages,
			COALESCE(mr.thinking_messages, 0) AS thinking_messages,
			COALESCE(tr.tool_calls, 0) AS tool_calls
		FROM session_rows sr
		FULL OUTER JOIN message_rows mr USING (bucket)
		FULL OUTER JOIN tool_rows tr
			ON tr.bucket = COALESCE(sr.bucket, mr.bucket)
		ORDER BY bucket`,
		queryArgs...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb analytics activity buckets: %w", err)
	}
	defer rows.Close()
	buckets := map[string]*db.ActivityEntry{}
	for rows.Next() {
		entry := db.ActivityEntry{ByAgent: map[string]int{}}
		if err := rows.Scan(
			&entry.Date,
			&entry.Sessions,
			&entry.Messages,
			&entry.UserMessages,
			&entry.AssistantMessages,
			&entry.ThinkingMessages,
			&entry.ToolCalls,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb analytics activity bucket: %w", err)
		}
		buckets[entry.Date] = &entry
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb analytics activity buckets: %w", err)
	}
	return buckets, nil
}

func (s *Store) addActivityAgentCounts(
	ctx context.Context, f db.AnalyticsFilter, granularity string,
	buckets map[string]*db.ActivityEntry,
) error {
	where, args := duckBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	localDate, localDateArgs := duckAnalyticsLocalDateExpr(
		"COALESCE(s.started_at, s.created_at)", f)
	bucketExpr := duckAnalyticsBucketExpr("local_date", granularity)
	queryArgs := append([]any{}, localDateArgs...)
	queryArgs = append(queryArgs, args...)
	if _, modelArgs := duckAnalyticsCSVPredicate("m.model", f.Model); len(modelArgs) > 0 {
		queryArgs = append(queryArgs, modelArgs...)
	}
	rows, err := s.queryContext(ctx, `
		WITH filtered_sessions AS (
			SELECT s.id, s.agent, `+localDate+` AS local_date
			FROM sessions s
			WHERE `+where+`
		)
		SELECT `+bucketExpr+` AS bucket, fs.agent, COUNT(*) AS messages
		FROM filtered_sessions fs
		JOIN messages m ON m.session_id = fs.id
		`+duckAnalyticsMessageFilterClause("m.model", f.Model)+`
		GROUP BY bucket, fs.agent
		ORDER BY bucket, fs.agent`,
		queryArgs...,
	)
	if err != nil {
		return fmt.Errorf("querying duckdb analytics activity agents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bucket, agent string
		var count int
		if err := rows.Scan(&bucket, &agent, &count); err != nil {
			return fmt.Errorf("scanning duckdb analytics activity agent: %w", err)
		}
		if entry, ok := buckets[bucket]; ok {
			entry.ByAgent[agent] = count
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating duckdb analytics activity agents: %w", err)
	}
	return nil
}

func bucketAnalyticsDate(date, granularity string) string {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	switch granularity {
	case "week":
		dow := int(t.Weekday())
		if dow == 0 {
			dow = 7
		}
		return t.AddDate(0, 0, -(dow - 1)).Format("2006-01-02")
	case "month":
		return t.Format("2006-01") + "-01"
	default:
		return date
	}
}

func duckAnalyticsBucketExpr(dateExpr, granularity string) string {
	switch granularity {
	case "week":
		return "strftime(date_trunc('week', CAST(" + dateExpr + " AS DATE)), '%Y-%m-%d')"
	case "month":
		return "strftime(date_trunc('month', CAST(" + dateExpr + " AS DATE)), '%Y-%m-%d')"
	default:
		return dateExpr
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func duckAnalyticsMessageFilterClause(col, raw string) string {
	pred, _ := duckAnalyticsCSVPredicate(col, raw)
	if pred == "" {
		return ""
	}
	return "WHERE " + pred
}

func duckAnalyticsAndClause(pred string) string {
	if pred == "" {
		return ""
	}
	return " AND " + pred
}

func duckAnalyticsToolMessageJoin(
	toolAlias string, model string,
) string {
	if model == "" {
		return ""
	}
	return `
			JOIN messages m
				ON m.session_id = ` + toolAlias + `.session_id
				AND m.id = ` + toolAlias + `.message_id`
}

func (s *Store) GetAnalyticsHeatmap(
	ctx context.Context, f db.AnalyticsFilter, metric string,
) (db.HeatmapResponse, error) {
	if metric == "" {
		metric = "messages"
	}
	if strings.TrimSpace(f.Model) != "" &&
		(metric == "messages" || metric == "output_tokens" ||
			metric == "sessions") {
		sessions, err := s.analyticsSessionsWithModelMessageCounts(ctx, f)
		if err != nil {
			return db.HeatmapResponse{}, err
		}
		counts := map[string]int{}
		for _, session := range sessions {
			date := analyticsLocalDate(analyticsDateTime(session), f.Timezone)
			switch metric {
			case "sessions":
				counts[date]++
			case "output_tokens":
				if session.hasTotalOutputTokens {
					counts[date] += session.totalOutputTokens
				}
			default:
				counts[date] += session.messageCount
			}
		}
		entriesFrom := duckClampHeatmapFrom(f.From, f.To)
		values := []int{}
		for date, v := range counts {
			if v > 0 && date >= entriesFrom && date <= f.To {
				values = append(values, v)
			}
		}
		sort.Ints(values)
		levels := duckComputeHeatmapLevels(values)
		entries := duckBuildHeatmapEntries(entriesFrom, f.To, counts, levels)
		if metric == "output_tokens" && len(counts) == 0 {
			return db.HeatmapResponse{
				Metric:      metric,
				EntriesFrom: entriesFrom,
			}, nil
		}
		return db.HeatmapResponse{
			Metric: metric, Entries: entries,
			Levels:      levels,
			EntriesFrom: entriesFrom,
		}, nil
	}
	where, args := duckBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	localDate, localDateArgs := duckAnalyticsLocalDateExpr(
		"COALESCE(s.started_at, s.created_at)", f)
	valueExpr := "COALESCE(SUM(s.message_count), 0)"
	switch metric {
	case "sessions":
		valueExpr = "COUNT(*)"
	case "output_tokens":
		where += " AND s.has_total_output_tokens = TRUE"
		valueExpr = "COALESCE(SUM(s.total_output_tokens), 0)"
	}
	queryArgs := append([]any{}, localDateArgs...)
	queryArgs = append(queryArgs, args...)
	rows, err := s.queryContext(ctx, `
		SELECT `+localDate+` AS local_date, `+valueExpr+` AS value
		FROM sessions s
		WHERE `+where+`
		GROUP BY local_date
		ORDER BY local_date`,
		queryArgs...,
	)
	if err != nil {
		return db.HeatmapResponse{}, fmt.Errorf("querying duckdb analytics heatmap: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var date string
		var value int
		if err := rows.Scan(&date, &value); err != nil {
			return db.HeatmapResponse{}, fmt.Errorf("scanning duckdb analytics heatmap: %w", err)
		}
		counts[date] = value
	}
	if err := rows.Err(); err != nil {
		return db.HeatmapResponse{}, fmt.Errorf("iterating duckdb analytics heatmap: %w", err)
	}
	if metric == "output_tokens" && len(counts) == 0 {
		return db.HeatmapResponse{
			Metric:      metric,
			EntriesFrom: duckClampHeatmapFrom(f.From, f.To),
		}, nil
	}
	entriesFrom := duckClampHeatmapFrom(f.From, f.To)
	values := []int{}
	for date, v := range counts {
		if v > 0 && date >= entriesFrom && date <= f.To {
			values = append(values, v)
		}
	}
	sort.Ints(values)
	levels := duckComputeHeatmapLevels(values)
	entries := duckBuildHeatmapEntries(entriesFrom, f.To, counts, levels)
	return db.HeatmapResponse{
		Metric: metric, Entries: entries,
		Levels:      levels,
		EntriesFrom: entriesFrom,
	}, nil
}

const duckMaxHeatmapDays = 366

func duckClampHeatmapFrom(from, to string) string {
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return from
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return from
	}
	earliest := end.AddDate(0, 0, -(duckMaxHeatmapDays - 1))
	if start.Before(earliest) {
		return earliest.Format("2006-01-02")
	}
	return from
}

func duckComputeHeatmapLevels(sorted []int) db.HeatmapLevels {
	if len(sorted) == 0 {
		return db.HeatmapLevels{L1: 1, L2: 2, L3: 3, L4: 4}
	}
	n := len(sorted)
	return db.HeatmapLevels{
		L1: sorted[0],
		L2: sorted[n/4],
		L3: sorted[n/2],
		L4: sorted[n*3/4],
	}
}

func duckHeatmapLevel(value int, levels db.HeatmapLevels) int {
	if value <= 0 {
		return 0
	}
	if value <= levels.L2 {
		return 1
	}
	if value <= levels.L3 {
		return 2
	}
	if value <= levels.L4 {
		return 3
	}
	return 4
}

func duckBuildHeatmapEntries(
	from, to string, values map[string]int, levels db.HeatmapLevels,
) []db.HeatmapEntry {
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return nil
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return nil
	}
	entries := []db.HeatmapEntry{}
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		date := d.Format("2006-01-02")
		v := values[date]
		entries = append(entries, db.HeatmapEntry{
			Date:  date,
			Value: v,
			Level: duckHeatmapLevel(v, levels),
		})
	}
	return entries
}

func (s *Store) GetAnalyticsProjects(
	ctx context.Context, f db.AnalyticsFilter,
) (db.ProjectsAnalyticsResponse, error) {
	// Per-project aggregate: count subagent sessions (mirrors SQLite).
	f.IncludeSubagents = true
	sessions, err := s.analyticsSessionsWithModelMessageCounts(ctx, f)
	if err != nil {
		return db.ProjectsAnalyticsResponse{}, err
	}
	type acc struct {
		row    db.ProjectAnalytics
		counts []int
		days   map[string]int
	}
	byProject := map[string]*acc{}
	for _, r := range sessions {
		a := byProject[r.project]
		if a == nil {
			a = &acc{
				row:  db.ProjectAnalytics{Name: r.project, Agents: map[string]int{}},
				days: map[string]int{},
			}
			byProject[r.project] = a
		}
		date := analyticsLocalDate(analyticsDateTime(r), f.Timezone)
		if a.row.FirstSession == "" || date < a.row.FirstSession {
			a.row.FirstSession = date
		}
		if date > a.row.LastSession {
			a.row.LastSession = date
		}
		a.row.Sessions++
		a.row.Messages += r.messageCount
		a.row.Agents[r.agent]++
		a.counts = append(a.counts, r.messageCount)
		a.days[date] += r.messageCount
	}
	resp := db.ProjectsAnalyticsResponse{}
	for _, name := range sortedKeys(byProject) {
		a, ok := byProject[name]
		if !ok || a == nil {
			continue
		}
		sort.Ints(a.counts)
		a.row.AvgMessages = round1(float64(a.row.Messages) / float64(a.row.Sessions))
		a.row.MedianMessages = median(a.counts)
		if len(a.days) > 0 {
			a.row.DailyTrend = round1(float64(a.row.Messages) / float64(len(a.days)))
		}
		resp.Projects = append(resp.Projects, a.row)
	}
	sort.Slice(resp.Projects, func(i, j int) bool {
		if resp.Projects[i].Messages != resp.Projects[j].Messages {
			return resp.Projects[i].Messages > resp.Projects[j].Messages
		}
		return resp.Projects[i].Name < resp.Projects[j].Name
	})
	return resp, nil
}

func (s *Store) GetAnalyticsHourOfWeek(
	ctx context.Context, f db.AnalyticsFilter,
) (db.HourOfWeekResponse, error) {
	if strings.TrimSpace(f.Model) != "" {
		return s.getAnalyticsHourOfWeekFilteredByModel(ctx, f)
	}
	sessionFilter := f
	sessionFilter.DayOfWeek = nil
	sessionFilter.Hour = nil
	where, args := duckBuildAnalyticsWhere(
		sessionFilter, "COALESCE(s.started_at, s.created_at)", "s.", true, false)
	localTime, localTimeArgs := duckAnalyticsLocalTimeExpr("m.timestamp", f)
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, localTimeArgs...)
	rows, err := s.queryContext(ctx, `
		WITH filtered_sessions AS (
			SELECT s.id
			FROM sessions s
			WHERE `+where+`
		),
		message_times AS (
			SELECT `+localTime+` AS local_ts
			FROM messages m
			JOIN filtered_sessions fs ON fs.id = m.session_id
			WHERE m.timestamp IS NOT NULL
		),
		message_buckets AS (
			SELECT ((CAST(strftime(local_ts, '%w') AS INTEGER) + 6) % 7) AS day_of_week,
				CAST(strftime(local_ts, '%H') AS INTEGER) AS hour
			FROM message_times
			WHERE local_ts IS NOT NULL
		)
		SELECT day_of_week, hour, COUNT(*)
		FROM message_buckets
		GROUP BY day_of_week, hour
		ORDER BY day_of_week, hour`,
		queryArgs...,
	)
	if err != nil {
		return db.HourOfWeekResponse{}, fmt.Errorf("querying duckdb analytics hour-of-week: %w", err)
	}
	defer rows.Close()
	var grid [7][24]int
	for rows.Next() {
		var day, hour, messages int
		if err := rows.Scan(&day, &hour, &messages); err != nil {
			return db.HourOfWeekResponse{}, fmt.Errorf("scanning duckdb analytics hour-of-week: %w", err)
		}
		grid[day][hour] = messages
	}
	if err := rows.Err(); err != nil {
		return db.HourOfWeekResponse{}, fmt.Errorf("iterating duckdb analytics hour-of-week: %w", err)
	}
	return db.HourOfWeekResponseFromGrid(grid), nil
}

// getAnalyticsHourOfWeekFilteredByModel buckets model-scoped messages by
// day-of-week and hour. It pairs empty-model user turns with their
// selected-model assistant via the shared scope reducer, so those turns appear
// in the heatmap consistently with the summary, activity, velocity, and trends
// panels. The heatmap is the control that sets the day/hour filter, so it
// clears DayOfWeek/Hour before scoping to keep showing the full grid, matching
// the no-model path.
func (s *Store) getAnalyticsHourOfWeekFilteredByModel(
	ctx context.Context, f db.AnalyticsFilter,
) (db.HourOfWeekResponse, error) {
	sessions, err := s.analyticsSessionsFiltered(ctx, f, true, false)
	if err != nil {
		return db.HourOfWeekResponse{}, err
	}
	sessionIDs := make([]string, 0, len(sessions))
	for _, session := range sessions {
		sessionIDs = append(sessionIDs, session.id)
	}

	scopeFilter := f
	scopeFilter.DayOfWeek = nil
	scopeFilter.Hour = nil
	scope, err := s.resolveAnalyticsMessageScope(
		ctx, sessionIDs, scopeFilter, false,
	)
	if err != nil {
		return db.HourOfWeekResponse{}, err
	}

	var grid [7][24]int
	if scope != nil {
		for _, msgs := range scope.MessagesBySession() {
			for _, m := range msgs {
				if !m.HasLocalTime {
					continue
				}
				dow := (int(m.LocalTime.Weekday()) + 6) % 7
				grid[dow][m.LocalTime.Hour()]++
			}
		}
	}

	return db.HourOfWeekResponseFromGrid(grid), nil
}

func (s *Store) GetAnalyticsSessionShape(
	ctx context.Context, f db.AnalyticsFilter,
) (db.SessionShapeResponse, error) {
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.SessionShapeResponse{}, err
	}
	modelFilter := strings.TrimSpace(f.Model) != ""
	lengths := map[string]int{}
	durations := map[string]int{}
	ids := []string{}
	for _, r := range sessions {
		ids = append(ids, r.id)
		if !modelFilter {
			lengths[lengthBucket(r.messageCount)]++
		}
		if start, okS := parseAnalyticsTime(r.startedAt); okS {
			if end, okE := parseAnalyticsTime(r.endedAt); okE && !end.Before(start) {
				durations[durationBucket(end.Sub(start).Minutes())]++
			}
		}
	}
	autonomy := map[string]int{}
	if modelFilter && len(ids) > 0 {
		stats, err := s.getAnalyticsFilteredMessageStats(ctx, ids, f)
		if err != nil {
			return db.SessionShapeResponse{}, err
		}
		lengths = map[string]int{}
		for _, r := range sessions {
			stat := stats[r.id]
			lengths[lengthBucket(stat.Messages)]++
			if stat.UserMessages > 0 {
				ratio := float64(stat.ToolUseMessages) /
					float64(stat.UserMessages)
				autonomy[autonomyBucket(ratio)]++
			}
		}
	} else {
		autonomy, err = s.analyticsAutonomyBuckets(ctx, ids)
		if err != nil {
			return db.SessionShapeResponse{}, err
		}
	}
	return db.SessionShapeResponse{
		Count:                len(sessions),
		LengthDistribution:   mapBuckets(lengths, lengthOrder()),
		DurationDistribution: mapBuckets(durations, durationOrder()),
		AutonomyDistribution: mapBuckets(autonomy, autonomyOrder()),
	}, nil
}

func lengthBucket(mc int) string {
	switch {
	case mc <= 5:
		return "1-5"
	case mc <= 15:
		return "6-15"
	case mc <= 30:
		return "16-30"
	case mc <= 60:
		return "31-60"
	case mc <= 120:
		return "61-120"
	default:
		return "121+"
	}
}

func durationBucket(mins float64) string {
	switch {
	case mins < 5:
		return "<5m"
	case mins < 15:
		return "5-15m"
	case mins < 30:
		return "15-30m"
	case mins < 60:
		return "30-60m"
	case mins < 120:
		return "1-2h"
	default:
		return "2h+"
	}
}

func lengthOrder() map[string]int {
	return map[string]int{"1-5": 0, "6-15": 1, "16-30": 2, "31-60": 3, "61-120": 4, "121+": 5}
}

func durationOrder() map[string]int {
	return map[string]int{"<5m": 0, "5-15m": 1, "15-30m": 2, "30-60m": 3, "1-2h": 4, "2h+": 5}
}

func autonomyBucket(ratio float64) string {
	switch {
	case ratio < 0.5:
		return "<0.5"
	case ratio < 1:
		return "0.5-1"
	case ratio < 2:
		return "1-2"
	case ratio < 5:
		return "2-5"
	case ratio < 10:
		return "5-10"
	default:
		return "10+"
	}
}

func autonomyOrder() map[string]int {
	return map[string]int{"<0.5": 0, "0.5-1": 1, "1-2": 2, "2-5": 3, "5-10": 4, "10+": 5}
}

func mapBuckets(values map[string]int, order map[string]int) []db.DistributionBucket {
	out := make([]db.DistributionBucket, 0, len(values))
	for label, count := range values {
		out = append(out, db.DistributionBucket{Label: label, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		return order[out[i].Label] < order[out[j].Label]
	})
	return out
}

func (s *Store) analyticsAutonomyBuckets(
	ctx context.Context, sessionIDs []string,
) (map[string]int, error) {
	counts := map[string]int{}
	if len(sessionIDs) == 0 {
		return counts, nil
	}
	args := make([]any, len(sessionIDs))
	placeholders := make([]string, len(sessionIDs))
	for i, id := range sessionIDs {
		args[i] = id
		placeholders[i] = "?"
	}
	rows, err := s.queryContext(ctx, `
		SELECT session_id,
			SUM(CASE WHEN role = 'user' AND is_system = FALSE THEN 1 ELSE 0 END) AS user_count,
			SUM(CASE WHEN role = 'assistant' AND has_tool_use = TRUE THEN 1 ELSE 0 END) AS tool_count
		FROM messages
		WHERE session_id IN (`+strings.Join(placeholders, ",")+`)
		GROUP BY session_id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb autonomy: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var userCount, toolCount int
		if err := rows.Scan(&sessionID, &userCount, &toolCount); err != nil {
			return nil, fmt.Errorf("scanning duckdb autonomy: %w", err)
		}
		if userCount > 0 {
			counts[autonomyBucket(float64(toolCount)/float64(userCount))]++
		}
	}
	return counts, rows.Err()
}

// duckMaxSQLVars bounds the IN-list size per query to stay well under
// driver bind-variable limits; larger ID sets are split into chunks.
const duckMaxSQLVars = 900

func duckInPlaceholders(ids []string) (string, []any) {
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	return "(" + strings.Join(ph, ",") + ")", args
}

func duckQueryChunked(ids []string, fn func(chunk []string) error) error {
	for i := 0; i < len(ids); i += duckMaxSQLVars {
		end := min(i+duckMaxSQLVars, len(ids))
		if err := fn(ids[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetAnalyticsTools(
	ctx context.Context, f db.AnalyticsFilter,
) (db.ToolsAnalyticsResponse, error) {
	sessions, err := s.analyticsSessionsFiltered(
		ctx, f, false, false,
	)
	if err != nil {
		return db.ToolsAnalyticsResponse{}, err
	}
	meta := map[string]duckAnalyticsSession{}
	var ids []string
	for _, r := range sessions {
		meta[r.id] = r
		ids = append(ids, r.id)
	}
	if len(ids) == 0 {
		return db.BuildToolsAnalytics(nil), nil
	}
	var toolRows []db.ToolAnalyticsRow
	err = duckQueryChunked(ids, func(chunk []string) error {
		ph, args := duckInPlaceholders(chunk)
		modelPred, modelArgs := duckAnalyticsCSVPredicate("m.model", f.Model)
		args = append(args, modelArgs...)
		query := `SELECT tc.session_id, tc.category,
				TRIM(COALESCE(tc.tool_name, '')), COUNT(*),
				m.timestamp
				FROM tool_calls tc
				LEFT JOIN messages m
					ON m.session_id = tc.session_id
					AND m.id = tc.message_id
				WHERE tc.session_id IN ` + ph
		if modelPred != "" {
			query += `
				AND ` + modelPred
		}
		query += `
				GROUP BY tc.session_id, tc.category,
					TRIM(COALESCE(tc.tool_name, '')), m.timestamp`
		rows, qErr := s.queryContext(ctx, query, args...)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			var sid, cat, toolName string
			var ts any
			var count int
			if err := rows.Scan(&sid, &cat, &toolName, &count, &ts); err != nil {
				return err
			}
			r, ok := meta[sid]
			if !ok {
				continue
			}
			_, date, keep := f.ResolveSkillRowTime(
				formatDBTime(ts), analyticsDateTime(r),
			)
			if !keep {
				continue
			}
			toolRows = append(toolRows, db.ToolAnalyticsRow{
				SessionID: sid,
				Category:  cat,
				ToolName:  toolName,
				Agent:     r.agent,
				Count:     count,
				Date:      date,
			})
		}
		return rows.Err()
	})
	if err != nil {
		return db.ToolsAnalyticsResponse{}, err
	}
	return db.BuildToolsAnalytics(toolRows), nil
}

// GetAnalyticsSkills returns skill usage analytics. granularity picks
// the trend bucket size (day, week, or month); empty defaults to week.
func (s *Store) GetAnalyticsSkills(
	ctx context.Context, f db.AnalyticsFilter, granularity string,
) (db.SkillsAnalyticsResponse, error) {
	sessions, err := s.analyticsSessionsFiltered(ctx, f, false, false)
	if err != nil {
		return db.SkillsAnalyticsResponse{}, err
	}
	meta := map[string]duckAnalyticsSession{}
	var ids []string
	for _, r := range sessions {
		meta[r.id] = r
		ids = append(ids, r.id)
	}
	if len(ids) == 0 {
		return db.BuildSkillsAnalytics(
			nil, f.From, f.To, granularity,
		), nil
	}

	var skillRows []db.SkillAnalyticsRow
	err = duckQueryChunked(ids, func(chunk []string) error {
		ph, args := duckInPlaceholders(chunk)
		modelPred, modelArgs := duckAnalyticsCSVPredicate("m.model", f.Model)
		args = append(args, modelArgs...)
		rows, qErr := s.queryContext(ctx,
			`SELECT tc.session_id, TRIM(COALESCE(tc.skill_name, '')),
				COUNT(*), m.timestamp
				FROM tool_calls tc
				LEFT JOIN messages m
					ON m.session_id = tc.session_id
					AND m.id = tc.message_id
				WHERE tc.session_id IN `+ph+`
					AND TRIM(COALESCE(tc.skill_name, '')) != ''
					`+duckAnalyticsAndClause(modelPred)+`
				GROUP BY tc.session_id, TRIM(COALESCE(tc.skill_name, '')),
					m.timestamp`, args...)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		for rows.Next() {
			var sid, skill string
			var count int
			var msgTS any
			if err := rows.Scan(&sid, &skill, &count, &msgTS); err != nil {
				return err
			}
			r, ok := meta[sid]
			if !ok {
				continue
			}
			usedTS, date, keep := f.ResolveSkillRowTime(
				formatDBTime(msgTS), analyticsDateTime(r),
			)
			if !keep {
				continue
			}
			skillRows = append(skillRows, db.SkillAnalyticsRow{
				SessionID:  sid,
				SkillName:  skill,
				Agent:      r.agent,
				Project:    r.project,
				Date:       date,
				LastUsedAt: usedTS,
				Count:      count,
			})
		}
		return rows.Err()
	})
	if err != nil {
		return db.SkillsAnalyticsResponse{}, err
	}
	return db.BuildSkillsAnalytics(
		skillRows, f.From, f.To, granularity,
	), nil
}

func (s *Store) GetAnalyticsVelocity(
	ctx context.Context, f db.AnalyticsFilter,
) (db.VelocityResponse, error) {
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.VelocityResponse{}, err
	}
	if len(sessions) == 0 {
		return db.VelocityResponse{
			ByAgent:      []db.VelocityBreakdown{},
			ByComplexity: []db.VelocityBreakdown{},
		}, nil
	}

	sessionIDs := make([]string, 0, len(sessions))
	sessionInfo := make(map[string]duckVelocitySession, len(sessions))
	for _, sess := range sessions {
		sessionIDs = append(sessionIDs, sess.id)
		sessionInfo[sess.id] = duckVelocitySession{
			agent: sess.agent,
			mc:    sess.messageCount,
		}
	}
	if strings.TrimSpace(f.Model) != "" {
		stats, err := s.getAnalyticsFilteredMessageStats(
			ctx, sessionIDs, f,
		)
		if err != nil {
			return db.VelocityResponse{}, err
		}
		for _, sid := range sessionIDs {
			info := sessionInfo[sid]
			info.mc = stats[sid].Messages
			sessionInfo[sid] = info
		}
	}

	var sessionMsgs map[string][]duckVelocityMsg
	if strings.TrimSpace(f.Model) != "" {
		sessionMsgs, err = s.filteredVelocityMessages(
			ctx, sessionIDs, f,
		)
	} else {
		sessionMsgs, err = s.velocityMessages(
			ctx, sessionIDs, analyticsLocation(f.Timezone),
		)
	}
	if err != nil {
		return db.VelocityResponse{}, err
	}
	var toolCounts map[string]int
	if strings.TrimSpace(f.Model) != "" {
		toolCounts, err = s.getAnalyticsFilteredToolCallCounts(
			ctx, sessionIDs, f,
		)
	} else {
		toolCounts, err = s.velocityToolCounts(ctx, sessionIDs)
	}
	if err != nil {
		return db.VelocityResponse{}, err
	}

	overall := &duckVelocityAccumulator{}
	byAgent := make(map[string]*duckVelocityAccumulator)
	byComplexity := make(map[string]*duckVelocityAccumulator)
	for _, sid := range sessionIDs {
		msgs := sessionMsgs[sid]
		if len(msgs) < 2 {
			continue
		}
		info := sessionInfo[sid]
		agentKey := info.agent
		compKey := duckComplexityBucket(info.mc)
		if byAgent[agentKey] == nil {
			byAgent[agentKey] = &duckVelocityAccumulator{}
		}
		if byComplexity[compKey] == nil {
			byComplexity[compKey] = &duckVelocityAccumulator{}
		}
		processDuckSessionVelocity(
			[]*duckVelocityAccumulator{overall, byAgent[agentKey], byComplexity[compKey]},
			msgs,
			toolCounts[sid],
		)
	}

	resp := db.VelocityResponse{
		Overall:      overall.computeOverview(),
		ByAgent:      []db.VelocityBreakdown{},
		ByComplexity: []db.VelocityBreakdown{},
	}
	for _, key := range sortedKeys(byAgent) {
		acc := byAgent[key]
		if acc == nil {
			continue
		}
		resp.ByAgent = append(resp.ByAgent, db.VelocityBreakdown{
			Label:    key,
			Sessions: acc.sessions,
			Overview: acc.computeOverview(),
		})
	}

	compOrder := map[string]int{"1-15": 0, "16-60": 1, "61+": 2}
	compKeys := sortedKeys(byComplexity)
	sort.Slice(compKeys, func(i, j int) bool {
		return compOrder[compKeys[i]] < compOrder[compKeys[j]]
	})
	for _, key := range compKeys {
		acc := byComplexity[key]
		if acc == nil {
			continue
		}
		resp.ByComplexity = append(resp.ByComplexity, db.VelocityBreakdown{
			Label:    key,
			Sessions: acc.sessions,
			Overview: acc.computeOverview(),
		})
	}
	return resp, nil
}

type duckVelocitySession struct {
	agent string
	mc    int
}

type duckVelocityMsg struct {
	role          string
	ts            time.Time
	valid         bool
	contentLength int
}

type duckVelocityAccumulator struct {
	turnCycles     []float64
	firstResponses []float64
	totalMsgs      int
	totalChars     int
	totalToolCalls int
	activeMinutes  float64
	sessions       int
}

func (s *Store) velocityMessages(
	ctx context.Context,
	sessionIDs []string,
	loc *time.Location,
) (map[string][]duckVelocityMsg, error) {
	out := make(map[string][]duckVelocityMsg, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	args, placeholders := stringInArgs(sessionIDs)
	rows, err := s.queryContext(ctx, `
		SELECT session_id, ordinal, role, timestamp, content_length
		FROM messages
		WHERE session_id IN (`+strings.Join(placeholders, ",")+`)
		ORDER BY session_id, ordinal`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb velocity messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid, role string
		var ordinal int
		var ts any
		var contentLength int
		if err := rows.Scan(&sid, &ordinal, &role, &ts, &contentLength); err != nil {
			return nil, fmt.Errorf("scanning duckdb velocity message: %w", err)
		}
		parsed, ok := duckLocalTime(formatDBTime(ts), loc)
		out[sid] = append(out[sid], duckVelocityMsg{
			role:          role,
			ts:            parsed,
			valid:         ok,
			contentLength: contentLength,
		})
	}
	return out, rows.Err()
}

func (s *Store) filteredVelocityMessages(
	ctx context.Context,
	sessionIDs []string,
	f db.AnalyticsFilter,
) (map[string][]duckVelocityMsg, error) {
	out := make(map[string][]duckVelocityMsg, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}

	scope, err := s.resolveAnalyticsMessageScope(ctx, sessionIDs, f, false)
	if err != nil {
		return nil, err
	}
	if scope == nil {
		return out, nil
	}
	for sessionID, rows := range scope.TimingBySession() {
		for _, row := range rows {
			out[sessionID] = append(out[sessionID], duckVelocityMsg{
				role:          row.Role,
				ts:            row.Time,
				valid:         row.Valid,
				contentLength: row.ContentLength,
			})
		}
	}
	return out, nil
}

func (s *Store) velocityToolCounts(
	ctx context.Context,
	sessionIDs []string,
) (map[string]int, error) {
	out := make(map[string]int, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	args, placeholders := stringInArgs(sessionIDs)
	rows, err := s.queryContext(ctx, `
		SELECT session_id, COUNT(*)
		FROM tool_calls
		WHERE session_id IN (`+strings.Join(placeholders, ",")+`)
		GROUP BY session_id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb velocity tool calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var count int
		if err := rows.Scan(&sid, &count); err != nil {
			return nil, fmt.Errorf("scanning duckdb velocity tool call count: %w", err)
		}
		out[sid] = count
	}
	return out, rows.Err()
}

func stringInArgs(values []string) ([]any, []string) {
	args := make([]any, len(values))
	placeholders := make([]string, len(values))
	for i, value := range values {
		args[i] = value
		placeholders[i] = "?"
	}
	return args, placeholders
}

func duckLocalTime(ts string, loc *time.Location) (time.Time, bool) {
	t, ok := parseAnalyticsTime(ts)
	if !ok {
		return time.Time{}, false
	}
	return t.In(loc), true
}

func duckComplexityBucket(mc int) string {
	switch {
	case mc <= 15:
		return "1-15"
	case mc <= 60:
		return "16-60"
	default:
		return "61+"
	}
}

func processDuckSessionVelocity(
	accums []*duckVelocityAccumulator,
	msgs []duckVelocityMsg,
	toolCount int,
) {
	const maxCycleSec = 1800.0
	// Shared with the Top Sessions "active duration" SQL so the two
	// "active" definitions stay in lockstep.
	const maxGapSec = db.ActiveGapCapSec

	for _, acc := range accums {
		acc.sessions++
	}
	for i := 1; i < len(msgs); i++ {
		prev := msgs[i-1]
		cur := msgs[i]
		if !prev.valid || !cur.valid {
			continue
		}
		if prev.role == "user" && cur.role == "assistant" {
			delta := cur.ts.Sub(prev.ts).Seconds()
			if delta > 0 && delta <= maxCycleSec {
				for _, acc := range accums {
					acc.turnCycles = append(acc.turnCycles, delta)
				}
			}
		}
	}

	var firstUser, firstAsst *duckVelocityMsg
	firstUserIdx := -1
	for i := range msgs {
		if msgs[i].role == "user" && msgs[i].valid {
			firstUser = &msgs[i]
			firstUserIdx = i
			break
		}
	}
	if firstUserIdx >= 0 {
		for i := firstUserIdx + 1; i < len(msgs); i++ {
			if msgs[i].role == "assistant" && msgs[i].valid {
				firstAsst = &msgs[i]
				break
			}
		}
	}
	if firstUser != nil && firstAsst != nil {
		delta := firstAsst.ts.Sub(firstUser.ts).Seconds()
		if delta < 0 {
			delta = 0
		}
		for _, acc := range accums {
			acc.firstResponses = append(acc.firstResponses, delta)
		}
	}

	activeSec := 0.0
	assistantChars := 0
	for i, msg := range msgs {
		if msg.role == "assistant" {
			assistantChars += msg.contentLength
		}
		if i > 0 && msgs[i-1].valid && msg.valid {
			gap := msg.ts.Sub(msgs[i-1].ts).Seconds()
			if gap > 0 {
				if gap > maxGapSec {
					gap = maxGapSec
				}
				activeSec += gap
			}
		}
	}
	activeMinutes := activeSec / 60
	if activeMinutes > 0 {
		for _, acc := range accums {
			acc.totalMsgs += len(msgs)
			acc.totalChars += assistantChars
			acc.totalToolCalls += toolCount
			acc.activeMinutes += activeMinutes
		}
	}
}

func (a *duckVelocityAccumulator) computeOverview() db.VelocityOverview {
	sort.Float64s(a.turnCycles)
	sort.Float64s(a.firstResponses)

	out := db.VelocityOverview{}
	out.TurnCycleSec = db.Percentiles{
		P50: round1(percentileFloat(a.turnCycles, 0.5)),
		P90: round1(percentileFloat(a.turnCycles, 0.9)),
	}
	out.FirstResponseSec = db.Percentiles{
		P50: round1(percentileFloat(a.firstResponses, 0.5)),
		P90: round1(percentileFloat(a.firstResponses, 0.9)),
	}
	if a.activeMinutes > 0 {
		out.MsgsPerActiveMin = round1(float64(a.totalMsgs) / a.activeMinutes)
		out.CharsPerActiveMin = round1(float64(a.totalChars) / a.activeMinutes)
		out.ToolCallsPerActiveMin = round1(float64(a.totalToolCalls) / a.activeMinutes)
	}
	return out
}

func percentileFloat(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	idx := int(float64(len(values)) * p)
	if idx >= len(values) {
		idx = len(values) - 1
	}
	return round1(values[idx])
}

func (s *Store) GetAnalyticsTopSessions(
	ctx context.Context, f db.AnalyticsFilter, metric string,
) (db.TopSessionsResponse, error) {
	switch metric {
	case "", "messages":
		metric = "messages"
	case "duration", "output_tokens":
	default:
		metric = "messages"
	}

	if strings.TrimSpace(f.Model) != "" &&
		(metric == "messages" || metric == "output_tokens") {
		sessions, err := s.analyticsSessionsWithModelMessageCounts(ctx, f)
		if err != nil {
			return db.TopSessionsResponse{}, err
		}
		sort.SliceStable(sessions, func(i, j int) bool {
			if metric == "output_tokens" {
				if sessions[i].totalOutputTokens != sessions[j].totalOutputTokens {
					return sessions[i].totalOutputTokens >
						sessions[j].totalOutputTokens
				}
			} else if sessions[i].messageCount != sessions[j].messageCount {
				return sessions[i].messageCount >
					sessions[j].messageCount
			}
			return sessions[i].id < sessions[j].id
		})

		out := db.TopSessionsResponse{Metric: metric}
		for i := range sessions {
			if metric == "output_tokens" &&
				!sessions[i].hasTotalOutputTokens {
				continue
			}
			if len(out.Sessions) >= 10 {
				break
			}
			startedAt := sessions[i].startedAt
			endedAt := sessions[i].endedAt
			out.Sessions = append(out.Sessions, db.TopSession{
				ID:                sessions[i].id,
				Project:           sessions[i].project,
				FirstMessage:      sessions[i].firstMessage,
				DisplayName:       sessions[i].displayName,
				MessageCount:      sessions[i].messageCount,
				OutputTokens:      sessions[i].totalOutputTokens,
				DurationMin:       duckSessionDurationMinutes(sessions[i]),
				StartedAt:         &startedAt,
				EndedAt:           &endedAt,
				TerminationStatus: sessions[i].terminationStatus,
			})
		}
		return out, nil
	}

	includeTime := true
	var pairedSet map[string]bool
	if f.HasTimeFilter() && strings.TrimSpace(f.Model) != "" {
		// Filter the scoped session set in Go rather than binding every
		// paired ID into one IN (...) predicate, which would exceed the
		// driver bind-variable cap for large result sets. Mirrors the
		// SQLite/PostgreSQL top-sessions Go path under a model filter: load
		// the model+date candidates, then keep only the paired sessions and
		// limit in Go. The in-SQL ORDER BY still ranks them by the metric.
		paired, err := s.analyticsSessionsModelTimeFiltered(ctx, f, true)
		if err != nil {
			return db.TopSessionsResponse{}, err
		}
		if len(paired) == 0 {
			return db.TopSessionsResponse{Metric: metric}, nil
		}
		pairedSet = make(map[string]bool, len(paired))
		for _, session := range paired {
			pairedSet[session.id] = true
		}
		includeTime = false
	}
	where, args := duckBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, includeTime)
	durationExpr := "(epoch(s.ended_at) - epoch(s.started_at)) / 60.0"
	durationSelectExpr := "COALESCE(" + durationExpr + ", 0)"
	activeDurationExpr := fmt.Sprintf(`
		(
			SELECT COALESCE(SUM(
				CASE
					WHEN inner2.delta_ms <= 0 THEN 0
					WHEN inner2.delta_ms > %[1]d THEN %[1]d
					ELSE inner2.delta_ms
				END), 0) / 60000.0
			FROM (
				SELECT CAST(
					ROUND(epoch(
						LEAD(m2.timestamp) OVER (ORDER BY m2.ordinal)
						- m2.timestamp
					) * 1000) AS BIGINT
				) AS delta_ms
				FROM messages m2
				WHERE m2.session_id = s.id
			) inner2
		)`, db.ActiveGapCapMs)
	activeDurationSelectExpr := "COALESCE(" + activeDurationExpr + ", 0)"
	orderExpr := "s.message_count DESC, s.id ASC"
	switch metric {
	case "duration":
		where += " AND s.started_at IS NOT NULL AND s.ended_at IS NOT NULL AND s.ended_at >= s.started_at"
		orderExpr = activeDurationSelectExpr + " DESC, s.id ASC"
	case "output_tokens":
		where += " AND s.has_total_output_tokens = TRUE"
		orderExpr = "s.total_output_tokens DESC, s.id ASC"
	}
	// When filtering the scoped set in Go (model+time), drop the SQL LIMIT so
	// the paired sessions aren't truncated before the Go filter; the in-SQL
	// ORDER BY keeps them ranked and the top 10 is taken after filtering.
	limitClause := "\n\t\tLIMIT 10"
	if pairedSet != nil {
		limitClause = ""
	}
	query := `
		SELECT s.id, s.project, s.first_message, s.message_count,
			s.total_output_tokens, ` + durationSelectExpr + ` AS duration_min,
			` + activeDurationSelectExpr + ` AS active_duration_min,
			s.started_at, s.ended_at, s.termination_status
		FROM sessions s
		WHERE ` + where + `
		ORDER BY ` + orderExpr + limitClause
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return db.TopSessionsResponse{}, fmt.Errorf("querying duckdb analytics top sessions: %w", err)
	}
	defer rows.Close()

	out := db.TopSessionsResponse{Metric: metric}
	for rows.Next() {
		var row db.TopSession
		var startedRaw, endedRaw any
		if err := rows.Scan(
			&row.ID, &row.Project, &row.FirstMessage, &row.MessageCount,
			&row.OutputTokens, &row.DurationMin, &row.ActiveDurationMin,
			&startedRaw, &endedRaw,
			&row.TerminationStatus,
		); err != nil {
			return db.TopSessionsResponse{}, fmt.Errorf("scanning duckdb analytics top session: %w", err)
		}
		if pairedSet != nil && !pairedSet[row.ID] {
			continue
		}
		startedAt := formatDBTime(startedRaw)
		endedAt := formatDBTime(endedRaw)
		row.StartedAt = &startedAt
		row.EndedAt = &endedAt
		row.DurationMin = round1(row.DurationMin)
		row.ActiveDurationMin = round1(row.ActiveDurationMin)
		out.Sessions = append(out.Sessions, row)
		if pairedSet != nil && len(out.Sessions) >= 10 {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return db.TopSessionsResponse{}, fmt.Errorf("iterating duckdb analytics top sessions: %w", err)
	}
	return out, nil
}

func duckSessionDurationMinutes(session duckAnalyticsSession) float64 {
	startedAt, okStart := parseAnalyticsTime(session.startedAt)
	endedAt, okEnd := parseAnalyticsTime(session.endedAt)
	if !okStart || !okEnd || endedAt.Before(startedAt) {
		return 0
	}
	return round1(endedAt.Sub(startedAt).Minutes())
}

// GetAnalyticsSignals returns aggregated session signal data. Signals stay
// session-scoped under a model filter (totals are session-level aggregates
// over sessions that used the model, not re-attributed per model); see the
// SQLite GetAnalyticsSignals for the rationale.
func (s *Store) GetAnalyticsSignals(
	ctx context.Context, f db.AnalyticsFilter,
) (db.SignalsAnalyticsResponse, error) {
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.SignalsAnalyticsResponse{}, err
	}
	rows := duckSignalRowsFromSessions(sessions, f)
	if err := s.duckPopulateFrustrationMarkers(ctx, rows); err != nil {
		return db.SignalsAnalyticsResponse{}, err
	}
	return db.AggregateSignals(rows), nil
}

func (s *Store) GetAnalyticsSignalSessions(
	ctx context.Context,
	f db.AnalyticsFilter,
	signal string,
	limit int,
) (db.SignalSessionsResponse, error) {
	if !db.IsSupportedAnalyticsSignal(signal) {
		return db.SignalSessionsResponse{}, db.ErrUnsupportedAnalyticsSignal
	}
	if limit <= 0 || limit > 20 {
		limit = 10
	}
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.SignalSessionsResponse{}, err
	}
	rows := duckSignalRowsFromSessions(sessions, f)
	if err := s.duckPopulateFrustrationMarkers(ctx, rows); err != nil {
		return db.SignalSessionsResponse{}, err
	}
	candidates := db.SignalCandidates(rows, signal, limit)
	messages, err := s.duckSignalMessages(ctx, candidates, f)
	if err != nil {
		return db.SignalSessionsResponse{}, err
	}
	return db.SignalSessionsResponse{
		Signal:   signal,
		Sessions: db.BuildSignalExamples(candidates, messages, signal),
	}, nil
}

func duckSignalRowsFromSessions(
	sessions []duckAnalyticsSession,
	f db.AnalyticsFilter,
) []db.SignalRow {
	rows := make([]db.SignalRow, 0, len(sessions))
	for _, r := range sessions {
		rows = append(rows, db.SignalRow{
			ID:                          r.id,
			Agent:                       r.agent,
			Project:                     r.project,
			FirstMessage:                r.firstMessage,
			IsAutomated:                 r.isAutomated,
			Date:                        analyticsLocalDate(analyticsDateTime(r), f.Timezone),
			HealthScore:                 r.healthScore,
			HealthGrade:                 r.healthGrade,
			Outcome:                     r.outcome,
			OutcomeConfidence:           r.outcomeConfidence,
			ToolFailureSignalCount:      r.toolFailures,
			ToolRetryCount:              r.toolRetries,
			EditChurnCount:              r.editChurn,
			CompactionCount:             r.compactions,
			MidTaskCompactionCount:      r.midTaskCompactions,
			ContextPressureMax:          r.contextPressureMax,
			QualitySignalVersion:        r.qualitySignalVersion,
			ShortPromptCount:            r.shortPromptCount,
			UnstructuredStart:           r.unstructuredStart,
			MissingSuccessCriteriaCount: r.missingSuccessCriteriaCount,
			MissingVerificationCount:    r.missingVerificationCount,
			DuplicatePromptCount:        r.duplicatePromptCount,
			NoCodeContextCount:          r.noCodeContextCount,
			RunawayToolLoopCount:        r.runawayToolLoopCount,
			FrustrationMarkerCount:      r.frustrationMarkerCount,
		})
	}
	return rows
}

func (s *Store) duckPopulateFrustrationMarkers(
	ctx context.Context,
	rows []db.SignalRow,
) error {
	if len(rows) == 0 {
		return nil
	}
	idx := make(map[string]int, len(rows))
	placeholders := make([]string, len(rows))
	args := make([]any, len(rows))
	for i := range rows {
		idx[rows[i].ID] = i
		placeholders[i] = "?"
		args[i] = rows[i].ID
	}
	q := `SELECT session_id, content, is_system
		FROM messages
		WHERE role = 'user' AND session_id IN (` +
		strings.Join(placeholders, ",") + `)`
	msgRows, err := s.queryContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("querying duckdb frustration markers: %w", err)
	}
	defer msgRows.Close()
	for msgRows.Next() {
		var sessionID, content string
		var isSystem bool
		if err := msgRows.Scan(
			&sessionID, &content, &isSystem,
		); err != nil {
			return fmt.Errorf("scanning duckdb frustration marker: %w", err)
		}
		i, ok := idx[sessionID]
		if !ok || isSystem {
			continue
		}
		if signals.IsFrustrationMarker(content) {
			rows[i].FrustrationMarkerCount++
		}
	}
	if err := msgRows.Err(); err != nil {
		return fmt.Errorf("iterating duckdb frustration markers: %w", err)
	}
	return nil
}

func (s *Store) duckSignalMessages(
	ctx context.Context,
	rows []db.SignalRow,
	f db.AnalyticsFilter,
) (map[string][]db.SignalMessage, error) {
	out := make(map[string][]db.SignalMessage, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	if strings.TrimSpace(f.Model) != "" {
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		scope, err := s.resolveAnalyticsMessageScope(ctx, ids, f, true)
		if err != nil {
			return nil, err
		}
		if scope != nil {
			for sessionID, scopedRows := range scope.MessagesBySession() {
				for _, row := range scopedRows {
					out[sessionID] = append(out[sessionID], db.SignalMessage{
						SessionID:  row.SessionID,
						Ordinal:    row.Ordinal,
						Role:       row.Role,
						Content:    row.Content,
						Timestamp:  row.Timestamp,
						IsSystem:   row.IsSystem,
						HasToolUse: row.HasToolUse,
					})
				}
			}
		}
		return out, nil
	}
	placeholders := make([]string, len(rows))
	args := make([]any, len(rows))
	for i, r := range rows {
		placeholders[i] = "?"
		args[i] = r.ID
	}
	filterModels := duckAnalyticsCSVValues(f.Model)
	q := `SELECT session_id, ordinal, role, content,
			timestamp, is_system, has_tool_use
		FROM messages
		WHERE session_id IN (` + strings.Join(placeholders, ",") + `)`
	if len(filterModels) == 1 {
		q += ` AND model = ?`
		args = append(args, filterModels[0])
	} else if len(filterModels) > 1 {
		modelPlaceholders := make([]string, len(filterModels))
		for i, model := range filterModels {
			modelPlaceholders[i] = "?"
			args = append(args, model)
		}
		q += ` AND model IN (` + strings.Join(modelPlaceholders, ",") + `)`
	}
	q += `
		ORDER BY session_id, ordinal`
	msgRows, err := s.queryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb signal messages: %w", err)
	}
	defer msgRows.Close()
	for msgRows.Next() {
		var m db.SignalMessage
		var ts any
		if err := msgRows.Scan(
			&m.SessionID, &m.Ordinal, &m.Role,
			&m.Content, &ts,
			&m.IsSystem, &m.HasToolUse,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb signal message: %w", err)
		}
		m.Timestamp = formatDBTime(ts)
		out[m.SessionID] = append(out[m.SessionID], m)
	}
	if err := msgRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb signal messages: %w", err)
	}
	return out, nil
}

func (s *Store) GetTrendsTerms(
	ctx context.Context, f db.AnalyticsFilter,
	terms []db.TrendTermInput, granularity string,
) (db.TrendsTermsResponse, error) {
	if granularity == "" {
		granularity = "week"
	}
	buckets := db.TrendBucketRange(f.From, f.To, granularity)
	index := map[string]int{}
	for i, bucket := range buckets {
		index[bucket.Date] = i
	}
	counts := make([][]int, len(terms))
	for i := range counts {
		counts[i] = make([]int, len(buckets))
	}
	messageCounts := make([]int, len(buckets))
	sessionFilter := f
	sessionFilter.From = ""
	sessionFilter.To = ""
	sessionFilter.Model = ""
	sessionFilter.DayOfWeek = nil
	sessionFilter.Hour = nil
	sessions, err := s.analyticsSessions(ctx, sessionFilter)
	if err != nil {
		return db.TrendsTermsResponse{}, err
	}
	allowedSessions := make(map[string]bool, len(sessions))
	for _, sess := range sessions {
		allowedSessions[sess.id] = true
	}
	if len(allowedSessions) == 0 {
		return db.BuildTrendsTermsResponse(
			f.From, f.To, granularity, buckets, terms, counts, messageCounts,
		), nil
	}
	loc := analyticsLocation(f.Timezone)
	flt := messageScopeFilter(f)
	modelFiltering := len(flt.Models) > 0
	trendLocal := func(msgTS, startedAt, createdAt any) (time.Time, bool) {
		ts := firstNonEmpty(formatDBTime(msgTS), formatDBTime(startedAt), formatDBTime(createdAt))
		t, ok := parseAnalyticsTime(ts)
		if !ok {
			return time.Time{}, false
		}
		return t.In(loc), true
	}
	rows, err := s.queryContext(ctx, `
		SELECT m.session_id, m.ordinal, m.role, m.is_system,
			COALESCE(m.model, ''), m.content, m.timestamp,
			s.started_at, s.created_at
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE s.deleted_at IS NULL
			AND m.role IN ('user', 'assistant')
			AND m.is_system = FALSE
			AND `+db.DuckDBSystemPrefixSQL("m.content", "m.role")+`
		ORDER BY m.session_id, m.ordinal`)
	if err != nil {
		return db.TrendsTermsResponse{}, err
	}
	defer rows.Close()
	type trendRow struct {
		sessionID string
		role      string
		isSystem  bool
		model     string
		content   string
		msgTS     any
		startedAt any
		createdAt any
	}
	processRow := func(sessionID, content string, local time.Time) {
		if !allowedSessions[sessionID] {
			return
		}
		date := local.Format("2006-01-02")
		if f.From != "" && date < f.From {
			return
		}
		if f.To != "" && date > f.To {
			return
		}
		bucket := bucketAnalyticsDate(date, granularity)
		pos, ok := index[bucket]
		if !ok {
			return
		}
		messageCounts[pos]++
		for i, term := range terms {
			counts[i][pos] += db.CountTrendOccurrences(content, term)
		}
	}
	emit := func(m db.ScopedMessage) {
		if !m.HasLocalTime {
			return
		}
		processRow(m.SessionID, m.Content, m.LocalTime)
	}
	reducer := db.NewScopeReducer(flt, emit)
	for rows.Next() {
		var row trendRow
		var ordinal int
		if err := rows.Scan(&row.sessionID, &ordinal, &row.role, &row.isSystem, &row.model, &row.content, &row.msgTS, &row.startedAt, &row.createdAt); err != nil {
			return db.TrendsTermsResponse{}, err
		}
		local, has := trendLocal(row.msgTS, row.startedAt, row.createdAt)
		if !modelFiltering {
			if has && flt.MatchesDayHour(local, true) {
				processRow(row.sessionID, row.content, local)
			}
			continue
		}
		if err := reducer.Push(db.MessageInput{
			SessionID:    row.sessionID,
			Ordinal:      ordinal,
			Role:         row.role,
			Model:        row.model,
			IsSystem:     row.isSystem,
			LocalTime:    local,
			HasLocalTime: has,
			Content:      row.content,
		}); err != nil {
			return db.TrendsTermsResponse{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return db.TrendsTermsResponse{}, err
	}
	return db.BuildTrendsTermsResponse(
		f.From, f.To, granularity, buckets, terms, counts, messageCounts,
	), nil
}

type duckRates struct {
	input         float64
	output        float64
	cacheCreation float64
	cacheRead     float64
	updatedAt     *time.Time
	source        export.PricingRowSource
}

func (s *Store) loadPricing(ctx context.Context) (map[string]duckRates, error) {
	rows, err := s.queryContext(ctx, `
		SELECT model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok, updated_at
		FROM model_pricing`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]duckRates{}
	count := 0
	for rows.Next() {
		var model string
		var rates duckRates
		var updatedAt string
		if err := rows.Scan(
			&model, &rates.input, &rates.output,
			&rates.cacheCreation, &rates.cacheRead, &updatedAt,
		); err != nil {
			return nil, err
		}
		if strings.HasPrefix(model, "_") {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339Nano, updatedAt); err == nil {
			t := parsed.UTC()
			rates.updatedAt = &t
		}
		rates.source = duckPricingSource(model, rates)
		out[model] = rates
		count++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if count == 0 {
		out = duckFallbackPricingMap()
	}
	for model, custom := range s.customPricing {
		rates := duckRates{
			input: custom.Input, output: custom.Output,
			cacheCreation: custom.CacheCreation, cacheRead: custom.CacheRead,
		}
		rates.source = duckCustomPricingSource()
		out[model] = rates
	}
	return out, nil
}

func duckCustomPricingSource() export.PricingRowSource {
	return export.PricingRowSourceCustom
}

func duckFallbackPricingMap() map[string]duckRates {
	prices := pricingpkg.FallbackPricing()
	out := make(map[string]duckRates, len(prices))
	for _, p := range prices {
		if strings.HasPrefix(p.ModelPattern, "_") {
			continue
		}
		out[p.ModelPattern] = duckRates{
			input:         p.InputPerMTok,
			output:        p.OutputPerMTok,
			cacheCreation: p.CacheCreationPerMTok,
			cacheRead:     p.CacheReadPerMTok,
			source:        export.PricingRowSourceEmbedded,
		}
	}
	return out
}

func duckPricingSource(model string, rates duckRates) export.PricingRowSource {
	fallback := duckFallbackPricingMap()
	if f, ok := fallback[model]; ok &&
		f.input == rates.input &&
		f.output == rates.output &&
		f.cacheCreation == rates.cacheCreation &&
		f.cacheRead == rates.cacheRead {
		return export.PricingRowSourceEmbedded
	}
	return export.PricingRowSourceFetched
}

func duckPricingRows(
	in map[string]duckRates,
) []export.EffectivePricingRow {
	out := make([]export.EffectivePricingRow, 0, len(in))
	for pattern, rates := range in {
		source := rates.source
		if source == "" {
			source = duckPricingSource(pattern, rates)
		}
		out = append(out, export.EffectivePricingRow{
			ModelPattern: pattern,
			Rates: export.ModelRates{
				InputPerMTok:      rates.input,
				OutputPerMTok:     rates.output,
				CacheWritePerMTok: rates.cacheCreation,
				CacheReadPerMTok:  rates.cacheRead,
				UpdatedAt:         rates.updatedAt,
				Source:            source,
			},
		})
	}
	return out
}

type duckUsageBounds struct {
	from string
	to   string
}

func duckUsagePaddedUTCBound(ts string, hours int) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Add(time.Duration(hours) * time.Hour).Format(time.RFC3339)
}

func duckUsageBoundsForFilter(f db.UsageFilter) duckUsageBounds {
	var b duckUsageBounds
	if f.From != "" {
		b.from = duckUsagePaddedUTCBound(f.From+"T00:00:00Z", -14)
	}
	if f.To != "" {
		b.to = duckUsagePaddedUTCBound(f.To+"T23:59:59Z", 14)
	}
	return b
}

func appendDuckUsageColumnBounds(
	where, col string, b duckUsageBounds, args []any,
) (string, []any) {
	if b.from != "" {
		where += "\n\t\t\tAND " + col + " >= CAST(? AS TIMESTAMP)"
		args = append(args, b.from)
	}
	if b.to != "" {
		where += "\n\t\t\tAND " + col + " <= CAST(? AS TIMESTAMP)"
		args = append(args, b.to)
	}
	return where, args
}

func appendDuckUsageCSVFilter(
	where string, args []any, col, csv string, include bool,
) (string, []any) {
	if csv == "" {
		return where, args
	}
	parts := strings.Split(csv, ",")
	vals := make([]string, 0, len(parts))
	for _, value := range parts {
		trimmed := strings.TrimSpace(value)
		if trimmed != "" {
			vals = append(vals, trimmed)
		}
	}
	return appendDuckUsageValuesFilter(where, args, col, vals, include)
}

func appendDuckUsageValuesFilter(
	where string, args []any, col string, vals []string, include bool,
) (string, []any) {
	if len(vals) == 0 {
		return where, args
	}
	op := "IN"
	if !include {
		op = "NOT IN"
	}
	if len(vals) == 1 {
		if include {
			where += "\n\t\t\tAND " + col + " = ?"
		} else {
			where += "\n\t\t\tAND " + col + " != ?"
		}
		args = append(args, vals[0])
		return where, args
	}
	ph := make([]string, len(vals))
	for i, value := range vals {
		ph[i] = "?"
		args = append(args, value)
	}
	where += "\n\t\t\tAND " + col + " " + op +
		" (" + strings.Join(ph, ",") + ")"
	return where, args
}

func appendDuckUsageSourceFilterClauses(
	where string, args []any, modelCol string, f db.UsageFilter,
) (string, []any) {
	where, args = appendDuckUsageCSVFilter(where, args, modelCol, f.Model, true)
	return appendDuckUsageCSVFilter(where, args, modelCol, f.ExcludeModel, false)
}

func appendDuckUsageSessionFilterClauses(
	where string, args []any, f db.UsageFilter, sessionID string,
) (string, []any) {
	where, args = appendDuckUsageCSVFilter(where, args, "s.agent", f.Agent, true)
	where, args = appendDuckUsageValuesFilter(
		where, args, "s.project", f.ProjectFilterLabels(), true,
	)
	where, args = appendDuckUsageCSVFilter(where, args, "s.machine", f.Machine, true)
	if f.GitBranch != "" {
		var clause string
		clause, args = db.BranchPairClauseArgs("s.project", "s.git_branch", f.GitBranch, args)
		where += "\n\t\t\tAND " + clause
	}
	where, args = appendDuckUsageValuesFilter(
		where, args, "s.project", f.ExcludedProjectFilterLabels(), false,
	)
	where, args = appendDuckUsageCSVFilter(where, args, "s.agent", f.ExcludeAgent, false)
	if sessionID != "" {
		where += "\n\t\t\tAND s.id = ?"
		args = append(args, sessionID)
	}
	if f.MinUserMessages > 0 {
		where += "\n\t\t\tAND s.user_message_count >= ?"
		args = append(args, f.MinUserMessages)
	}
	scope := duckNormalizeAutomatedScope(
		f.AutomatedScope, f.ExcludeAutomated)
	if f.ExcludeOneShot {
		if scope == "human" {
			where += "\n\t\t\tAND s.user_message_count > 1"
		} else {
			where += "\n\t\t\tAND (s.user_message_count > 1 OR COALESCE(s.is_automated, FALSE) = TRUE)"
		}
	}
	if pred := duckAutomatedScopePredicate(
		scope, "COALESCE(s.is_automated, FALSE)"); pred != "" {
		where += "\n\t\t\tAND " + pred
	}
	if f.ActiveSince != "" {
		where += "\n\t\t\tAND COALESCE(s.ended_at, s.started_at, s.created_at) >= CAST(? AS TIMESTAMP)"
		args = append(args, f.ActiveSince)
	}
	if pred, predArgs := duckUsageTerminationPred(f.Termination); pred != "" {
		where += "\n\t\t\tAND " + pred
		args = append(args, predArgs...)
	}
	return where, args
}

func duckUsageTerminationPred(status string) (string, []any) {
	return duckAnalyticsTerminationPred(
		status,
		"COALESCE(s.ended_at, s.started_at, s.created_at)",
		"s.termination_status",
	)
}

const duckDailyCursorUsageRowsSQLTemplate = `
SELECT
	'' AS session_id,
	NULL AS message_ordinal,
	'cursor' AS source,
	cu.occurred_at AS ts,
	cu.model AS model,
	'' AS token_json,
	'' AS claude_message_id,
	'' AS claude_request_id,
	'' AS source_uuid,
	cu.dedup_key AS usage_dedup_key,
	cu.input_tokens AS input_tokens,
	cu.output_tokens AS output_tokens,
	cu.cache_write_tokens AS cache_create,
	cu.cache_read_tokens AS cache_read,
	0 AS reasoning_tokens,
	cu.charged_cents / 100.0 AS cost_usd,
	'cursor-reported' AS cost_source,
	'' AS project,
	'cursor' AS agent,
	'' AS machine,
	0 AS user_message_count,
	cu.is_headless AS is_automated,
	'' AS display_name,
	NULL AS started_at,
	cu.occurred_at AS activity_at
FROM cursor_usage_events cu
WHERE %s`

const duckUsageMessageEligibility = `
			m.token_usage != ''
			AND m.model != ''
			AND m.model != '<synthetic>'
			AND s.deleted_at IS NULL`

// duckUsageMatchingMessageSourceEligibility is the message-only half of
// duckUsageMessageEligibility with the token-presence requirement removed
// and the model-presence requirement relaxed to a role check, for
// GetUsageMatchingSessionCount. See the usageMatchingMessageEligibility
// doc comment in internal/db.
const duckUsageMatchingMessageSourceEligibility = `
			m.role = 'assistant'
			AND m.model != '<synthetic>'`

const duckUsageMatchingMessageEligibility = duckUsageMatchingMessageSourceEligibility + `
			AND s.deleted_at IS NULL`

const duckUsageEventSourceEligibility = `
			ue.model != ''`

const duckUsageEventEligibility = duckUsageEventSourceEligibility + `
			AND s.deleted_at IS NULL`

// duckUsageSourceWheres builds the message/event WHERE clauses shared by
// duckUsageRawSQL and duckMatchingUsageRawSQL; the two callers differ only
// in the message eligibility predicate.
func duckUsageSourceWheres(
	f db.UsageFilter, sessionID, messageEligibility string, b duckUsageBounds,
) (string, []any, string, []any) {
	messageWhere := messageEligibility
	var messageArgs []any
	messageWhere, messageArgs = appendDuckUsageSourceFilterClauses(
		messageWhere, messageArgs, "m.model", f)
	messageWhere, messageArgs = appendDuckUsageSessionFilterClauses(
		messageWhere, messageArgs, f, sessionID)
	messageWhere, messageArgs = appendDuckUsageColumnBounds(
		messageWhere, "COALESCE(m.timestamp, s.started_at)", b, messageArgs)

	eventWhere := duckUsageEventEligibility
	var eventArgs []any
	eventWhere, eventArgs = appendDuckUsageSourceFilterClauses(
		eventWhere, eventArgs, "ue.model", f)
	eventWhere, eventArgs = appendDuckUsageSessionFilterClauses(
		eventWhere, eventArgs, f, sessionID)
	eventWhere, eventArgs = appendDuckUsageColumnBounds(
		eventWhere, "COALESCE(ue.occurred_at, s.started_at)", b, eventArgs)

	return messageWhere, messageArgs, eventWhere, eventArgs
}

func duckUsageRawSQL(f db.UsageFilter, sessionID string) (string, []any) {
	messageWhere, messageArgs, eventWhere, eventArgs := duckUsageSourceWheres(
		f, sessionID, duckUsageMessageEligibility, duckUsageBoundsForFilter(f))

	query := fmt.Sprintf(`
		SELECT m.session_id AS session_id, m.ordinal AS message_ordinal,
			'message' AS source, COALESCE(m.timestamp, s.started_at) AS ts,
			m.model AS model, m.token_usage AS token_json,
			m.claude_message_id AS claude_message_id,
			m.claude_request_id AS claude_request_id,
			m.source_uuid AS source_uuid,
			'' AS usage_dedup_key,
				0 AS input_tokens, 0 AS output_tokens,
				0 AS cache_create, 0 AS cache_read,
				COALESCE(TRY_CAST(json_extract_string(m.token_usage, '$.reasoning_tokens') AS BIGINT), 0) AS reasoning_tokens,
				NULL AS cost_usd,
				'' AS cost_source,
			s.project AS project, s.agent AS agent, s.machine AS machine,
			s.user_message_count AS user_message_count, s.is_automated AS is_automated,
			COALESCE(s.display_name, s.session_name, s.first_message, s.project, s.id) AS display_name,
			s.started_at AS started_at,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS activity_at
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE %s
		UNION ALL
		SELECT ue.session_id AS session_id, ue.message_ordinal AS message_ordinal,
			ue.source AS source, COALESCE(ue.occurred_at, s.started_at) AS ts,
			ue.model AS model, '' AS token_json,
			'' AS claude_message_id, '' AS claude_request_id,
			'' AS source_uuid,
			CASE
				WHEN ue.dedup_key != '' THEN ue.session_id || ':' || ue.source || ':' || ue.dedup_key
				ELSE ue.session_id || ':' || ue.source || ':id:' || CAST(ue.id AS VARCHAR)
			END AS usage_dedup_key,
				ue.input_tokens AS input_tokens, ue.output_tokens AS output_tokens,
				ue.cache_creation_input_tokens AS cache_create,
				ue.cache_read_input_tokens AS cache_read,
				ue.reasoning_tokens AS reasoning_tokens,
				ue.cost_usd AS cost_usd,
				ue.cost_source AS cost_source,
			s.project AS project, s.agent AS agent, s.machine AS machine,
			s.user_message_count AS user_message_count, s.is_automated AS is_automated,
			COALESCE(s.display_name, s.session_name, s.first_message, s.project, s.id) AS display_name,
			s.started_at AS started_at,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS activity_at
		FROM usage_events ue
		JOIN sessions s ON s.id = ue.session_id
		WHERE %s`,
		messageWhere, eventWhere)
	args := make([]any, 0, len(messageArgs)+len(eventArgs))
	args = append(args, messageArgs...)
	args = append(args, eventArgs...)
	return query, args
}

// duckMatchingUsageRawSQL builds the bounded-range row source for
// GetUsageMatchingSessionCount. It shares duckUsageRawSQL's WHERE
// assembly (via duckUsageSourceWheres) but relaxes the message predicate:
// no token_usage requirement (Copilot messages never populate it) and no
// model-presence requirement (some Copilot assistant messages parse
// before a model name is known), scoping to assistant rows via m.role
// instead. Model/ExcludeModel filters are still applied per-row, same as
// duckUsageRawSQL.
func duckMatchingUsageRawSQL(f db.UsageFilter) (string, []any) {
	messageWhere, messageArgs, eventWhere, eventArgs := duckUsageSourceWheres(
		f, "", duckUsageMatchingMessageEligibility, duckUsageBoundsForFilter(f))

	query := fmt.Sprintf(`
		SELECT m.session_id AS session_id,
			COALESCE(m.timestamp, s.started_at) AS ts
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE %s
		UNION ALL
		SELECT ue.session_id AS session_id,
			COALESCE(ue.occurred_at, s.started_at) AS ts
		FROM usage_events ue
		JOIN sessions s ON s.id = ue.session_id
		WHERE %s`,
		messageWhere, eventWhere)
	args := make([]any, 0, len(messageArgs)+len(eventArgs))
	args = append(args, messageArgs...)
	args = append(args, eventArgs...)
	return query, args
}

func duckCursorUsageRowsSQLForBounds(
	f db.UsageFilter, b duckUsageBounds,
) (string, []any, bool) {
	hasTermFilter := f.Termination != "" && f.Termination != "all"
	// Cursor usage rows carry no project or git branch and bypass the session
	// filter, so any filter they cannot satisfy (project, machine, branch)
	// must exclude them entirely rather than let them leak into totals.
	if len(f.ProjectFilterLabels()) > 0 ||
		len(f.ExcludedProjectFilterLabels()) > 0 ||
		f.Machine != "" || f.GitBranch != "" || f.MinUserMessages > 0 ||
		f.ExcludeOneShot || hasTermFilter ||
		f.ActiveSince != "" {
		return "", nil, false
	}
	if f.Agent != "" {
		vals := strings.Split(f.Agent, ",")
		for i := range vals {
			vals[i] = strings.TrimSpace(vals[i])
		}
		if !slices.Contains(vals, "cursor") {
			return "", nil, false
		}
	}
	if f.ExcludeAgent != "" {
		vals := strings.Split(f.ExcludeAgent, ",")
		for i := range vals {
			vals[i] = strings.TrimSpace(vals[i])
		}
		if slices.Contains(vals, "cursor") {
			return "", nil, false
		}
	}

	where := "cu.model != ''"
	var args []any
	scope := duckNormalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	if pred := duckAutomatedScopePredicate(scope, "cu.is_headless"); pred != "" {
		where += "\n\tAND " + pred
	}
	where, args = appendDuckUsageSourceFilterClauses(
		where, args, "cu.model", f,
	)
	where, args = appendDuckUsageColumnBounds(
		where, "cu.occurred_at", b, args,
	)
	return fmt.Sprintf(duckDailyCursorUsageRowsSQLTemplate, where), args, true
}

func duckDailyUsageRawSQL(f db.UsageFilter) (string, []any) {
	bounds := duckUsageBoundsForFilter(f)
	sessionRowsSQL, sessionArgs := duckUsageRawSQL(f, "")
	cursorRowsSQL, cursorArgs, ok := duckCursorUsageRowsSQLForBounds(f, bounds)
	if !ok {
		return sessionRowsSQL, sessionArgs
	}
	rowsSQL := sessionRowsSQL + "\n\t\tUNION ALL\n" + cursorRowsSQL
	args := make([]any, 0, len(sessionArgs)+len(cursorArgs))
	args = append(args, sessionArgs...)
	args = append(args, cursorArgs...)
	return rowsSQL, args
}

func duckUsageLocalDateSQL(f db.UsageFilter) (string, any) {
	if f.Timezone != "" {
		return "COALESCE(strftime(timezone(?, timezone('UTC', ts)), '%Y-%m-%d'), '')", f.Timezone
	}
	ref := time.Now().UTC()
	if f.From != "" {
		if t, err := time.Parse(time.RFC3339, f.From+"T12:00:00Z"); err == nil {
			ref = t
		}
	}
	_, offset := ref.In(time.Local).Zone()
	return "COALESCE(strftime(ts + (? * INTERVAL 1 SECOND), '%Y-%m-%d'), '')", offset
}

func duckUsageCTE(f db.UsageFilter, sessionID string) (string, []any) {
	rawSQL, args := duckUsageRawSQL(f, sessionID)
	return duckUsageCTEFromRaw(f, rawSQL, args)
}

func duckDailyUsageCTE(f db.UsageFilter) (string, []any) {
	rawSQL, args := duckDailyUsageRawSQL(f)
	return duckUsageCTEFromRaw(f, rawSQL, args)
}

func duckUsageCTEFromRaw(
	f db.UsageFilter, rawSQL string, args []any,
) (string, []any) {
	localDateSQL, localDateArg := duckUsageLocalDateSQL(f)
	// Apply the local-date window BEFORE deduping so an out-of-range
	// duplicate (pulled in by the padded UTC bounds) cannot win
	// dedup_rank = 1 and suppress the in-range row. Mirrors the
	// dedup-after-date-filter order in internal/db/usage.go.
	datePred := "TRUE"
	var dateArgs []any
	if f.From != "" {
		datePred += " AND local_date >= ?"
		dateArgs = append(dateArgs, f.From)
	}
	if f.To != "" {
		datePred += " AND local_date <= ?"
		dateArgs = append(dateArgs, f.To)
	}
	query := fmt.Sprintf(`
		WITH usage_raw AS (
			%[1]s
		),
		usage_normalized AS (
			SELECT *,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.input_tokens') AS BIGINT), 0), 0), %[4]d)
					WHEN source = 'session' THEN GREATEST(input_tokens, 0)
					ELSE LEAST(GREATEST(input_tokens, 0), %[4]d)
				END AS input_tokens_norm,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.output_tokens') AS BIGINT), 0), 0), %[4]d)
					WHEN source = 'session' THEN GREATEST(output_tokens, 0)
					ELSE LEAST(GREATEST(output_tokens, 0), %[4]d)
				END AS output_tokens_norm,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_creation_input_tokens') AS BIGINT), 0), 0), %[4]d)
					WHEN source = 'session' THEN GREATEST(cache_create, 0)
					ELSE LEAST(GREATEST(cache_create, 0), %[4]d)
				END AS cache_create_norm,
					CASE
						WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_read_input_tokens') AS BIGINT), 0), 0), %[4]d)
						WHEN source = 'session' THEN GREATEST(cache_read, 0)
						ELSE LEAST(GREATEST(cache_read, 0), %[4]d)
					END AS cache_read_norm,
					CASE
						WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.reasoning_tokens') AS BIGINT), 0), 0), %[4]d)
						WHEN source = 'session' THEN GREATEST(reasoning_tokens, 0)
						ELSE LEAST(GREATEST(reasoning_tokens, 0), %[4]d)
					END AS reasoning_tokens_norm,
				CASE
					WHEN claude_message_id != '' AND claude_request_id != ''
						THEN 'claude:' || claude_message_id || ':' || claude_request_id
					WHEN source = 'message' AND agent != '' AND source_uuid != ''
						THEN 'source:' || agent || ':' || source_uuid
					WHEN usage_dedup_key != ''
						THEN 'usage:' || usage_dedup_key
					ELSE 'row:' || session_id || ':' || source || ':' ||
						COALESCE(CAST(message_ordinal AS VARCHAR), '') || ':' ||
						COALESCE(CAST(ts AS VARCHAR), '') || ':' || model
				END AS dedup_group,
				%[2]s AS local_date
			FROM usage_raw
		),
		usage_windowed AS (
			SELECT *
			FROM usage_normalized
			WHERE %[3]s
		),
		usage_ranked AS (
			SELECT *,
				ROW_NUMBER() OVER (
					PARTITION BY dedup_group
					ORDER BY ts ASC, session_id ASC, COALESCE(message_ordinal, -1) ASC
				) AS dedup_rank
			FROM usage_windowed
		),
		usage_localized AS (
			SELECT *
			FROM usage_ranked
			WHERE dedup_rank = 1
		)`, rawSQL, localDateSQL, datePred, db.MaxPlausibleTokens)
	args = append(args, localDateArg)
	args = append(args, dateArgs...)
	return query, args
}

type duckUsageBucket struct {
	inputTok      int
	outputTok     int
	cacheCr       int
	cacheRd       int
	cost          float64
	aggregateCost float64
}

type duckUsageAggregateRow struct {
	date          string
	sessionID     string
	project       string
	agent         string
	machine       string
	model         string
	displayName   string
	startedAt     string
	inputTok      int
	outputTok     int
	cacheCr       int
	cacheRd       int
	billableInput int
	// Output-rate billable tokens. SQL folds reasoning-only rows into this
	// value before grouping because reasoning is otherwise a row-level choice.
	billableOutput        int
	billableReason        int
	billableCacheCr       int
	billableCacheRd       int
	explicitCost          float64
	reportedCostRows      int
	authoritativeCost     float64
	authoritativeCostRows int
}

type duckSessionUsageRow struct {
	sessionID      string
	messageOrdinal sql.NullInt64
	source         string
	ts             string
	model          string
	inputTok       int
	outputTok      int
	cacheCr        int
	cacheRd        int
	reasoningTok   int
	costUSD        sql.NullFloat64
	costSource     string
}

func duckUsageAggregateCost(
	model string,
	inputTok, outputTok, cacheCr, cacheRd int,
	billableInput, billableOutput, billableReasoning, billableCacheCr, billableCacheRd int,
	explicitCost float64,
	hasReportedCost bool,
	pricing *export.PricingResolver,
) (float64, float64, bool, bool) {
	hasBillableTokens := billableInput != 0 || billableOutput != 0 ||
		billableReasoning != 0 || billableCacheCr != 0 || billableCacheRd != 0
	if !hasReportedCost &&
		explicitCost == 0 &&
		inputTok == 0 && outputTok == 0 && cacheCr == 0 && cacheRd == 0 &&
		!hasBillableTokens {
		pricing.RecordComputed(model, pricing.Lookup(model))
		return 0, 0, true, false
	}
	lookup := pricing.Lookup(model)
	rates := lookup.Rates
	cost := explicitCost +
		rates.CostForTokens(
			billableInput, billableOutput, billableReasoning,
			billableCacheCr, billableCacheRd)
	if hasReportedCost {
		pricing.RecordReported(model, lookup)
	}
	if hasBillableTokens {
		pricing.RecordComputed(model, lookup)
	}
	readDelta := float64(cacheRd) * (rates.InputPerMTok - rates.CacheReadPerMTok) / 1_000_000
	createDelta := float64(cacheCr) * (rates.InputPerMTok - rates.CacheWritePerMTok) / 1_000_000
	priced := lookup.OK
	if !hasBillableTokens && hasReportedCost {
		priced = true
	}
	return cost, readDelta + createDelta, priced, true
}

func duckSessionUsageRowCost(
	r duckSessionUsageRow, pricing map[string]duckRates,
) (float64, bool, bool) {
	if r.costUSD.Valid && r.costSource != db.CopilotReportedCostSource {
		return r.costUSD.Float64, true, true
	}
	if r.inputTok == 0 && r.outputTok == 0 && r.reasoningTok == 0 &&
		r.cacheCr == 0 && r.cacheRd == 0 {
		return 0, true, false
	}
	rates, priced := pricingpkg.Resolve(pricing, r.model)
	if !priced {
		return 0, false, true
	}
	// Reasoning is a breakdown of output, not additional billable
	// output; reasoning-only rows bill at the output rate. Mirrors
	// export.ModelRates.CostForTokens and the aggregate SQL fold.
	billableOutput := r.outputTok
	if billableOutput == 0 {
		billableOutput = r.reasoningTok
	}
	cost := (float64(r.inputTok)*rates.input +
		float64(billableOutput)*rates.output +
		float64(r.cacheCr)*rates.cacheCreation +
		float64(r.cacheRd)*rates.cacheRead) / 1_000_000
	return cost, true, true
}

func duckSessionUsageBreakdownEntry(
	r duckSessionUsageRow,
	ordinal int,
	cost float64,
	priced bool,
) db.SessionUsageBreakdownEntry {
	entry := db.SessionUsageBreakdownEntry{
		Ordinal:                  ordinal,
		Source:                   r.source,
		Label:                    duckSessionUsageBreakdownLabel(r),
		Timestamp:                r.ts,
		Model:                    r.model,
		InputTokens:              r.inputTok,
		OutputTokens:             r.outputTok,
		CacheCreationInputTokens: r.cacheCr,
		CacheReadInputTokens:     r.cacheRd,
		CostUSD:                  cost,
		HasCost:                  priced,
	}
	if r.messageOrdinal.Valid {
		messageOrdinal := int(r.messageOrdinal.Int64)
		entry.MessageOrdinal = &messageOrdinal
	}
	return entry
}

func duckSessionUsageBreakdownLabel(r duckSessionUsageRow) string {
	if r.messageOrdinal.Valid {
		if r.source == "message" {
			return fmt.Sprintf("Prompt %d", r.messageOrdinal.Int64+1)
		}
		return fmt.Sprintf("Step %d", r.messageOrdinal.Int64+1)
	}
	if r.source != "" {
		return r.source
	}
	return "usage"
}

func (s *Store) dailyUsageAggregateRows(
	ctx context.Context, f db.UsageFilter,
) ([]duckUsageAggregateRow, error) {
	cte, args := duckDailyUsageCTE(f)
	machineSelect := "'' AS machine"
	machineGroup := ""
	machineOrder := ""
	if f.Breakdowns {
		machineSelect = "machine"
		machineGroup = ", machine"
		machineOrder = ", machine ASC"
	}
	query := cte + `
		SELECT session_id, local_date, project, agent, ` + machineSelect + `, model,
			SUM(input_tokens_norm) AS input_tokens,
			SUM(output_tokens_norm) AS output_tokens,
			SUM(cache_create_norm) AS cache_creation_tokens,
				SUM(cache_read_norm) AS cache_read_tokens,
				SUM(CASE WHEN cost_usd IS NULL OR cost_source = 'copilot-reported' THEN input_tokens_norm ELSE 0 END) AS billable_input_tokens,
				SUM(CASE
					WHEN cost_usd IS NOT NULL AND cost_source != 'copilot-reported' THEN 0
					WHEN output_tokens_norm = 0 THEN reasoning_tokens_norm
					ELSE output_tokens_norm
				END) AS billable_output_tokens,
				CAST(0 AS BIGINT) AS billable_reasoning_tokens,
				SUM(CASE WHEN cost_usd IS NULL OR cost_source = 'copilot-reported' THEN cache_create_norm ELSE 0 END) AS billable_cache_creation_tokens,
				SUM(CASE WHEN cost_usd IS NULL OR cost_source = 'copilot-reported' THEN cache_read_norm ELSE 0 END) AS billable_cache_read_tokens,
				COALESCE(SUM(cost_usd) FILTER (WHERE cost_source != 'copilot-reported'), 0) AS explicit_cost,
				COUNT(cost_usd) FILTER (WHERE cost_source != 'copilot-reported') AS reported_cost_rows,
				COALESCE(SUM(cost_usd) FILTER (WHERE cost_source = 'copilot-reported'), 0) AS authoritative_cost,
				COUNT(cost_usd) FILTER (WHERE cost_source = 'copilot-reported') AS authoritative_cost_rows
		FROM usage_localized
		GROUP BY session_id, local_date, project, agent` + machineGroup + `, model
		ORDER BY session_id ASC, local_date ASC, project ASC, agent ASC` + machineOrder + `, model ASC`
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb daily usage aggregates: %w", err)
	}
	defer rows.Close()
	var out []duckUsageAggregateRow
	for rows.Next() {
		var r duckUsageAggregateRow
		if err := rows.Scan(
			&r.sessionID, &r.date, &r.project, &r.agent, &r.machine, &r.model,
			&r.inputTok, &r.outputTok, &r.cacheCr, &r.cacheRd,
			&r.billableInput, &r.billableOutput, &r.billableReason,
			&r.billableCacheCr, &r.billableCacheRd,
			&r.explicitCost, &r.reportedCostRows,
			&r.authoritativeCost, &r.authoritativeCostRows,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb daily usage aggregate: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetDailyUsage(
	ctx context.Context, f db.UsageFilter,
) (db.DailyUsageResult, error) {
	pricing, err := s.loadPricing(ctx)
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	rateResolver := export.NewPricingResolver(duckPricingRows(pricing))
	rows, err := s.dailyUsageAggregateRows(ctx, f)
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	type usageAccumKey struct {
		date    string
		project string
		agent   string
		machine string
		model   string
	}
	accum := map[usageAccumKey]*duckUsageBucket{}
	type sessionCost struct {
		agent         string
		estimated     map[usageAccumKey]float64
		authoritative *struct {
			key  usageAccumKey
			cost float64
		}
	}
	sessionCosts := map[string]*sessionCost{}
	useAuthoritativeCost := f.Model == "" && f.ExcludeModel == ""
	hasAuthoritativeCost := false
	projectLabels := map[string]bool{}
	totalSavings := 0.0
	for _, r := range rows {
		key := usageAccumKey{
			date: r.date, project: r.project, agent: r.agent,
			machine: r.machine, model: r.model,
		}
		if r.project != "" {
			projectLabels[r.project] = true
		}
		b := accum[key]
		if b == nil {
			b = &duckUsageBucket{}
			accum[key] = b
		}
		cost, savings, _, _ := duckUsageAggregateCost(
			r.model,
			r.inputTok, r.outputTok, r.cacheCr, r.cacheRd,
			r.billableInput, r.billableOutput, r.billableReason,
			r.billableCacheCr, r.billableCacheRd,
			r.explicitCost,
			r.reportedCostRows > 0,
			rateResolver,
		)
		totalSavings += savings
		b.inputTok += r.inputTok
		b.outputTok += r.outputTok
		b.cacheCr += r.cacheCr
		b.cacheRd += r.cacheRd
		b.cost += cost
		sc := sessionCosts[r.sessionID]
		if sc == nil {
			sc = &sessionCost{
				agent:     r.agent,
				estimated: map[usageAccumKey]float64{},
			}
			sessionCosts[r.sessionID] = sc
		}
		sc.estimated[key] += cost
		if useAuthoritativeCost && r.authoritativeCostRows > 0 {
			hasAuthoritativeCost = true
			sc.authoritative = &struct {
				key  usageAccumKey
				cost float64
			}{key: key, cost: r.authoritativeCost}
		}
	}
	var copilotAICredits float64
	for _, sc := range sessionCosts {
		resolvedCost := 0.0
		if sc.authoritative != nil {
			b := accum[sc.authoritative.key]
			if b == nil {
				b = &duckUsageBucket{}
				accum[sc.authoritative.key] = b
			}
			b.aggregateCost += sc.authoritative.cost
			resolvedCost = sc.authoritative.cost
		} else {
			for key, cost := range sc.estimated {
				b := accum[key]
				if b == nil {
					b = &duckUsageBucket{}
					accum[key] = b
				}
				b.aggregateCost += cost
				resolvedCost += cost
			}
		}
		copilotAICredits += db.AICreditsFromCost(sc.agent, resolvedCost)
	}

	type dayMaps struct {
		models    map[string]duckUsageBucket
		projects  map[string]duckUsageBucket
		agents    map[string]duckUsageBucket
		machines  map[string]duckUsageBucket
		totalCost float64
	}
	days := map[string]*dayMaps{}
	for key, b := range accum {
		day := days[key.date]
		if day == nil {
			day = &dayMaps{
				models:   map[string]duckUsageBucket{},
				projects: map[string]duckUsageBucket{},
				agents:   map[string]duckUsageBucket{},
				machines: map[string]duckUsageBucket{},
			}
			days[key.date] = day
		}
		modelBucket := *b
		modelBucket.aggregateCost = 0
		addUsageBucket(day.models, key.model, modelBucket)
		day.totalCost += b.aggregateCost
		if f.Breakdowns {
			aggregateBucket := *b
			aggregateBucket.cost = b.aggregateCost
			addUsageBucket(day.projects, key.project, aggregateBucket)
			addUsageBucket(day.agents, key.agent, aggregateBucket)
			addUsageBucket(day.machines, key.machine, aggregateBucket)
		}
	}

	var result db.DailyUsageResult
	for _, date := range sortedKeys(days) {
		day := days[date]
		if day == nil {
			continue
		}
		entry := db.DailyUsageEntry{Date: date}
		modelNames := sortedUsageBucketKeys(day.models)
		entry.ModelsUsed = modelNames
		for _, model := range modelNames {
			b := day.models[model]
			entry.InputTokens += b.inputTok
			entry.OutputTokens += b.outputTok
			entry.CacheCreationTokens += b.cacheCr
			entry.CacheReadTokens += b.cacheRd
			entry.ModelBreakdowns = append(entry.ModelBreakdowns, db.ModelBreakdown{
				ModelName:           model,
				InputTokens:         b.inputTok,
				OutputTokens:        b.outputTok,
				CacheCreationTokens: b.cacheCr,
				CacheReadTokens:     b.cacheRd,
				Cost:                roundCost(b.cost),
			})
		}
		entry.TotalCost = day.totalCost
		if f.Breakdowns {
			for _, project := range sortedUsageBucketKeys(day.projects) {
				b := day.projects[project]
				entry.ProjectBreakdowns = append(entry.ProjectBreakdowns, db.ProjectBreakdown{
					Project:             project,
					InputTokens:         b.inputTok,
					OutputTokens:        b.outputTok,
					CacheCreationTokens: b.cacheCr,
					CacheReadTokens:     b.cacheRd,
					Cost:                roundCost(b.cost),
				})
			}
			for _, agent := range sortedUsageBucketKeys(day.agents) {
				b := day.agents[agent]
				entry.AgentBreakdowns = append(entry.AgentBreakdowns, db.AgentBreakdown{
					Agent:               agent,
					InputTokens:         b.inputTok,
					OutputTokens:        b.outputTok,
					CacheCreationTokens: b.cacheCr,
					CacheReadTokens:     b.cacheRd,
					Cost:                roundCost(b.cost),
				})
			}
			for _, machine := range sortedUsageBucketKeys(day.machines) {
				b := day.machines[machine]
				entry.MachineBreakdowns = append(
					entry.MachineBreakdowns,
					db.MachineBreakdown{
						MachineName:         machine,
						InputTokens:         b.inputTok,
						OutputTokens:        b.outputTok,
						CacheCreationTokens: b.cacheCr,
						CacheReadTokens:     b.cacheRd,
						Cost:                roundCost(b.cost),
					},
				)
			}
		}
		if !hasAuthoritativeCost {
			entry.TotalCost = roundCost(entry.TotalCost)
		}
		result.Daily = append(result.Daily, entry)
		result.Totals.InputTokens += entry.InputTokens
		result.Totals.OutputTokens += entry.OutputTokens
		result.Totals.CacheCreationTokens += entry.CacheCreationTokens
		result.Totals.CacheReadTokens += entry.CacheReadTokens
		result.Totals.TotalCost += entry.TotalCost
	}
	result.Totals.CacheSavings = roundCost(totalSavings)
	if !hasAuthoritativeCost {
		result.Totals.TotalCost = roundCost(result.Totals.TotalCost)
	}

	if copilotAICredits > 0 {
		result.Totals.CopilotAICredits = copilotAICredits
	}

	if result.Daily == nil {
		result.Daily = []db.DailyUsageEntry{}
	}
	result.SchemaVersion = export.UsageDailySchemaVersion
	pricingBlock, err := rateResolver.BuildBlock()
	if err != nil {
		return db.DailyUsageResult{}, fmt.Errorf(
			"building pricing block: %w", err)
	}
	result.Pricing = &pricingBlock
	projects, err := s.BuildProjectIdentityMap(ctx, sortedBoolKeys(projectLabels))
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	result.Projects = export.ProjectMapForWire(projects)
	if !f.SkipSessionCounts {
		counts, err := s.GetUsageSessionCounts(ctx, f)
		if err != nil {
			return db.DailyUsageResult{}, err
		}
		result.SessionCounts = counts
	}
	db.SanitizeDailyUsageProjectLabelsWithCatalog(&result, projects)
	return result, nil
}

func addUsageBucket(m map[string]duckUsageBucket, key string, b duckUsageBucket) {
	cur := m[key]
	cur.inputTok += b.inputTok
	cur.outputTok += b.outputTok
	cur.cacheCr += b.cacheCr
	cur.cacheRd += b.cacheRd
	cur.cost += b.cost
	m[key] = cur
}

func sortedUsageBucketKeys(m map[string]duckUsageBucket) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		left := m[out[i]]
		right := m[out[j]]
		if left.cost != right.cost {
			return left.cost > right.cost
		}
		return out[i] < out[j]
	})
	return out
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func roundCost(v float64) float64 { return math.Round(v*1_000_000) / 1_000_000 }

func (s *Store) sessionUsageAggregateRows(
	ctx context.Context, f db.UsageFilter, sessionID string,
) ([]duckUsageAggregateRow, error) {
	cte, args := duckUsageCTE(f, sessionID)
	query := cte + `
		SELECT session_id, project, agent, model,
			ANY_VALUE(display_name) AS display_name,
			ANY_VALUE(started_at) AS started_at,
			SUM(input_tokens_norm) AS input_tokens,
			SUM(output_tokens_norm) AS output_tokens,
			SUM(cache_create_norm) AS cache_creation_tokens,
			SUM(cache_read_norm) AS cache_read_tokens,
				SUM(CASE WHEN cost_usd IS NULL OR cost_source = 'copilot-reported' THEN input_tokens_norm ELSE 0 END) AS billable_input_tokens,
				SUM(CASE
					WHEN cost_usd IS NOT NULL AND cost_source != 'copilot-reported' THEN 0
					WHEN output_tokens_norm = 0 THEN reasoning_tokens_norm
					ELSE output_tokens_norm
				END) AS billable_output_tokens,
				CAST(0 AS BIGINT) AS billable_reasoning_tokens,
				SUM(CASE WHEN cost_usd IS NULL OR cost_source = 'copilot-reported' THEN cache_create_norm ELSE 0 END) AS billable_cache_creation_tokens,
			SUM(CASE WHEN cost_usd IS NULL OR cost_source = 'copilot-reported' THEN cache_read_norm ELSE 0 END) AS billable_cache_read_tokens,
			COALESCE(SUM(cost_usd) FILTER (WHERE cost_source != 'copilot-reported'), 0) AS explicit_cost,
			COUNT(cost_usd) FILTER (WHERE cost_source != 'copilot-reported') AS reported_cost_rows,
			COALESCE(SUM(cost_usd) FILTER (WHERE cost_source = 'copilot-reported'), 0) AS authoritative_cost,
			COUNT(cost_usd) FILTER (WHERE cost_source = 'copilot-reported') AS authoritative_cost_rows
		FROM usage_localized
		GROUP BY session_id, project, agent, model
		ORDER BY session_id ASC, model ASC`
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb session usage aggregates: %w", err)
	}
	defer rows.Close()
	var out []duckUsageAggregateRow
	for rows.Next() {
		var r duckUsageAggregateRow
		var startedAt any
		if err := rows.Scan(
			&r.sessionID, &r.project, &r.agent, &r.model,
			&r.displayName, &startedAt,
			&r.inputTok, &r.outputTok, &r.cacheCr, &r.cacheRd,
			&r.billableInput, &r.billableOutput, &r.billableReason,
			&r.billableCacheCr, &r.billableCacheRd,
			&r.explicitCost, &r.reportedCostRows,
			&r.authoritativeCost, &r.authoritativeCostRows,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb session usage aggregate: %w", err)
		}
		r.startedAt = formatDBTime(startedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// sessionUsageRowCount counts the deduped usage rows that would
// contribute breakdown entries, mirroring duckSessionUsageRowCost's
// contributes rule (an explicit cost or any nonzero token counter)
// without shipping the rows.
func (s *Store) sessionUsageRowCount(
	ctx context.Context, sessionID string,
) (int, error) {
	cte, args := duckUsageCTE(db.UsageFilter{}, sessionID)
	query := cte + `
		SELECT COUNT(*)
		FROM usage_localized
		WHERE cost_usd IS NOT NULL
			OR input_tokens_norm != 0
			OR output_tokens_norm != 0
			OR cache_create_norm != 0
			OR cache_read_norm != 0
			OR reasoning_tokens_norm != 0`
	var count int
	if err := s.queryRowContext(ctx, query, args...).
		Scan(&count); err != nil {
		return 0, fmt.Errorf(
			"counting duckdb session usage rows: %w", err)
	}
	return count, nil
}

func (s *Store) sessionUsageRows(
	ctx context.Context, sessionID string,
) ([]duckSessionUsageRow, error) {
	cte, args := duckUsageCTE(db.UsageFilter{}, sessionID)
	query := cte + `
		SELECT session_id, message_ordinal, source, ts, model,
			input_tokens_norm, output_tokens_norm,
			cache_create_norm, cache_read_norm,
			reasoning_tokens_norm, cost_usd, cost_source
		FROM usage_localized
		ORDER BY ts ASC, session_id ASC,
			COALESCE(message_ordinal, -1) ASC,
			source ASC,
			usage_dedup_key ASC`
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb session usage rows: %w", err)
	}
	defer rows.Close()
	var out []duckSessionUsageRow
	for rows.Next() {
		var r duckSessionUsageRow
		var ts any
		if err := rows.Scan(
			&r.sessionID, &r.messageOrdinal, &r.source, &ts, &r.model,
			&r.inputTok, &r.outputTok, &r.cacheCr, &r.cacheRd,
			&r.reasoningTok, &r.costUSD, &r.costSource,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb session usage row: %w", err)
		}
		r.ts = formatDBTime(ts)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetTopSessionsByCost(
	ctx context.Context, f db.UsageFilter, limit int,
) ([]db.TopSessionEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	pricing, err := s.loadPricing(ctx)
	if err != nil {
		return nil, err
	}
	rateResolver := export.NewPricingResolver(duckPricingRows(pricing))
	rows, err := s.sessionUsageAggregateRows(ctx, f, "")
	if err != nil {
		return nil, err
	}
	type acc struct {
		row               db.TopSessionEntry
		tokens            int
		cost              float64
		authoritativeCost *float64
	}
	bySession := map[string]*acc{}
	for _, r := range rows {
		a := bySession[r.sessionID]
		if a == nil {
			a = &acc{row: db.TopSessionEntry{
				SessionID: r.sessionID, DisplayName: r.displayName,
				Agent: r.agent, Project: r.project, StartedAt: r.startedAt,
			}}
			bySession[r.sessionID] = a
		}
		cost, _, _, _ := duckUsageAggregateCost(
			r.model,
			r.inputTok, r.outputTok, r.cacheCr, r.cacheRd,
			r.billableInput, r.billableOutput, r.billableReason,
			r.billableCacheCr, r.billableCacheRd,
			r.explicitCost,
			r.reportedCostRows > 0,
			rateResolver,
		)
		a.tokens += r.inputTok + r.outputTok + r.cacheCr + r.cacheRd
		a.cost += cost
		if f.Model == "" && f.ExcludeModel == "" &&
			r.authoritativeCostRows > 0 {
			v := r.authoritativeCost
			a.authoritativeCost = &v
		}
	}
	out := make([]db.TopSessionEntry, 0, len(bySession))
	for _, a := range bySession {
		a.row.TotalTokens = a.tokens
		if a.authoritativeCost != nil {
			a.row.Cost = *a.authoritativeCost
		} else {
			a.row.Cost = roundCost(a.cost)
		}
		out = append(out, a.row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].SessionID < out[j].SessionID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) GetUsageSessionCounts(
	ctx context.Context, f db.UsageFilter,
) (db.UsageSessionCounts, error) {
	rows, err := s.sessionUsageAggregateRows(ctx, f, "")
	if err != nil {
		return db.UsageSessionCounts{}, err
	}
	type sessionInfo struct {
		project string
		agent   string
	}
	seen := map[string]sessionInfo{}
	for _, r := range rows {
		seen[r.sessionID] = sessionInfo{
			project: r.project,
			agent:   r.agent,
		}
	}
	out := db.UsageSessionCounts{ByProject: map[string]int{}, ByAgent: map[string]int{}}
	for _, r := range seen {
		out.Total++
		out.ByProject[r.project]++
		out.ByAgent[r.agent]++
	}
	return out, nil
}

// appendDuckUsageMatchingActivityClauses requires the session to have at
// least one row that GetUsageMatchingSessionCount's bounded branch would
// count, mirroring appendUsageMatchingActivityClauses in internal/db so
// bounded and unbounded requests agree on which sessions match.
func appendDuckUsageMatchingActivityClauses(
	where string, args []any, f db.UsageFilter,
) (string, []any) {
	var messageArgs []any
	messageWhere, messageArgs := appendDuckUsageSourceFilterClauses(
		duckUsageMatchingMessageSourceEligibility, messageArgs, "m.model", f,
	)
	var eventArgs []any
	eventWhere, eventArgs := appendDuckUsageSourceFilterClauses(
		duckUsageEventSourceEligibility, eventArgs, "ue.model", f,
	)

	where += `
		AND (
			EXISTS (
				SELECT 1
				FROM messages m
				WHERE m.session_id = s.id
					AND ` + messageWhere + `
			)
			OR EXISTS (
				SELECT 1
				FROM usage_events ue
				WHERE ue.session_id = s.id
					AND ` + eventWhere + `
			)
		)`
	args = append(args, messageArgs...)
	args = append(args, eventArgs...)
	return where, args
}

// GetUsageMatchingSessionCount counts sessions that match the usage filter
// even when they have no token-bearing usage rows. Bounded ranges are
// resolved against message/usage_events timestamps (falling back to
// s.started_at), the same shape duckUsageRawSQL already uses for the
// normal usage query, so a session whose activity falls outside the
// window but whose message timestamp falls inside it is still counted.
func (s *Store) GetUsageMatchingSessionCount(
	ctx context.Context, f db.UsageFilter,
) (int, error) {
	if f.From == "" && f.To == "" {
		where, args := appendDuckUsageSessionFilterClauses(
			"s.deleted_at IS NULL", nil, f, "")
		where, args = appendDuckUsageMatchingActivityClauses(where, args, f)

		var count int
		err := s.duck.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM sessions s WHERE `+where, args...).Scan(&count)
		if err != nil {
			return 0, fmt.Errorf("querying matching usage sessions: %w", err)
		}
		return count, nil
	}

	query, args := duckMatchingUsageRawSQL(f)
	rows, err := s.duck.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("querying matching usage sessions: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	for rows.Next() {
		var (
			id string
			ts any
		)
		if err := rows.Scan(&id, &ts); err != nil {
			return 0, fmt.Errorf("scanning matching usage session: %w", err)
		}
		date := analyticsLocalDate(formatDBTime(ts), f.Timezone)
		if date == "" {
			continue
		}
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}
		seen[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating matching usage sessions: %w", err)
	}
	return len(seen), nil
}

func (s *Store) GetSessionUsage(
	ctx context.Context, sessionID string, includeBreakdown bool,
) (*db.SessionUsage, error) {
	sess, err := s.GetSession(ctx, sessionID)
	if err != nil || sess == nil {
		return nil, err
	}
	pricing, err := s.loadPricing(ctx)
	if err != nil {
		return nil, err
	}
	rateResolver := export.NewPricingResolver(duckPricingRows(pricing))
	rows, err := s.sessionUsageAggregateRows(ctx, db.UsageFilter{}, sessionID)
	if err != nil {
		return nil, err
	}
	var breakdownRows []duckSessionUsageRow
	breakdownCount := 0
	if includeBreakdown {
		breakdownRows, err = s.sessionUsageRows(ctx, sessionID)
	} else {
		breakdownCount, err = s.sessionUsageRowCount(ctx, sessionID)
	}
	if err != nil {
		return nil, err
	}
	models := map[string]bool{}
	unpriced := map[string]bool{}
	totalCost := 0.0
	var authoritativeCost *float64
	hasRows := false
	for _, r := range rows {
		hasRows = true
		models[r.model] = true
		if r.authoritativeCostRows > 0 {
			v := r.authoritativeCost
			authoritativeCost = &v
			rateResolver.RecordReported(r.model, rateResolver.Lookup(r.model))
		}
		cost, _, priced, contributes := duckUsageAggregateCost(
			r.model,
			r.inputTok, r.outputTok, r.cacheCr, r.cacheRd,
			r.billableInput, r.billableOutput, r.billableReason,
			r.billableCacheCr, r.billableCacheRd,
			r.explicitCost,
			r.reportedCostRows > 0,
			rateResolver,
		)
		if !contributes {
			continue
		}
		totalCost += cost
		if !priced {
			unpriced[r.model] = true
		}
	}
	breakdown := make([]db.SessionUsageBreakdownEntry, 0, len(breakdownRows))
	for _, r := range breakdownRows {
		cost, priced, contributes := duckSessionUsageRowCost(r, pricing)
		if !contributes {
			continue
		}
		breakdown = append(breakdown, duckSessionUsageBreakdownEntry(
			r, len(breakdown)+1, cost, priced))
	}
	if includeBreakdown {
		breakdownCount = len(breakdown)
	}
	out := &db.SessionUsage{
		SessionID: sessionID, Agent: sess.Agent, Project: sess.Project,
		TotalOutputTokens: sess.TotalOutputTokens,
		PeakContextTokens: sess.PeakContextTokens,
		HasTokenData:      hasRows || sess.HasTotalOutputTokens || sess.HasPeakContextTokens,
		Models:            sortedBoolKeys(models),
		UnpricedModels:    sortedBoolKeys(unpriced),
		BreakdownCount:    breakdownCount,
		Breakdown:         breakdown,
	}
	if authoritativeCost != nil {
		out.HasCost = true
		out.CostUSD = *authoritativeCost
		out.AICredits = db.AICreditsFromCost(sess.Agent, out.CostUSD)
	} else if len(unpriced) == 0 && hasRows {
		out.HasCost = true
		out.CostUSD = roundCost(totalCost)
		out.AICredits = db.AICreditsFromCost(sess.Agent, out.CostUSD)
	}
	return out, nil
}
