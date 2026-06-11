package duckdb

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

const (
	duckActiveWindow = 10 * time.Minute
	duckStaleWindow  = 60 * time.Minute
)

type duckAnalyticsSession struct {
	id                   string
	project              string
	machine              string
	agent                string
	firstMessage         *string
	displayName          *string
	startedAt            string
	endedAt              string
	createdAt            string
	messageCount         int
	userMessageCount     int
	totalOutputTokens    int
	hasTotalOutputTokens bool
	isAutomated          bool
	terminationStatus    *string
	healthScore          *int
	healthGrade          *string
	outcome              string
	outcomeConfidence    string
	toolFailures         int
	toolRetries          int
	editChurn            int
	compactions          int
	midTaskCompactions   int
	contextPressureMax   *float64
}

func (s *Store) analyticsSessions(
	ctx context.Context, f db.AnalyticsFilter,
) ([]duckAnalyticsSession, error) {
	where, args := duckBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	rows, err := s.duck.QueryContext(ctx, `
		SELECT id, project, machine, agent, first_message,
			COALESCE(display_name, session_name) AS display_name,
			started_at, ended_at, created_at, message_count,
			user_message_count, total_output_tokens,
			has_total_output_tokens, is_automated,
			termination_status, health_score, health_grade, outcome,
			outcome_confidence, tool_failure_signal_count,
			tool_retry_count, edit_churn_count, compaction_count,
			mid_task_compaction_count, context_pressure_max
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
			&r.contextPressureMax,
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
		q("relationship_type") + " NOT IN ('subagent', 'fork')",
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
	if f.Agent != "" {
		preds, args = appendDuckAnalyticsCSVFilter(preds, args, q("agent"), f.Agent)
	}
	if f.MinUserMessages > 0 {
		preds = append(preds, q("user_message_count")+" >= ?")
		args = append(args, f.MinUserMessages)
	}
	if f.ExcludeOneShot {
		if f.ExcludeAutomated {
			preds = append(preds, q("user_message_count")+" > 1")
		} else {
			preds = append(preds, "("+q("user_message_count")+" > 1 OR "+q("is_automated")+" = TRUE)")
		}
	}
	if f.ExcludeAutomated {
		preds = append(preds, q("is_automated")+" = FALSE")
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

func appendDuckAnalyticsCSVFilter(
	preds []string, args []any, col string, raw string,
) ([]string, []any) {
	values := duckAnalyticsCSVValues(raw)
	if len(values) == 0 {
		return preds, args
	}
	if len(values) == 1 {
		preds = append(preds, col+" = ?")
		args = append(args, values[0])
		return preds, args
	}
	placeholders := make([]string, len(values))
	for i, value := range values {
		placeholders[i] = "?"
		args = append(args, value)
	}
	preds = append(preds, col+" IN ("+strings.Join(placeholders, ",")+")")
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

func (s *Store) GetAnalyticsSummary(
	ctx context.Context, f db.AnalyticsFilter,
) (db.AnalyticsSummary, error) {
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
	rows, err := s.duck.QueryContext(ctx, query, queryArgs...)
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

	agentRows, err := s.duck.QueryContext(ctx, `
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
	return resp, nil
}

func (s *Store) GetAnalyticsActivity(
	ctx context.Context, f db.AnalyticsFilter, granularity string,
) (db.ActivityResponse, error) {
	if granularity == "" {
		granularity = "day"
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
	rows, err := s.duck.QueryContext(ctx, `
		WITH filtered_sessions AS (
			SELECT s.id, s.message_count, `+localDate+` AS local_date
			FROM sessions s
			WHERE `+where+`
		),
		session_rows AS (
			SELECT `+bucketExpr+` AS bucket,
				COUNT(*) AS sessions,
				COALESCE(SUM(message_count), 0) AS messages
			FROM filtered_sessions
			GROUP BY bucket
		),
		message_rows AS (
			SELECT `+bucketExpr+` AS bucket,
				COUNT(*) FILTER (WHERE m.role = 'user' AND m.is_system = FALSE) AS user_messages,
				COUNT(*) FILTER (WHERE m.role = 'assistant') AS assistant_messages,
				COUNT(*) FILTER (WHERE m.has_thinking = TRUE) AS thinking_messages
			FROM filtered_sessions fs
			JOIN messages m ON m.session_id = fs.id
			GROUP BY bucket
		),
		tool_rows AS (
			SELECT `+bucketExpr+` AS bucket, COUNT(*) AS tool_calls
			FROM filtered_sessions fs
			JOIN tool_calls tc ON tc.session_id = fs.id
			GROUP BY bucket
		)
		SELECT COALESCE(sr.bucket, mr.bucket, tr.bucket) AS bucket,
			COALESCE(sr.sessions, 0) AS sessions,
			COALESCE(sr.messages, 0) AS messages,
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
	rows, err := s.duck.QueryContext(ctx, `
		WITH filtered_sessions AS (
			SELECT s.id, s.agent, `+localDate+` AS local_date
			FROM sessions s
			WHERE `+where+`
		)
		SELECT `+bucketExpr+` AS bucket, fs.agent, COUNT(*) AS messages
		FROM filtered_sessions fs
		JOIN messages m ON m.session_id = fs.id
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

func (s *Store) GetAnalyticsHeatmap(
	ctx context.Context, f db.AnalyticsFilter, metric string,
) (db.HeatmapResponse, error) {
	if metric == "" {
		metric = "messages"
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
	rows, err := s.duck.QueryContext(ctx, `
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
	sessions, err := s.analyticsSessions(ctx, f)
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
	sessionFilter := f
	sessionFilter.DayOfWeek = nil
	sessionFilter.Hour = nil
	where, args := duckBuildAnalyticsWhere(
		sessionFilter, "COALESCE(s.started_at, s.created_at)", "s.", true, false)
	localTime, localTimeArgs := duckAnalyticsLocalTimeExpr("m.timestamp", f)
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, localTimeArgs...)
	rows, err := s.duck.QueryContext(ctx, `
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
	resp := db.HourOfWeekResponse{Cells: make([]db.HourOfWeekCell, 0, 168)}
	for d := range 7 {
		for h := range 24 {
			resp.Cells = append(resp.Cells, db.HourOfWeekCell{
				DayOfWeek: d, Hour: h, Messages: grid[d][h],
			})
		}
	}
	return resp, nil
}

func (s *Store) GetAnalyticsSessionShape(
	ctx context.Context, f db.AnalyticsFilter,
) (db.SessionShapeResponse, error) {
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.SessionShapeResponse{}, err
	}
	lengths := map[string]int{}
	durations := map[string]int{}
	ids := []string{}
	for _, r := range sessions {
		lengths[lengthBucket(r.messageCount)]++
		ids = append(ids, r.id)
		if start, okS := parseAnalyticsTime(r.startedAt); okS {
			if end, okE := parseAnalyticsTime(r.endedAt); okE && !end.Before(start) {
				durations[durationBucket(end.Sub(start).Minutes())]++
			}
		}
	}
	autonomy, err := s.analyticsAutonomyBuckets(ctx, ids)
	if err != nil {
		return db.SessionShapeResponse{}, err
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
	rows, err := s.duck.QueryContext(ctx, `
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

func (s *Store) GetAnalyticsTools(
	ctx context.Context, f db.AnalyticsFilter,
) (db.ToolsAnalyticsResponse, error) {
	sessions, err := s.analyticsSessions(ctx, f)
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
		return db.ToolsAnalyticsResponse{}, nil
	}
	rows, err := s.duck.QueryContext(ctx, `
		SELECT session_id, category FROM tool_calls`)
	if err != nil {
		return db.ToolsAnalyticsResponse{}, err
	}
	defer rows.Close()
	cats := map[string]int{}
	agents := map[string]map[string]int{}
	trends := map[string]map[string]int{}
	total := 0
	for rows.Next() {
		var sid, cat string
		if err := rows.Scan(&sid, &cat); err != nil {
			return db.ToolsAnalyticsResponse{}, err
		}
		r, ok := meta[sid]
		if !ok {
			continue
		}
		total++
		cats[cat]++
		if agents[r.agent] == nil {
			agents[r.agent] = map[string]int{}
		}
		agents[r.agent][cat]++
		week := bucketAnalyticsDate(analyticsLocalDate(analyticsDateTime(r), f.Timezone), "week")
		if trends[week] == nil {
			trends[week] = map[string]int{}
		}
		trends[week][cat]++
	}
	if err := rows.Err(); err != nil {
		return db.ToolsAnalyticsResponse{}, err
	}
	resp := db.ToolsAnalyticsResponse{TotalCalls: total}
	for cat, count := range cats {
		resp.ByCategory = append(resp.ByCategory, db.ToolCategoryCount{
			Category: cat, Count: count, Pct: round1(float64(count) / float64(max(total, 1)) * 100),
		})
	}
	sort.Slice(resp.ByCategory, func(i, j int) bool {
		if resp.ByCategory[i].Count != resp.ByCategory[j].Count {
			return resp.ByCategory[i].Count > resp.ByCategory[j].Count
		}
		return resp.ByCategory[i].Category < resp.ByCategory[j].Category
	})
	for _, agent := range sortedKeys(agents) {
		breakdown := db.ToolAgentBreakdown{Agent: agent}
		for cat, count := range agents[agent] {
			breakdown.Total += count
			breakdown.Categories = append(breakdown.Categories, db.ToolCategoryCount{Category: cat, Count: count})
		}
		for i := range breakdown.Categories {
			breakdown.Categories[i].Pct = round1(float64(breakdown.Categories[i].Count) / float64(max(breakdown.Total, 1)) * 100)
		}
		resp.ByAgent = append(resp.ByAgent, breakdown)
	}
	for _, date := range sortedKeys(trends) {
		resp.Trend = append(resp.Trend, db.ToolTrendEntry{Date: date, ByCat: trends[date]})
	}
	return resp, nil
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

	sessionMsgs, err := s.velocityMessages(ctx, sessionIDs, analyticsLocation(f.Timezone))
	if err != nil {
		return db.VelocityResponse{}, err
	}
	toolCounts, err := s.velocityToolCounts(ctx, sessionIDs)
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
	rows, err := s.duck.QueryContext(ctx, `
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

func (s *Store) velocityToolCounts(
	ctx context.Context,
	sessionIDs []string,
) (map[string]int, error) {
	out := make(map[string]int, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}
	args, placeholders := stringInArgs(sessionIDs)
	rows, err := s.duck.QueryContext(ctx, `
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
	const maxGapSec = 300.0

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

	where, args := duckBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	durationExpr := "(epoch(s.ended_at) - epoch(s.started_at)) / 60.0"
	durationSelectExpr := "COALESCE(" + durationExpr + ", 0)"
	orderExpr := "s.message_count DESC, s.id ASC"
	switch metric {
	case "duration":
		where += " AND s.started_at IS NOT NULL AND s.ended_at IS NOT NULL AND s.ended_at >= s.started_at"
		orderExpr = durationExpr + " DESC, s.id ASC"
	case "output_tokens":
		where += " AND s.has_total_output_tokens = TRUE"
		orderExpr = "s.total_output_tokens DESC, s.id ASC"
	}
	query := `
		SELECT s.id, s.project, s.first_message, s.message_count,
			s.total_output_tokens, ` + durationSelectExpr + ` AS duration_min,
			s.started_at, s.ended_at, s.termination_status
		FROM sessions s
		WHERE ` + where + `
		ORDER BY ` + orderExpr + `
		LIMIT 10`
	rows, err := s.duck.QueryContext(ctx, query, args...)
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
			&row.OutputTokens, &row.DurationMin, &startedRaw, &endedRaw,
			&row.TerminationStatus,
		); err != nil {
			return db.TopSessionsResponse{}, fmt.Errorf("scanning duckdb analytics top session: %w", err)
		}
		startedAt := formatDBTime(startedRaw)
		endedAt := formatDBTime(endedRaw)
		row.StartedAt = &startedAt
		row.EndedAt = &endedAt
		out.Sessions = append(out.Sessions, row)
	}
	if err := rows.Err(); err != nil {
		return db.TopSessionsResponse{}, fmt.Errorf("iterating duckdb analytics top sessions: %w", err)
	}
	return out, nil
}

func (s *Store) GetAnalyticsSignals(
	ctx context.Context, f db.AnalyticsFilter,
) (db.SignalsAnalyticsResponse, error) {
	sessions, err := s.analyticsSessions(ctx, f)
	if err != nil {
		return db.SignalsAnalyticsResponse{}, err
	}
	rows := make([]db.SignalRow, 0, len(sessions))
	for _, r := range sessions {
		rows = append(rows, db.SignalRow{
			ID: r.id, Agent: r.agent, Project: r.project,
			Date:        analyticsLocalDate(analyticsDateTime(r), f.Timezone),
			HealthScore: r.healthScore, HealthGrade: r.healthGrade,
			Outcome: r.outcome, OutcomeConfidence: r.outcomeConfidence,
			ToolFailureSignalCount: r.toolFailures,
			ToolRetryCount:         r.toolRetries, EditChurnCount: r.editChurn,
			CompactionCount:        r.compactions,
			MidTaskCompactionCount: r.midTaskCompactions,
			ContextPressureMax:     r.contextPressureMax,
		})
	}
	return db.AggregateSignals(rows), nil
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
	rows, err := s.duck.QueryContext(ctx, `
		SELECT m.session_id, m.content, m.timestamp, s.started_at, s.created_at
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE s.deleted_at IS NULL
			AND m.role IN ('user', 'assistant')
			AND m.is_system = FALSE
			AND `+db.SystemPrefixSQL("m.content", "m.role"))
	if err != nil {
		return db.TrendsTermsResponse{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var sessionID string
		var content string
		var msgTS, startedAt, createdAt any
		if err := rows.Scan(&sessionID, &content, &msgTS, &startedAt, &createdAt); err != nil {
			return db.TrendsTermsResponse{}, err
		}
		if !allowedSessions[sessionID] {
			continue
		}
		ts := firstNonEmpty(formatDBTime(msgTS), formatDBTime(startedAt), formatDBTime(createdAt))
		t, ok := parseAnalyticsTime(ts)
		if !ok {
			continue
		}
		local := t.In(analyticsLocation(f.Timezone))
		if !duckAnalyticsTimeMatches(local, f) {
			continue
		}
		date := local.Format("2006-01-02")
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}
		bucket := bucketAnalyticsDate(date, granularity)
		pos, ok := index[bucket]
		if !ok {
			continue
		}
		messageCounts[pos]++
		for i, term := range terms {
			counts[i][pos] += db.CountTrendOccurrences(content, term)
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
}

func (s *Store) loadPricing(ctx context.Context) (map[string]duckRates, error) {
	rows, err := s.duck.QueryContext(ctx, `
		SELECT model_pattern, input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok
		FROM model_pricing`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := duckFallbackPricingMap()
	for rows.Next() {
		var model string
		var rates duckRates
		if err := rows.Scan(&model, &rates.input, &rates.output, &rates.cacheCreation, &rates.cacheRead); err != nil {
			return nil, err
		}
		if strings.HasPrefix(model, "_") {
			continue
		}
		out[model] = rates
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for model, custom := range s.customPricing {
		out[model] = duckRates{
			input: custom.Input, output: custom.Output,
			cacheCreation: custom.CacheCreation, cacheRead: custom.CacheRead,
		}
	}
	return out, nil
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
		}
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
	vals := strings.Split(csv, ",")
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
		args = append(args, strings.TrimSpace(vals[0]))
		return where, args
	}
	ph := make([]string, 0, len(vals))
	for _, v := range vals {
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			continue
		}
		ph = append(ph, "?")
		args = append(args, trimmed)
	}
	if len(ph) == 0 {
		return where, args
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
	where, args = appendDuckUsageCSVFilter(where, args, "s.project", f.Project, true)
	where, args = appendDuckUsageCSVFilter(where, args, "s.machine", f.Machine, true)
	where, args = appendDuckUsageCSVFilter(where, args, "s.project", f.ExcludeProject, false)
	where, args = appendDuckUsageCSVFilter(where, args, "s.agent", f.ExcludeAgent, false)
	if sessionID != "" {
		where += "\n\t\t\tAND s.id = ?"
		args = append(args, sessionID)
	}
	if f.MinUserMessages > 0 {
		where += "\n\t\t\tAND s.user_message_count >= ?"
		args = append(args, f.MinUserMessages)
	}
	if f.ExcludeOneShot {
		where += "\n\t\t\tAND s.user_message_count > 1"
	}
	if f.ExcludeAutomated {
		where += "\n\t\t\tAND COALESCE(s.is_automated, FALSE) = FALSE"
	}
	if f.ActiveSince != "" {
		where += "\n\t\t\tAND COALESCE(s.ended_at, s.started_at, s.created_at) >= CAST(? AS TIMESTAMP)"
		args = append(args, f.ActiveSince)
	}
	return where, args
}

func duckUsageRawSQL(f db.UsageFilter, sessionID string) (string, []any) {
	bounds := duckUsageBoundsForFilter(f)
	messageWhere := `
			m.token_usage != ''
			AND m.model != ''
			AND m.model != '<synthetic>'
			AND s.deleted_at IS NULL`
	var messageArgs []any
	messageWhere, messageArgs = appendDuckUsageSourceFilterClauses(
		messageWhere, messageArgs, "m.model", f)
	messageWhere, messageArgs = appendDuckUsageSessionFilterClauses(
		messageWhere, messageArgs, f, sessionID)
	messageWhere, messageArgs = appendDuckUsageColumnBounds(
		messageWhere, "COALESCE(m.timestamp, s.started_at)", bounds, messageArgs)

	eventWhere := `
			ue.model != ''
			AND s.deleted_at IS NULL`
	var eventArgs []any
	eventWhere, eventArgs = appendDuckUsageSourceFilterClauses(
		eventWhere, eventArgs, "ue.model", f)
	eventWhere, eventArgs = appendDuckUsageSessionFilterClauses(
		eventWhere, eventArgs, f, sessionID)
	eventWhere, eventArgs = appendDuckUsageColumnBounds(
		eventWhere, "COALESCE(ue.occurred_at, s.started_at)", bounds, eventArgs)

	query := fmt.Sprintf(`
		SELECT m.session_id AS session_id, m.ordinal AS message_ordinal,
			'message' AS source, COALESCE(m.timestamp, s.started_at) AS ts,
			m.model AS model, m.token_usage AS token_json,
			m.claude_message_id AS claude_message_id,
			m.claude_request_id AS claude_request_id,
			'' AS usage_dedup_key,
			0 AS input_tokens, 0 AS output_tokens,
			0 AS cache_create, 0 AS cache_read, NULL AS cost_usd,
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
			CASE
				WHEN ue.dedup_key != '' THEN ue.session_id || ':' || ue.source || ':' || ue.dedup_key
				ELSE ue.session_id || ':' || ue.source || ':id:' || CAST(ue.id AS VARCHAR)
			END AS usage_dedup_key,
			ue.input_tokens AS input_tokens, ue.output_tokens AS output_tokens,
			ue.cache_creation_input_tokens AS cache_create,
			ue.cache_read_input_tokens AS cache_read,
			ue.cost_usd AS cost_usd,
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

func duckUsageLocalDateSQL(f db.UsageFilter) (string, any) {
	if f.Timezone != "" {
		return "strftime(timezone(?, timezone('UTC', ts)), '%Y-%m-%d')", f.Timezone
	}
	ref := time.Now().UTC()
	if f.From != "" {
		if t, err := time.Parse(time.RFC3339, f.From+"T12:00:00Z"); err == nil {
			ref = t
		}
	}
	_, offset := ref.In(time.Local).Zone()
	return "strftime(ts + (? * INTERVAL 1 SECOND), '%Y-%m-%d')", offset
}

func duckUsageCTE(f db.UsageFilter, sessionID string) (string, []any) {
	rawSQL, args := duckUsageRawSQL(f, sessionID)
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
			%s
		),
		usage_normalized AS (
			SELECT *,
				CASE
					WHEN source = 'message' THEN COALESCE(TRY_CAST(json_extract_string(token_json, '$.input_tokens') AS BIGINT), 0)
					ELSE input_tokens
				END AS input_tokens_norm,
				CASE
					WHEN source = 'message' THEN COALESCE(TRY_CAST(json_extract_string(token_json, '$.output_tokens') AS BIGINT), 0)
					ELSE output_tokens
				END AS output_tokens_norm,
				CASE
					WHEN source = 'message' THEN COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_creation_input_tokens') AS BIGINT), 0)
					ELSE cache_create
				END AS cache_create_norm,
				CASE
					WHEN source = 'message' THEN COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_read_input_tokens') AS BIGINT), 0)
					ELSE cache_read
				END AS cache_read_norm,
				CASE
					WHEN claude_message_id != '' AND claude_request_id != ''
						THEN 'claude:' || claude_message_id || ':' || claude_request_id
					WHEN usage_dedup_key != ''
						THEN 'usage:' || usage_dedup_key
					ELSE 'row:' || session_id || ':' || source || ':' ||
						COALESCE(CAST(message_ordinal AS VARCHAR), '') || ':' ||
						CAST(ts AS VARCHAR) || ':' || model
				END AS dedup_group,
				%s AS local_date
			FROM usage_raw
		),
		usage_windowed AS (
			SELECT *
			FROM usage_normalized
			WHERE %s
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
		)`, rawSQL, localDateSQL, datePred)
	args = append(args, localDateArg)
	args = append(args, dateArgs...)
	return query, args
}

type duckUsageBucket struct {
	inputTok  int
	outputTok int
	cacheCr   int
	cacheRd   int
	cost      float64
}

type duckUsageAggregateRow struct {
	date            string
	sessionID       string
	project         string
	agent           string
	model           string
	displayName     string
	startedAt       string
	inputTok        int
	outputTok       int
	cacheCr         int
	cacheRd         int
	billableInput   int
	billableOutput  int
	billableCacheCr int
	billableCacheRd int
	explicitCost    float64
}

func duckUsageAggregateCost(
	model string,
	inputTok, outputTok, cacheCr, cacheRd int,
	billableInput, billableOutput, billableCacheCr, billableCacheRd int,
	explicitCost float64,
	pricing map[string]duckRates,
) (float64, float64, bool, bool) {
	if explicitCost == 0 &&
		inputTok == 0 && outputTok == 0 && cacheCr == 0 && cacheRd == 0 {
		return 0, 0, true, false
	}
	rates, priced := pricingpkg.Resolve(pricing, model)
	cost := explicitCost +
		(float64(billableInput)*rates.input+
			float64(billableOutput)*rates.output+
			float64(billableCacheCr)*rates.cacheCreation+
			float64(billableCacheRd)*rates.cacheRead)/1_000_000
	readDelta := float64(cacheRd) * (rates.input - rates.cacheRead) / 1_000_000
	createDelta := float64(cacheCr) * (rates.input - rates.cacheCreation) / 1_000_000
	return cost, readDelta + createDelta, priced || explicitCost != 0, true
}

func (s *Store) dailyUsageAggregateRows(
	ctx context.Context, f db.UsageFilter,
) ([]duckUsageAggregateRow, error) {
	cte, args := duckUsageCTE(f, "")
	query := cte + `
		SELECT local_date, project, agent, model,
			SUM(input_tokens_norm) AS input_tokens,
			SUM(output_tokens_norm) AS output_tokens,
			SUM(cache_create_norm) AS cache_creation_tokens,
			SUM(cache_read_norm) AS cache_read_tokens,
			SUM(CASE WHEN cost_usd IS NULL THEN input_tokens_norm ELSE 0 END) AS billable_input_tokens,
			SUM(CASE WHEN cost_usd IS NULL THEN output_tokens_norm ELSE 0 END) AS billable_output_tokens,
			SUM(CASE WHEN cost_usd IS NULL THEN cache_create_norm ELSE 0 END) AS billable_cache_creation_tokens,
			SUM(CASE WHEN cost_usd IS NULL THEN cache_read_norm ELSE 0 END) AS billable_cache_read_tokens,
			COALESCE(SUM(cost_usd), 0) AS explicit_cost
		FROM usage_localized
		GROUP BY local_date, project, agent, model
		ORDER BY local_date ASC, project ASC, agent ASC, model ASC`
	rows, err := s.duck.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb daily usage aggregates: %w", err)
	}
	defer rows.Close()
	var out []duckUsageAggregateRow
	for rows.Next() {
		var r duckUsageAggregateRow
		if err := rows.Scan(
			&r.date, &r.project, &r.agent, &r.model,
			&r.inputTok, &r.outputTok, &r.cacheCr, &r.cacheRd,
			&r.billableInput, &r.billableOutput,
			&r.billableCacheCr, &r.billableCacheRd,
			&r.explicitCost,
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
	rows, err := s.dailyUsageAggregateRows(ctx, f)
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	type usageAccumKey struct {
		date    string
		project string
		agent   string
		model   string
	}
	accum := map[usageAccumKey]*duckUsageBucket{}
	totalSavings := 0.0
	for _, r := range rows {
		key := usageAccumKey{date: r.date, project: r.project, agent: r.agent, model: r.model}
		b := accum[key]
		if b == nil {
			b = &duckUsageBucket{}
			accum[key] = b
		}
		cost, savings, _, _ := duckUsageAggregateCost(
			r.model,
			r.inputTok, r.outputTok, r.cacheCr, r.cacheRd,
			r.billableInput, r.billableOutput,
			r.billableCacheCr, r.billableCacheRd,
			r.explicitCost,
			pricing,
		)
		totalSavings += savings
		b.inputTok += r.inputTok
		b.outputTok += r.outputTok
		b.cacheCr += r.cacheCr
		b.cacheRd += r.cacheRd
		b.cost += cost
	}

	type dayMaps struct {
		models   map[string]duckUsageBucket
		projects map[string]duckUsageBucket
		agents   map[string]duckUsageBucket
	}
	days := map[string]*dayMaps{}
	for key, b := range accum {
		day := days[key.date]
		if day == nil {
			day = &dayMaps{
				models:   map[string]duckUsageBucket{},
				projects: map[string]duckUsageBucket{},
				agents:   map[string]duckUsageBucket{},
			}
			days[key.date] = day
		}
		addUsageBucket(day.models, key.model, *b)
		if f.Breakdowns {
			addUsageBucket(day.projects, key.project, *b)
			addUsageBucket(day.agents, key.agent, *b)
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
			entry.TotalCost += b.cost
			entry.ModelBreakdowns = append(entry.ModelBreakdowns, db.ModelBreakdown{
				ModelName:           model,
				InputTokens:         b.inputTok,
				OutputTokens:        b.outputTok,
				CacheCreationTokens: b.cacheCr,
				CacheReadTokens:     b.cacheRd,
				Cost:                roundCost(b.cost),
			})
		}
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
		}
		entry.TotalCost = roundCost(entry.TotalCost)
		result.Daily = append(result.Daily, entry)
		result.Totals.InputTokens += entry.InputTokens
		result.Totals.OutputTokens += entry.OutputTokens
		result.Totals.CacheCreationTokens += entry.CacheCreationTokens
		result.Totals.CacheReadTokens += entry.CacheReadTokens
		result.Totals.TotalCost += entry.TotalCost
	}
	result.Totals.CacheSavings = roundCost(totalSavings)
	result.Totals.TotalCost = roundCost(result.Totals.TotalCost)
	if result.Daily == nil {
		result.Daily = []db.DailyUsageEntry{}
	}
	counts, err := s.GetUsageSessionCounts(ctx, f)
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	result.SessionCounts = counts
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
			SUM(CASE WHEN cost_usd IS NULL THEN input_tokens_norm ELSE 0 END) AS billable_input_tokens,
			SUM(CASE WHEN cost_usd IS NULL THEN output_tokens_norm ELSE 0 END) AS billable_output_tokens,
			SUM(CASE WHEN cost_usd IS NULL THEN cache_create_norm ELSE 0 END) AS billable_cache_creation_tokens,
			SUM(CASE WHEN cost_usd IS NULL THEN cache_read_norm ELSE 0 END) AS billable_cache_read_tokens,
			COALESCE(SUM(cost_usd), 0) AS explicit_cost
		FROM usage_localized
		GROUP BY session_id, project, agent, model
		ORDER BY session_id ASC, model ASC`
	rows, err := s.duck.QueryContext(ctx, query, args...)
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
			&r.billableInput, &r.billableOutput,
			&r.billableCacheCr, &r.billableCacheRd,
			&r.explicitCost,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb session usage aggregate: %w", err)
		}
		r.startedAt = formatDBTime(startedAt)
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
	rows, err := s.sessionUsageAggregateRows(ctx, f, "")
	if err != nil {
		return nil, err
	}
	type acc struct {
		row    db.TopSessionEntry
		tokens int
		cost   float64
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
			r.billableInput, r.billableOutput,
			r.billableCacheCr, r.billableCacheRd,
			r.explicitCost,
			pricing,
		)
		a.tokens += r.inputTok + r.outputTok + r.cacheCr + r.cacheRd
		a.cost += cost
	}
	out := make([]db.TopSessionEntry, 0, len(bySession))
	for _, a := range bySession {
		a.row.TotalTokens = a.tokens
		a.row.Cost = roundCost(a.cost)
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

func (s *Store) GetSessionUsage(
	ctx context.Context, sessionID string,
) (*db.SessionUsage, error) {
	sess, err := s.GetSession(ctx, sessionID)
	if err != nil || sess == nil {
		return nil, err
	}
	pricing, err := s.loadPricing(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.sessionUsageAggregateRows(ctx, db.UsageFilter{}, sessionID)
	if err != nil {
		return nil, err
	}
	models := map[string]bool{}
	unpriced := map[string]bool{}
	totalCost := 0.0
	hasRows := false
	for _, r := range rows {
		hasRows = true
		models[r.model] = true
		cost, _, priced, contributes := duckUsageAggregateCost(
			r.model,
			r.inputTok, r.outputTok, r.cacheCr, r.cacheRd,
			r.billableInput, r.billableOutput,
			r.billableCacheCr, r.billableCacheRd,
			r.explicitCost,
			pricing,
		)
		if !contributes {
			continue
		}
		totalCost += cost
		if !priced {
			unpriced[r.model] = true
		}
	}
	out := &db.SessionUsage{
		SessionID: sessionID, Agent: sess.Agent, Project: sess.Project,
		TotalOutputTokens: sess.TotalOutputTokens,
		PeakContextTokens: sess.PeakContextTokens,
		HasTokenData:      hasRows || sess.HasTotalOutputTokens || sess.HasPeakContextTokens,
		Models:            sortedBoolKeys(models),
		UnpricedModels:    sortedBoolKeys(unpriced),
	}
	if len(unpriced) == 0 && hasRows {
		out.HasCost = true
		out.CostUSD = roundCost(totalCost)
	}
	return out, nil
}
