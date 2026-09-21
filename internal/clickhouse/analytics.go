package clickhouse

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/signals"
)

type chAnalyticsSession struct {
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
) ([]chAnalyticsSession, error) {
	return s.analyticsSessionsFiltered(ctx, f, true, true, "", nil)
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
	extraPred string, extraArgs []any,
) ([]chAnalyticsSession, error) {
	if includeTime && f.HasTimeFilter() && strings.TrimSpace(f.Model) != "" {
		return s.analyticsSessionsModelTimeFiltered(ctx, f, includeDate)
	}
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.",
		includeDate, includeTime)
	if extraPred != "" {
		where += " AND " + extraPred
		args = append(args, extraArgs...)
	}
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
		return nil, fmt.Errorf("querying clickhouse analytics sessions: %w", err)
	}
	defer rows.Close()

	var out []chAnalyticsSession
	for rows.Next() {
		var r chAnalyticsSession
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
			return nil, fmt.Errorf("scanning clickhouse analytics session: %w", err)
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
) ([]chAnalyticsSession, error) {
	sessions, err := s.analyticsSessionsFiltered(ctx, f, includeDate, false, "", nil)
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
	out := make([]chAnalyticsSession, 0, len(sessions))
	for _, session := range sessions {
		if _, ok := matched[session.id]; ok {
			out = append(out, session)
		}
	}
	return out, nil
}

func chBuildAnalyticsWhere(
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
			preds = append(preds, dateCol+" >= "+chTimestampSQL)
			args = append(args, chUsagePaddedUTCBound(f.From+"T00:00:00Z", -14))
		}
		if f.To != "" {
			preds = append(preds, dateCol+" <= "+chTimestampSQL)
			args = append(args, chUsagePaddedUTCBound(f.To+"T23:59:59Z", 14))
		}
		localDate, localDateArgs := chAnalyticsLocalDateExpr(dateCol, f)
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
		preds, args = appendChAnalyticsCSVFilter(preds, args, q("machine"), f.Machine)
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
		preds, args = appendChAnalyticsCSVFilter(preds, args, q("agent"), f.Agent)
	}
	if modelPred, modelArgs := chAnalyticsCSVPredicate("m.model", f.Model); modelPred != "" {
		// ClickHouse 25.8 correlated EXISTS is unreliable; an IN subquery
		// matches sessions that have at least one message with the model.
		preds = append(preds,
			q("id")+" IN (SELECT m.session_id FROM messages m WHERE "+modelPred+")")
		args = append(args, modelArgs...)
	}
	if f.MinUserMessages > 0 {
		preds = append(preds, q("user_message_count")+" >= ?")
		args = append(args, f.MinUserMessages)
	}
	scope := chNormalizeAutomatedScope(
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
			preds = append(preds, oneShot("("+q("user_message_count")+" > 1 OR "+q("is_automated")+" = true)"))
		} else {
			preds = append(preds, oneShot(q("user_message_count")+" > 1"))
		}
	}
	if pred := chAutomatedScopePredicate(
		scope, q("is_automated")); pred != "" {
		preds = append(preds, pred)
	}
	if f.ExcludeInteractive {
		preds = append(preds, q("is_automated")+" = true")
	}
	if f.ActiveSince != "" {
		activeSince := f.ActiveSince
		if parsed, ok := parseAnalyticsTime(f.ActiveSince); ok {
			activeSince = parsed.Format(time.RFC3339)
		}
		preds = append(preds,
			"COALESCE("+q("ended_at")+", "+q("started_at")+", "+q("created_at")+") >= "+chTimestampSQL)
		args = append(args, activeSince)
	}
	if pred, predArgs := chTerminationPred(
		f.Termination,
		"COALESCE("+q("ended_at")+", "+q("started_at")+", "+q("created_at")+")",
		q("termination_status"),
	); pred != "" {
		preds = append(preds, pred)
		args = append(args, predArgs...)
	}
	if includeTime && (f.DayOfWeek != nil || f.Hour != nil) {
		pred, predArgs := chAnalyticsMessageTimeExists(f, q("id"))
		preds = append(preds, pred)
		args = append(args, predArgs...)
	}

	return strings.Join(preds, " AND "), args
}

func appendChAnalyticsCSVFilter(
	preds []string, args []any, col string, raw string,
) ([]string, []any) {
	pred, predArgs := chAnalyticsCSVPredicate(col, raw)
	if pred != "" {
		preds = append(preds, pred)
		args = append(args, predArgs...)
	}
	return preds, args
}

func chAnalyticsCSVValues(raw string) []string {
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

func chAnalyticsCSVPredicate(
	col string, raw string,
) (string, []any) {
	values := chAnalyticsCSVValues(raw)
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

func chAnalyticsLocalDateExpr(
	tsExpr string, f db.AnalyticsFilter,
) (string, []any) {
	if f.Timezone != "" {
		return "formatDateTime(" + tsExpr + ", '%Y-%m-%d', ?)", []any{f.Timezone}
	}
	return "formatDateTime(" + tsExpr + ", '%Y-%m-%d')", nil
}

// chAnalyticsDayOfWeekExpr is Monday=0..Sunday=6, matching DuckDB
// ((strftime('%w') + 6) % 7). ClickHouse toDayOfWeek mode 1 uses that mapping.
func chAnalyticsDayOfWeekExpr(tsExpr string, f db.AnalyticsFilter) (string, []any) {
	if f.Timezone != "" {
		return "toDayOfWeek(" + tsExpr + ", 1, ?)", []any{f.Timezone}
	}
	return "toDayOfWeek(" + tsExpr + ", 1)", nil
}

func chAnalyticsHourExpr(tsExpr string, f db.AnalyticsFilter) (string, []any) {
	if f.Timezone != "" {
		return "toHour(" + tsExpr + ", ?)", []any{f.Timezone}
	}
	return "toHour(" + tsExpr + ")", nil
}

func chAnalyticsMessageTimeExists(
	f db.AnalyticsFilter, sessionIDExpr string,
) (string, []any) {
	preds := []string{
		"m.timestamp IS NOT NULL",
	}
	var args []any
	if modelPred, modelArgs := chAnalyticsCSVPredicate("m.model", f.Model); modelPred != "" {
		preds = append(preds, modelPred)
		args = append(args, modelArgs...)
	}
	if f.DayOfWeek != nil {
		expr, exprArgs := chAnalyticsDayOfWeekExpr("m.timestamp", f)
		preds = append(preds, expr+" = ?")
		args = append(args, append(exprArgs, *f.DayOfWeek)...)
	}
	if f.Hour != nil {
		expr, exprArgs := chAnalyticsHourExpr("m.timestamp", f)
		preds = append(preds, expr+" = ?")
		args = append(args, append(exprArgs, *f.Hour)...)
	}
	return sessionIDExpr + " IN (SELECT m.session_id FROM messages m WHERE " +
		strings.Join(preds, " AND ") + ")", args
}

func chAnalyticsTimeMatches(t time.Time, f db.AnalyticsFilter) bool {
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

func analyticsDateTime(r chAnalyticsSession) string {
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

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func (s *Store) getAnalyticsModelsForSessionIDs(
	ctx context.Context, sessionIDs []string,
) ([]string, error) {
	if len(sessionIDs) == 0 {
		return []string{}, nil
	}
	models := map[string]bool{}
	err := chQueryChunked(sessionIDs, func(chunk []string) error {
		ph, args := chInPlaceholders(chunk)
		rows, err := s.queryContext(ctx, `
			SELECT DISTINCT model
			FROM messages
			WHERE session_id IN `+ph+`
				AND COALESCE(model, '') <> ''
			ORDER BY model`, args...)
		if err != nil {
			return fmt.Errorf("querying clickhouse analytics models: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var model string
			if err := rows.Scan(&model); err != nil {
				return fmt.Errorf("scanning clickhouse analytics model: %w", err)
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

	filterModels := chAnalyticsCSVValues(f.Model)
	allowedModels := make(map[string]struct{}, len(filterModels))
	for _, model := range filterModels {
		allowedModels[model] = struct{}{}
	}
	loc := analyticsLocation(f.Timezone)
	models := map[string]bool{}
	err := chQueryChunked(unique, func(chunk []string) error {
		ph, args := chInPlaceholders(chunk)
		rows, err := s.queryContext(ctx, `
			SELECT model, timestamp
			FROM messages
			WHERE session_id IN `+ph+`
				AND COALESCE(model, '') <> ''`, args...)
		if err != nil {
			return fmt.Errorf("querying clickhouse filtered analytics models: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var model string
			var ts any
			if err := rows.Scan(&model, &ts); err != nil {
				return fmt.Errorf("scanning clickhouse filtered analytics model: %w", err)
			}
			if len(allowedModels) > 0 {
				if _, ok := allowedModels[model]; !ok {
					continue
				}
			}
			if f.HasTimeFilter() {
				t, ok := parseAnalyticsTime(formatDBTime(ts))
				if !ok || !chAnalyticsTimeMatches(t.In(loc), f) {
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
) ([]chAnalyticsSession, error) {
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
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	localDate, localDateArgs := chAnalyticsLocalDateExpr(
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
				toInt64(row_number() OVER (ORDER BY message_count ASC)) AS rn,
				toInt64(COUNT(*) OVER ()) AS n
			FROM filtered
		),
		project_totals AS (
			SELECT project, toInt64(SUM(message_count)) AS messages
			FROM filtered
			GROUP BY project
		)
		SELECT
			toInt64(COUNT(*)) AS total_sessions,
			toInt64(COALESCE(SUM(message_count), 0)) AS total_messages,
			toInt64(COALESCE(sumIf(total_output_tokens, has_total_output_tokens = true), 0)) AS total_output_tokens,
			toInt64(countIf(has_total_output_tokens = true)) AS token_reporting_sessions,
			toInt64(COUNT(DISTINCT project)) AS active_projects,
			toInt64(COUNT(DISTINCT local_date)) AS active_days,
			ifNotFinite(round(avg(message_count), 1), 0) AS avg_messages,
			COALESCE((
				SELECT toInt64(ifNotFinite(floor(avg(message_count)), 0))
				FROM ranked
				WHERE rn = toInt64(floor((n + 1) / 2.0))
					OR rn = toInt64(floor((n + 2) / 2.0))
			), 0) AS median_messages,
			COALESCE((
				SELECT message_count
				FROM ranked
				WHERE rn = least(toInt64(floor(n * 0.9)) + 1, n)
				LIMIT 1
			), 0) AS p90_messages,
			COALESCE((
				SELECT project
				FROM project_totals
				ORDER BY messages DESC, project ASC
				LIMIT 1
			), '') AS most_active,
			ifNotFinite(round(CAST((
				SELECT SUM(messages)
				FROM (
					SELECT messages
					FROM project_totals
					ORDER BY messages DESC
					LIMIT 3
				)
			) AS Float64) / NULLIF(SUM(message_count), 0), 3), 0) AS concentration
		FROM filtered`
	rows, err := s.queryContext(ctx, query, queryArgs...)
	if err != nil {
		return db.AnalyticsSummary{}, fmt.Errorf("querying clickhouse analytics summary: %w", err)
	}
	defer rows.Close()
	resp := db.AnalyticsSummary{Agents: map[string]*db.AgentSummary{}}
	if !rows.Next() {
		return resp, rows.Err()
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
		return db.AnalyticsSummary{}, fmt.Errorf("scanning clickhouse analytics summary: %w", err)
	}
	if err := rows.Err(); err != nil {
		return db.AnalyticsSummary{}, fmt.Errorf("iterating clickhouse analytics summary: %w", err)
	}
	if err := rows.Close(); err != nil {
		return db.AnalyticsSummary{}, fmt.Errorf("closing clickhouse analytics summary rows: %w", err)
	}

	agentRows, err := s.queryContext(ctx, `
		WITH filtered AS (
			SELECT s.agent, s.message_count
			FROM sessions s
			WHERE `+where+`
		)
		SELECT agent, toInt64(COUNT(*)), toInt64(COALESCE(SUM(message_count), 0))
		FROM filtered
		GROUP BY agent`,
		args...,
	)
	if err != nil {
		return db.AnalyticsSummary{}, fmt.Errorf("querying clickhouse analytics summary agents: %w", err)
	}
	defer agentRows.Close()
	for agentRows.Next() {
		var agent string
		var summary db.AgentSummary
		if err := agentRows.Scan(&agent, &summary.Sessions, &summary.Messages); err != nil {
			return db.AnalyticsSummary{}, fmt.Errorf("scanning clickhouse analytics summary agent: %w", err)
		}
		resp.Agents[agent] = &summary
	}
	if err := agentRows.Err(); err != nil {
		return db.AnalyticsSummary{}, fmt.Errorf("iterating clickhouse analytics summary agents: %w", err)
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
	for _, model := range chAnalyticsCSVValues(f.Model) {
		allowedModels[model] = struct{}{}
	}
	loc := analyticsLocation(f.Timezone)
	err := chQueryChunked(sessionIDs, func(chunk []string) error {
		ph, args := chInPlaceholders(chunk)
		rows, err := s.queryContext(ctx, `
			SELECT tc.session_id, m.model, m.timestamp, toInt64(COUNT(*))
			FROM tool_calls tc
			JOIN messages m
				ON m.session_id = tc.session_id
				AND m.ordinal = tc.message_ordinal
			WHERE tc.session_id IN `+ph+`
			GROUP BY tc.session_id, m.model, m.timestamp`, args...)
		if err != nil {
			return fmt.Errorf(
				"querying clickhouse filtered analytics tool calls: %w",
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
					"scanning clickhouse filtered analytics tool calls: %w",
					err,
				)
			}
			if _, ok := allowedModels[model]; !ok {
				continue
			}
			if f.HasTimeFilter() {
				t, ok := parseAnalyticsTime(formatDBTime(ts))
				if !ok || !chAnalyticsTimeMatches(t.In(loc), f) {
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
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	localDate, localDateArgs := chAnalyticsLocalDateExpr(
		"COALESCE(s.started_at, s.created_at)", f)
	bucketExpr := chAnalyticsBucketExpr("local_date", granularity)
	queryArgs := append([]any{}, localDateArgs...)
	queryArgs = append(queryArgs, args...)
	if _, modelArgs := chAnalyticsCSVPredicate("m.model", f.Model); len(modelArgs) > 0 {
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
				toInt64(COUNT(*)) AS sessions
			FROM filtered_sessions
			GROUP BY bucket
		),
		message_rows AS (
			SELECT `+bucketExpr+` AS bucket,
				toInt64(COUNT(*)) AS messages,
				toInt64(countIf(m.role = 'user' AND m.is_system = false
					AND COALESCE(m.source_subtype, '') != 'tool_result')) AS user_messages,
				toInt64(countIf(m.role = 'assistant')) AS assistant_messages,
				toInt64(countIf(m.has_thinking = true)) AS thinking_messages
			FROM filtered_sessions fs
			JOIN messages m ON m.session_id = fs.id
			`+chAnalyticsMessageFilterClause("m.model", f.Model)+`
			GROUP BY bucket
		),
		tool_rows AS (
			SELECT `+bucketExpr+` AS bucket, toInt64(COUNT(*)) AS tool_calls
			FROM filtered_sessions fs
			JOIN tool_calls tc ON tc.session_id = fs.id
			`+chAnalyticsToolMessageJoin("tc", f.Model)+`
			`+chAnalyticsMessageFilterClause("m.model", f.Model)+`
			GROUP BY bucket
		)
		SELECT bucket,
			toInt64(sum(sessions)) AS sessions,
			toInt64(sum(messages)) AS messages,
			toInt64(sum(user_messages)) AS user_messages,
			toInt64(sum(assistant_messages)) AS assistant_messages,
			toInt64(sum(thinking_messages)) AS thinking_messages,
			toInt64(sum(tool_calls)) AS tool_calls
		FROM (
			SELECT bucket, sessions,
				toInt64(0) AS messages, toInt64(0) AS user_messages,
				toInt64(0) AS assistant_messages, toInt64(0) AS thinking_messages,
				toInt64(0) AS tool_calls
			FROM session_rows
			UNION ALL
			SELECT bucket, toInt64(0) AS sessions, messages, user_messages,
				assistant_messages, thinking_messages, toInt64(0) AS tool_calls
			FROM message_rows
			UNION ALL
			SELECT bucket, toInt64(0) AS sessions, toInt64(0) AS messages,
				toInt64(0) AS user_messages, toInt64(0) AS assistant_messages,
				toInt64(0) AS thinking_messages, tool_calls
			FROM tool_rows
		) combined
		GROUP BY bucket
		ORDER BY bucket`,
		queryArgs...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse analytics activity buckets: %w", err)
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
			return nil, fmt.Errorf("scanning clickhouse analytics activity bucket: %w", err)
		}
		buckets[entry.Date] = &entry
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse analytics activity buckets: %w", err)
	}
	return buckets, nil
}

func (s *Store) addActivityAgentCounts(
	ctx context.Context, f db.AnalyticsFilter, granularity string,
	buckets map[string]*db.ActivityEntry,
) error {
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	localDate, localDateArgs := chAnalyticsLocalDateExpr(
		"COALESCE(s.started_at, s.created_at)", f)
	bucketExpr := chAnalyticsBucketExpr("local_date", granularity)
	queryArgs := append([]any{}, localDateArgs...)
	queryArgs = append(queryArgs, args...)
	if _, modelArgs := chAnalyticsCSVPredicate("m.model", f.Model); len(modelArgs) > 0 {
		queryArgs = append(queryArgs, modelArgs...)
	}
	rows, err := s.queryContext(ctx, `
		WITH filtered_sessions AS (
			SELECT s.id, s.agent, `+localDate+` AS local_date
			FROM sessions s
			WHERE `+where+`
		)
		SELECT `+bucketExpr+` AS bucket, fs.agent, toInt64(COUNT(*)) AS messages
		FROM filtered_sessions fs
		JOIN messages m ON m.session_id = fs.id
		`+chAnalyticsMessageFilterClause("m.model", f.Model)+`
		GROUP BY bucket, fs.agent
		ORDER BY bucket, fs.agent`,
		queryArgs...,
	)
	if err != nil {
		return fmt.Errorf("querying clickhouse analytics activity agents: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bucket, agent string
		var count int
		if err := rows.Scan(&bucket, &agent, &count); err != nil {
			return fmt.Errorf("scanning clickhouse analytics activity agent: %w", err)
		}
		if entry, ok := buckets[bucket]; ok {
			entry.ByAgent[agent] = count
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating clickhouse analytics activity agents: %w", err)
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

func chAnalyticsBucketExpr(dateExpr, granularity string) string {
	switch granularity {
	case "week":
		// toStartOfWeek mode 1 starts the week on Monday, matching DuckDB
		// date_trunc('week').
		return "formatDateTime(toStartOfWeek(toDate(" + dateExpr + "), 1), '%Y-%m-%d')"
	case "month":
		return "formatDateTime(toStartOfMonth(toDate(" + dateExpr + ")), '%Y-%m-%d')"
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

func chAnalyticsMessageFilterClause(col, raw string) string {
	pred, _ := chAnalyticsCSVPredicate(col, raw)
	if pred == "" {
		return ""
	}
	return "WHERE " + pred
}

func chAnalyticsAndClause(pred string) string {
	if pred == "" {
		return ""
	}
	return " AND " + pred
}

func chAnalyticsToolMessageJoin(
	toolAlias string, model string,
) string {
	if model == "" {
		return ""
	}
	return `
			JOIN messages m
				ON m.session_id = ` + toolAlias + `.session_id
				AND m.ordinal = ` + toolAlias + `.message_ordinal`
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
		entriesFrom := chClampHeatmapFrom(f.From, f.To)
		values := []int{}
		for date, v := range counts {
			if v > 0 && date >= entriesFrom && date <= f.To {
				values = append(values, v)
			}
		}
		sort.Ints(values)
		levels := chComputeHeatmapLevels(values)
		entries := chBuildHeatmapEntries(entriesFrom, f.To, counts, levels)
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
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	localDate, localDateArgs := chAnalyticsLocalDateExpr(
		"COALESCE(s.started_at, s.created_at)", f)
	valueExpr := "toInt64(COALESCE(SUM(s.message_count), 0))"
	switch metric {
	case "sessions":
		valueExpr = "toInt64(COUNT(*))"
	case "output_tokens":
		where += " AND s.has_total_output_tokens = true"
		valueExpr = "toInt64(COALESCE(SUM(s.total_output_tokens), 0))"
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
		return db.HeatmapResponse{}, fmt.Errorf("querying clickhouse analytics heatmap: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var date string
		var value int
		if err := rows.Scan(&date, &value); err != nil {
			return db.HeatmapResponse{}, fmt.Errorf("scanning clickhouse analytics heatmap: %w", err)
		}
		counts[date] = value
	}
	if err := rows.Err(); err != nil {
		return db.HeatmapResponse{}, fmt.Errorf("iterating clickhouse analytics heatmap: %w", err)
	}
	if metric == "output_tokens" && len(counts) == 0 {
		return db.HeatmapResponse{
			Metric:      metric,
			EntriesFrom: chClampHeatmapFrom(f.From, f.To),
		}, nil
	}
	entriesFrom := chClampHeatmapFrom(f.From, f.To)
	values := []int{}
	for date, v := range counts {
		if v > 0 && date >= entriesFrom && date <= f.To {
			values = append(values, v)
		}
	}
	sort.Ints(values)
	levels := chComputeHeatmapLevels(values)
	entries := chBuildHeatmapEntries(entriesFrom, f.To, counts, levels)
	return db.HeatmapResponse{
		Metric: metric, Entries: entries,
		Levels:      levels,
		EntriesFrom: entriesFrom,
	}, nil
}

const chMaxHeatmapDays = 366

func chClampHeatmapFrom(from, to string) string {
	start, err := time.Parse("2006-01-02", from)
	if err != nil {
		return from
	}
	end, err := time.Parse("2006-01-02", to)
	if err != nil {
		return from
	}
	earliest := end.AddDate(0, 0, -(chMaxHeatmapDays - 1))
	if start.Before(earliest) {
		return earliest.Format("2006-01-02")
	}
	return from
}

func chComputeHeatmapLevels(sorted []int) db.HeatmapLevels {
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

func chHeatmapLevel(value int, levels db.HeatmapLevels) int {
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

func chBuildHeatmapEntries(
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
			Level: chHeatmapLevel(v, levels),
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
	where, args := chBuildAnalyticsWhere(
		sessionFilter, "COALESCE(s.started_at, s.created_at)", "s.", true, false)
	dowExpr, dowArgs := chAnalyticsDayOfWeekExpr("m.timestamp", f)
	hourExpr, hourArgs := chAnalyticsHourExpr("m.timestamp", f)
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, dowArgs...)
	queryArgs = append(queryArgs, hourArgs...)
	rows, err := s.queryContext(ctx, `
		WITH filtered_sessions AS (
			SELECT s.id
			FROM sessions s
			WHERE `+where+`
		),
		message_buckets AS (
			SELECT toInt64(`+dowExpr+`) AS day_of_week,
				toInt64(`+hourExpr+`) AS hour
			FROM messages m
			JOIN filtered_sessions fs ON fs.id = m.session_id
			WHERE m.timestamp IS NOT NULL
		)
		SELECT day_of_week, hour, toInt64(COUNT(*))
		FROM message_buckets
		GROUP BY day_of_week, hour
		ORDER BY day_of_week, hour`,
		queryArgs...,
	)
	if err != nil {
		return db.HourOfWeekResponse{}, fmt.Errorf("querying clickhouse analytics hour-of-week: %w", err)
	}
	defer rows.Close()
	var grid [7][24]int
	for rows.Next() {
		var day, hour, messages int
		if err := rows.Scan(&day, &hour, &messages); err != nil {
			return db.HourOfWeekResponse{}, fmt.Errorf("scanning clickhouse analytics hour-of-week: %w", err)
		}
		if day < 0 || day > 6 || hour < 0 || hour > 23 {
			continue
		}
		grid[day][hour] = messages
	}
	if err := rows.Err(); err != nil {
		return db.HourOfWeekResponse{}, fmt.Errorf("iterating clickhouse analytics hour-of-week: %w", err)
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
	sessions, err := s.analyticsSessionsFiltered(ctx, f, true, false, "", nil)
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
	switch {
	case len(ids) == 0:
	case modelFilter:
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
	default:
		autonomy, err = s.analyticsAutonomyBuckets(ctx, chAnalyticsSessionSet(f))
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
	ctx context.Context, sessions chSessionSet,
) (map[string]int, error) {
	counts := map[string]int{}
	sessionIn, args := sessions.in("session_id")
	rows, err := s.queryContext(ctx, `
		SELECT session_id,
			toInt64(countIf(role = 'user' AND is_system = false
				AND COALESCE(source_subtype, '') != 'tool_result')) AS user_count,
			toInt64(countIf(role = 'assistant' AND has_tool_use = true)) AS tool_count
		FROM messages
		WHERE `+sessionIn+`
		GROUP BY session_id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse autonomy: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var userCount, toolCount int
		if err := rows.Scan(&sessionID, &userCount, &toolCount); err != nil {
			return nil, fmt.Errorf("scanning clickhouse autonomy: %w", err)
		}
		if userCount > 0 {
			counts[autonomyBucket(float64(toolCount)/float64(userCount))]++
		}
	}
	return counts, rows.Err()
}

// chMaxSQLVars bounds the IN-list size per query to stay well under
// driver bind-variable limits; larger ID sets are split into chunks.
const chMaxSQLVars = 900

func chInPlaceholders(ids []string) (string, []any) {
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	return "(" + strings.Join(ph, ",") + ")", args
}

// chSessionSet is a relation of session IDs that a query embeds as
// `col IN (...)`. clickhouse-go inlines every bound argument into the
// statement text and the server rejects statements over max_query_size
// (256 KiB by default), so sets the database already knows are selected in
// SQL instead of being round-tripped through Go as placeholder lists.
type chSessionSet struct {
	body string
	args []any
}

// chSessionSetFromWhere selects session IDs with a WHERE clause written
// against the alias `s`.
func chSessionSetFromWhere(where string, args []any) chSessionSet {
	return chSessionSet{
		body: "SELECT s.id FROM sessions s WHERE " + where,
		args: args,
	}
}

// chSessionSetFromIDs embeds an explicit ID list. It exists for callers
// that pin exact sessions, such as contract tests; the list still grows the
// statement, so production paths derive the set in SQL instead.
func chSessionSetFromIDs(ids []string) chSessionSet {
	ph, args := chInPlaceholders(ids)
	return chSessionSet{body: "SELECT arrayJoin([" + strings.Trim(ph, "()") + "]) AS id", args: args}
}

// in returns `col IN (...)` with a fresh copy of the bound arguments so
// callers can embed the set more than once in one statement.
func (set chSessionSet) in(col string) (string, []any) {
	return col + " IN (" + set.body + ")", slices.Clone(set.args)
}

// chAnalyticsSessionSet is the SQL form of analyticsSessions for filters
// without a model, which is the only case analyticsSessions answers from
// chBuildAnalyticsWhere alone.
func chAnalyticsSessionSet(f db.AnalyticsFilter) chSessionSet {
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	return chSessionSetFromWhere(where, args)
}

func chQueryChunked(ids []string, fn func(chunk []string) error) error {
	for i := 0; i < len(ids); i += chMaxSQLVars {
		end := min(i+chMaxSQLVars, len(ids))
		if err := fn(ids[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetAnalyticsTools(
	ctx context.Context, f db.AnalyticsFilter,
) (db.ToolsAnalyticsResponse, error) {
	sessionPred, sessionArgs := chAnalyticsToolSessionWindow(f)
	sessions, err := s.analyticsSessionsFiltered(
		ctx, f, false, false, sessionPred, sessionArgs,
	)
	if err != nil {
		return db.ToolsAnalyticsResponse{}, err
	}
	meta := map[string]chAnalyticsSession{}
	var ids []string
	for _, r := range sessions {
		meta[r.id] = r
		ids = append(ids, r.id)
	}
	if len(ids) == 0 {
		return db.BuildToolsAnalytics(nil), nil
	}
	var toolRows []db.ToolAnalyticsRow
	err = chQueryChunked(ids, func(chunk []string) error {
		ph, args := chInPlaceholders(chunk)
		modelPred, modelArgs := chAnalyticsCSVPredicate("m.model", f.Model)
		args = append(args, modelArgs...)
		from, to := chAnalyticsWindowBounds(f)
		windowPred, windowArgs := chAnalyticsMessageWindowPred("m.timestamp", from, to)
		args = append(args, windowArgs...)
		query := `SELECT tc.session_id, tc.category,
				trim(COALESCE(tc.tool_name, '')), toInt64(COUNT(*)),
				MAX(m.timestamp)
				FROM tool_calls tc
				LEFT JOIN messages m
					ON m.session_id = tc.session_id
					AND m.ordinal = tc.message_ordinal
				WHERE tc.session_id IN ` + ph
		if modelPred != "" {
			query += `
				AND ` + modelPred
		}
		query += chAnalyticsAndClause(windowPred)
		query += `
				GROUP BY tc.session_id, tc.category,
					trim(COALESCE(tc.tool_name, '')), toStartOfMinute(m.timestamp)`
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
	sessionPred, sessionArgs := chAnalyticsToolSessionWindow(f)
	sessions, err := s.analyticsSessionsFiltered(ctx, f, false, false, sessionPred, sessionArgs)
	if err != nil {
		return db.SkillsAnalyticsResponse{}, err
	}
	meta := map[string]chAnalyticsSession{}
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
	err = chQueryChunked(ids, func(chunk []string) error {
		ph, args := chInPlaceholders(chunk)
		modelPred, modelArgs := chAnalyticsCSVPredicate("m.model", f.Model)
		args = append(args, modelArgs...)
		from, to := chAnalyticsWindowBounds(f)
		windowPred, windowArgs := chAnalyticsMessageWindowPred("m.timestamp", from, to)
		args = append(args, windowArgs...)
		rows, qErr := s.queryContext(ctx,
			`SELECT tc.session_id, trim(COALESCE(tc.skill_name, '')),
				toInt64(COUNT(*)), MAX(m.timestamp)
				FROM tool_calls tc
				LEFT JOIN messages m
					ON m.session_id = tc.session_id
					AND m.ordinal = tc.message_ordinal
				WHERE tc.session_id IN `+ph+`
					AND trim(COALESCE(tc.skill_name, '')) != ''
					`+chAnalyticsAndClause(modelPred)+chAnalyticsAndClause(windowPred)+`
				GROUP BY tc.session_id, trim(COALESCE(tc.skill_name, '')),
					toStartOfMinute(m.timestamp)`, args...)
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
	sessionInfo := make(map[string]chVelocitySession, len(sessions))
	for _, sess := range sessions {
		sessionIDs = append(sessionIDs, sess.id)
		sessionInfo[sess.id] = chVelocitySession{
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

	var sessionMsgs map[string][]chVelocityMsg
	if strings.TrimSpace(f.Model) != "" {
		sessionMsgs, err = s.filteredVelocityMessages(
			ctx, sessionIDs, f,
		)
	} else {
		sessionMsgs, err = s.velocityMessages(
			ctx, chAnalyticsSessionSet(f), analyticsLocation(f.Timezone),
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
		toolCounts, err = s.velocityToolCounts(ctx, chAnalyticsSessionSet(f))
	}
	if err != nil {
		return db.VelocityResponse{}, err
	}

	overall := &chVelocityAccumulator{}
	byAgent := make(map[string]*chVelocityAccumulator)
	byComplexity := make(map[string]*chVelocityAccumulator)
	for _, sid := range sessionIDs {
		msgs := sessionMsgs[sid]
		if len(msgs) < 2 {
			continue
		}
		info := sessionInfo[sid]
		agentKey := info.agent
		compKey := chComplexityBucket(info.mc)
		if byAgent[agentKey] == nil {
			byAgent[agentKey] = &chVelocityAccumulator{}
		}
		if byComplexity[compKey] == nil {
			byComplexity[compKey] = &chVelocityAccumulator{}
		}
		processChSessionVelocity(
			[]*chVelocityAccumulator{overall, byAgent[agentKey], byComplexity[compKey]},
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

type chVelocitySession struct {
	agent string
	mc    int
}

type chVelocityMsg struct {
	role          string
	ts            time.Time
	valid         bool
	contentLength int
}

type chVelocityAccumulator struct {
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
	sessions chSessionSet,
	loc *time.Location,
) (map[string][]chVelocityMsg, error) {
	out := make(map[string][]chVelocityMsg)
	sessionIn, args := sessions.in("session_id")
	rows, err := s.queryContext(ctx, `
		SELECT session_id, ordinal, role, timestamp, content_length
		FROM messages
		WHERE `+sessionIn+`
		ORDER BY session_id, ordinal`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse velocity messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid, role string
		var ordinal int
		var ts any
		var contentLength int
		if err := rows.Scan(&sid, &ordinal, &role, &ts, &contentLength); err != nil {
			return nil, fmt.Errorf("scanning clickhouse velocity message: %w", err)
		}
		parsed, ok := chLocalTime(formatDBTime(ts), loc)
		out[sid] = append(out[sid], chVelocityMsg{
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
) (map[string][]chVelocityMsg, error) {
	out := make(map[string][]chVelocityMsg, len(sessionIDs))
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
			out[sessionID] = append(out[sessionID], chVelocityMsg{
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
	sessions chSessionSet,
) (map[string]int, error) {
	out := make(map[string]int)
	sessionIn, args := sessions.in("session_id")
	rows, err := s.queryContext(ctx, `
		SELECT session_id, toInt64(COUNT(*))
		FROM tool_calls
		WHERE `+sessionIn+`
		GROUP BY session_id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse velocity tool calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid string
		var count int
		if err := rows.Scan(&sid, &count); err != nil {
			return nil, fmt.Errorf("scanning clickhouse velocity tool call count: %w", err)
		}
		out[sid] = count
	}
	return out, rows.Err()
}

func chLocalTime(ts string, loc *time.Location) (time.Time, bool) {
	t, ok := parseAnalyticsTime(ts)
	if !ok {
		return time.Time{}, false
	}
	return t.In(loc), true
}

func chComplexityBucket(mc int) string {
	switch {
	case mc <= 15:
		return "1-15"
	case mc <= 60:
		return "16-60"
	default:
		return "61+"
	}
}

func processChSessionVelocity(
	accums []*chVelocityAccumulator,
	msgs []chVelocityMsg,
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

	var firstUser, firstAsst *chVelocityMsg
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

func (a *chVelocityAccumulator) computeOverview() db.VelocityOverview {
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
				DurationMin:       chSessionDurationMinutes(sessions[i]),
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
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, includeTime)
	durationSelectExpr := `COALESCE(toFloat64(toUnixTimestamp64Micro(s.ended_at) - toUnixTimestamp64Micro(s.started_at)) / 60000000.0, 0)`
	activeDurationSelectExpr := "COALESCE(ad.active_duration_min, 0)"
	orderExpr := "s.message_count DESC, s.id ASC"
	switch metric {
	case "duration":
		where += " AND s.started_at IS NOT NULL AND s.ended_at IS NOT NULL AND s.ended_at >= s.started_at"
		orderExpr = activeDurationSelectExpr + " DESC, s.id ASC"
	case "output_tokens":
		where += " AND s.has_total_output_tokens = true"
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
		SELECT s.id, s.project, s.first_message,
			COALESCE(s.display_name, s.session_name) AS display_name,
			s.message_count,
			s.total_output_tokens, ` + durationSelectExpr + ` AS duration_min,
			` + activeDurationSelectExpr + ` AS active_duration_min,
			s.started_at, s.ended_at, s.termination_status
		FROM sessions s
		LEFT JOIN (
			SELECT session_id,
				COALESCE(sum(
					CASE
						WHEN delta_ms <= 0 THEN 0
						WHEN delta_ms > ` + strconv.Itoa(db.ActiveGapCapMs) + ` THEN ` + strconv.Itoa(db.ActiveGapCapMs) + `
						ELSE delta_ms
					END
				), 0) / 60000.0 AS active_duration_min
			FROM (
				SELECT session_id,
					toInt64(round(
						(toUnixTimestamp64Micro(leadInFrame(timestamp) OVER (
							PARTITION BY session_id ORDER BY ordinal
							ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING
						)) - toUnixTimestamp64Micro(timestamp)) / 1000.0
					)) AS delta_ms
				FROM messages
			)
			GROUP BY session_id
		) ad ON ad.session_id = s.id
		WHERE ` + where + `
		ORDER BY ` + orderExpr + limitClause
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return db.TopSessionsResponse{}, fmt.Errorf("querying clickhouse analytics top sessions: %w", err)
	}
	defer rows.Close()

	out := db.TopSessionsResponse{Metric: metric}
	for rows.Next() {
		var row db.TopSession
		var startedRaw, endedRaw any
		if err := rows.Scan(
			&row.ID, &row.Project, &row.FirstMessage, &row.DisplayName,
			&row.MessageCount,
			&row.OutputTokens, &row.DurationMin, &row.ActiveDurationMin,
			&startedRaw, &endedRaw,
			&row.TerminationStatus,
		); err != nil {
			return db.TopSessionsResponse{}, fmt.Errorf("scanning clickhouse analytics top session: %w", err)
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
		return db.TopSessionsResponse{}, fmt.Errorf("iterating clickhouse analytics top sessions: %w", err)
	}
	return out, nil
}

func chSessionDurationMinutes(session chAnalyticsSession) float64 {
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
	rows := chSignalRowsFromSessions(sessions, f)
	if err := s.chPopulateFrustrationMarkers(ctx, rows); err != nil {
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
	rows := chSignalRowsFromSessions(sessions, f)
	if err := s.chPopulateFrustrationMarkers(ctx, rows); err != nil {
		return db.SignalSessionsResponse{}, err
	}
	candidates := db.SignalCandidates(rows, signal, limit)
	messages, err := s.chSignalMessages(ctx, candidates, f)
	if err != nil {
		return db.SignalSessionsResponse{}, err
	}
	return db.SignalSessionsResponse{
		Signal:   signal,
		Sessions: db.BuildSignalExamples(candidates, messages, signal),
	}, nil
}

func chSignalRowsFromSessions(
	sessions []chAnalyticsSession,
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

func (s *Store) chPopulateFrustrationMarkers(
	ctx context.Context,
	rows []db.SignalRow,
) error {
	if len(rows) == 0 {
		return nil
	}
	idx := make(map[string]int, len(rows))
	ids := make([]string, len(rows))
	for i := range rows {
		idx[rows[i].ID] = i
		ids[i] = rows[i].ID
	}
	return chQueryChunked(ids, func(chunk []string) error {
		ph, args := chInPlaceholders(chunk)
		q := `SELECT session_id, content, is_system
			FROM messages
			WHERE role = 'user' AND COALESCE(source_subtype, '') <> 'tool_result' AND session_id IN ` + ph
		msgRows, err := s.queryContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("querying clickhouse frustration markers: %w", err)
		}
		defer msgRows.Close()
		for msgRows.Next() {
			var sessionID, content string
			var isSystem bool
			if err := msgRows.Scan(
				&sessionID, &content, &isSystem,
			); err != nil {
				return fmt.Errorf("scanning clickhouse frustration marker: %w", err)
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
			return fmt.Errorf("iterating clickhouse frustration markers: %w", err)
		}
		return nil
	})
}

func (s *Store) chSignalMessages(
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
						SessionID:     row.SessionID,
						Ordinal:       row.Ordinal,
						Role:          row.Role,
						SourceSubtype: row.SourceSubtype,
						Content:       row.Content,
						Timestamp:     row.Timestamp,
						IsSystem:      row.IsSystem,
						HasToolUse:    row.HasToolUse,
					})
				}
			}
		}
		return out, nil
	}
	placeholders := make([]string, len(rows))
	args := make([]any, 0, len(rows))
	for i, r := range rows {
		placeholders[i] = "?"
		args = append(args, r.ID)
	}
	filterModels := chAnalyticsCSVValues(f.Model)
	q := `SELECT session_id, ordinal, role, content,
			timestamp, is_system, has_tool_use, COALESCE(source_subtype, '')
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
		return nil, fmt.Errorf("querying clickhouse signal messages: %w", err)
	}
	defer msgRows.Close()
	for msgRows.Next() {
		var m db.SignalMessage
		var ts any
		if err := msgRows.Scan(
			&m.SessionID, &m.Ordinal, &m.Role,
			&m.Content, &ts,
			&m.IsSystem, &m.HasToolUse, &m.SourceSubtype,
		); err != nil {
			return nil, fmt.Errorf("scanning clickhouse signal message: %w", err)
		}
		m.Timestamp = formatDBTime(ts)
		out[m.SessionID] = append(out[m.SessionID], m)
	}
	if err := msgRows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse signal messages: %w", err)
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
			AND m.is_system = false
			AND `+db.ClickHouseSystemPrefixSQL("m.content", "m.role")+`
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

func chAnalyticsWindowBounds(f db.AnalyticsFilter) (string, string) {
	var from, to string
	if f.From != "" {
		from = chUsagePaddedUTCBound(f.From+"T00:00:00Z", -14)
	}
	if f.To != "" {
		to = chUsagePaddedUTCBound(f.To+"T23:59:59Z", 14)
		if t, err := time.Parse(time.RFC3339, to); err == nil {
			to = t.Add(time.Second).Format(time.RFC3339)
		}
	}
	return from, to
}

func chAnalyticsMessageWindowPred(col, from, to string) (string, []any) {
	var preds []string
	var args []any
	if from != "" {
		preds = append(preds, col+" >= "+chTimestampSQL)
		args = append(args, from)
	}
	if to != "" {
		preds = append(preds, col+" < "+chTimestampSQL)
		args = append(args, to)
	}
	if len(preds) == 0 {
		return "", nil
	}
	return "(" + col + " IS NULL OR (" + strings.Join(preds, " AND ") + "))", args
}

func chAnalyticsToolSessionWindow(f db.AnalyticsFilter) (string, []any) {
	from, to := chAnalyticsWindowBounds(f)
	sessionPred, args := chAnalyticsMessageWindowPred("COALESCE(s.started_at, s.created_at)", from, to)
	if sessionPred == "" {
		return "", nil
	}
	messagePred, messageArgs := chAnalyticsMessageWindowPred("wm.timestamp", from, to)
	return "(" + sessionPred + " OR s.id IN (SELECT wm.session_id FROM messages wm WHERE " + messagePred + "))", append(args, messageArgs...)
}
