package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
)

type analyticsSQL struct{ store *Store }

func (s *Store) analytics() *readbase.Analytics {
	return readbase.NewAnalytics(analyticsSQL{s}, "clickhouse")
}

func (s analyticsSQL) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.store.queryContext(ctx, query, args...)
}

func (s analyticsSQL) Sessions(ctx context.Context, f db.AnalyticsFilter, includeDate, includeTime bool, extraPred string, extraArgs []any) ([]readbase.AnalyticsSession, error) {
	return s.store.loadAnalyticsSessions(ctx, f, includeDate, includeTime, extraPred, extraArgs)
}

func (s analyticsSQL) SummarySQL(f db.AnalyticsFilter) (string, []any) {
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
			SELECT project, SUM(message_count) AS messages
			FROM filtered
			GROUP BY project
		)
		SELECT
			toInt64(COUNT(*)) AS total_sessions,
			SUM(message_count) AS total_messages,
			sumIf(total_output_tokens, has_total_output_tokens = true) AS total_output_tokens,
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
			ifNotFinite(round((
				SELECT SUM(messages)
				FROM (
					SELECT messages
					FROM project_totals
					ORDER BY messages DESC
					LIMIT 3
				)
			) / NULLIF(SUM(message_count), 0), 3), 0) AS concentration
		FROM filtered`
	return query, queryArgs
}

func (s analyticsSQL) SummaryAgentsSQL(f db.AnalyticsFilter) (string, []any) {
	where, args := chBuildAnalyticsWhere(f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	return readbase.AnalyticsSummaryAgentsSQL(where, args, db.ClickHouseQueryDialect())
}

func (s analyticsSQL) ActivityBucketsSQL(f db.AnalyticsFilter, granularity string) (string, []any) {
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	localDate, localDateArgs := chAnalyticsLocalDateExpr(
		"COALESCE(s.started_at, s.created_at)", f)
	bucketExpr := chAnalyticsBucketExpr("local_date", granularity)
	queryArgs := append([]any{}, localDateArgs...)
	queryArgs = append(queryArgs, args...)
	modelPred, modelArgs := readbase.AnalyticsCSVPredicate("m.model", f.Model, db.ClickHouseQueryDialect())
	if modelPred == "" {
		return chActivityBucketsFromFactsSQL(where, localDate, bucketExpr), queryArgs
	}
	queryArgs = append(queryArgs, modelArgs...)
	queryArgs = append(queryArgs, modelArgs...)
	return `
		WITH filtered_sessions AS (
			SELECT s.id, s.message_count, ` + localDate + ` AS local_date
			FROM sessions s
			WHERE ` + where + `
		),
		session_rows AS (
			SELECT ` + bucketExpr + ` AS bucket,
				toInt64(COUNT(*)) AS sessions
			FROM filtered_sessions
			GROUP BY bucket
		),
		message_rows AS (
			SELECT ` + bucketExpr + ` AS bucket,
				toInt64(COUNT(*)) AS messages,
				toInt64(countIf(m.role = 'user' AND m.is_system = false
					AND m.source_subtype != 'tool_result')) AS user_messages,
				toInt64(countIf(m.role = 'assistant')) AS assistant_messages,
				toInt64(countIf(m.has_thinking = true)) AS thinking_messages
			FROM filtered_sessions fs
			JOIN messages m ON m.session_id = fs.id
			` + readbase.AnalyticsMessageFilterClause("m.model", f.Model, db.ClickHouseQueryDialect()) + `
			GROUP BY bucket
		),
		tool_rows AS (
			SELECT ` + bucketExpr + ` AS bucket, toInt64(COUNT(*)) AS tool_calls
			FROM filtered_sessions fs
			JOIN tool_calls tc ON tc.session_id = fs.id
			` + chAnalyticsToolMessageJoin("tc", f.Model) + `
			` + readbase.AnalyticsMessageFilterClause("m.model", f.Model, db.ClickHouseQueryDialect()) + `
			GROUP BY bucket
		)
		SELECT bucket,
			sum(sessions) AS sessions,
			sum(messages) AS messages,
			sum(user_messages) AS user_messages,
			sum(assistant_messages) AS assistant_messages,
			sum(thinking_messages) AS thinking_messages,
			sum(tool_calls) AS tool_calls
		FROM (
			SELECT bucket, sessions,
				0 AS messages, 0 AS user_messages,
				0 AS assistant_messages, 0 AS thinking_messages,
				0 AS tool_calls
			FROM session_rows
			UNION ALL
			SELECT bucket, 0 AS sessions, messages, user_messages,
				assistant_messages, thinking_messages, 0 AS tool_calls
			FROM message_rows
			UNION ALL
			SELECT bucket, 0 AS sessions, 0 AS messages,
				0 AS user_messages, 0 AS assistant_messages,
				0 AS thinking_messages, tool_calls
			FROM tool_rows
		) combined
		GROUP BY bucket
		ORDER BY bucket`,
		queryArgs
}

// chActivityBucketsFromFactsSQL is ActivityBucketsSQL without a model
// filter. Sessions with current dashboard facts contribute their stored
// counts; only the rest join messages and tool_calls, so a mirror whose
// rows all carry facts reads neither table. Its arguments are the local
// date arguments, then the session filter's.
func chActivityBucketsFromFactsSQL(where, localDate, bucketExpr string) string {
	return `
		WITH filtered_sessions AS (
			SELECT s.id, ` + localDate + ` AS local_date,
				s.` + chFactsKnown + ` AS facts_known,
				ifNull(s.stored_message_count, 0) AS fact_messages,
				s.fact_user_messages, s.fact_assistant_messages,
				s.fact_thinking_messages, s.fact_tool_calls
			FROM sessions s
			WHERE ` + where + `
		),
		session_rows AS (
			SELECT ` + bucketExpr + ` AS bucket,
				toInt64(COUNT(*)) AS sessions,
				sumIf(fact_messages, facts_known) AS messages,
				sumIf(fact_user_messages, facts_known) AS user_messages,
				sumIf(fact_assistant_messages, facts_known) AS assistant_messages,
				sumIf(fact_thinking_messages, facts_known) AS thinking_messages,
				sumIf(fact_tool_calls, facts_known) AS tool_calls
			FROM filtered_sessions
			GROUP BY bucket
		),
		message_rows AS (
			SELECT ` + bucketExpr + ` AS bucket,
				toInt64(COUNT(*)) AS messages,
				toInt64(countIf(m.role = 'user' AND m.is_system = false
					AND m.source_subtype != 'tool_result')) AS user_messages,
				toInt64(countIf(m.role = 'assistant')) AS assistant_messages,
				toInt64(countIf(m.has_thinking = true)) AS thinking_messages
			FROM filtered_sessions fs
			JOIN (
				SELECT session_id, role, is_system, source_subtype, has_thinking
				FROM messages
				WHERE session_id IN (SELECT id FROM filtered_sessions WHERE NOT facts_known)
			) m ON m.session_id = fs.id
			GROUP BY bucket
		),
		tool_rows AS (
			SELECT ` + bucketExpr + ` AS bucket, toInt64(COUNT(*)) AS tool_calls
			FROM filtered_sessions fs
			JOIN (
				SELECT session_id
				FROM tool_calls
				WHERE session_id IN (SELECT id FROM filtered_sessions WHERE NOT facts_known)
			) tc ON tc.session_id = fs.id
			GROUP BY bucket
		)
		SELECT bucket,
			sum(sessions) AS sessions,
			sum(messages) AS messages,
			sum(user_messages) AS user_messages,
			sum(assistant_messages) AS assistant_messages,
			sum(thinking_messages) AS thinking_messages,
			sum(tool_calls) AS tool_calls
		FROM (
			SELECT bucket, sessions, messages, user_messages,
				assistant_messages, thinking_messages, tool_calls
			FROM session_rows
			UNION ALL
			SELECT bucket, 0 AS sessions, messages, user_messages,
				assistant_messages, thinking_messages, 0 AS tool_calls
			FROM message_rows
			UNION ALL
			SELECT bucket, 0 AS sessions, 0 AS messages,
				0 AS user_messages, 0 AS assistant_messages,
				0 AS thinking_messages, tool_calls
			FROM tool_rows
		) combined
		GROUP BY bucket
		ORDER BY bucket`
}

func (s analyticsSQL) ActivityAgentsSQL(f db.AnalyticsFilter, granularity string) (string, []any) {
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	localDate, localDateArgs := chAnalyticsLocalDateExpr(
		"COALESCE(s.started_at, s.created_at)", f)
	bucketExpr := chAnalyticsBucketExpr("local_date", granularity)
	modelPred, modelArgs := readbase.AnalyticsCSVPredicate("m.model", f.Model, db.ClickHouseQueryDialect())
	if modelPred == "" {
		// Sessions with current facts count their stored messages; the
		// rest join messages. HAVING drops the zero rows stored counts
		// add, which the join never produces.
		return `
		WITH filtered_sessions AS (
			SELECT s.id, s.agent, ` + localDate + ` AS local_date,
				s.` + chFactsKnown + ` AS facts_known,
				ifNull(s.stored_message_count, 0) AS fact_messages
			FROM sessions s
			WHERE ` + where + `
		)
		SELECT bucket, agent, sum(messages) AS messages
		FROM (
			SELECT ` + bucketExpr + ` AS bucket, agent, fact_messages AS messages
			FROM filtered_sessions
			WHERE facts_known
			UNION ALL
			SELECT ` + bucketExpr + ` AS bucket, fs.agent AS agent, toInt64(COUNT(*)) AS messages
			FROM filtered_sessions fs
			JOIN (
				SELECT session_id
				FROM messages
				WHERE session_id IN (SELECT id FROM filtered_sessions WHERE NOT facts_known)
			) m ON m.session_id = fs.id
			GROUP BY bucket, agent
		) combined
		GROUP BY bucket, agent
		HAVING messages > 0
		ORDER BY bucket, agent`, slices.Concat(localDateArgs, args)
	}
	return readbase.AnalyticsActivityAgentsSQL(where, args, localDate, localDateArgs, bucketExpr, readbase.AnalyticsMessageFilterClause("m.model", f.Model, db.ClickHouseQueryDialect()), modelArgs, db.ClickHouseQueryDialect())
}

func (s analyticsSQL) HeatmapSQL(f db.AnalyticsFilter, metric string) (string, []any) {
	where, args := chBuildAnalyticsWhere(f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	localDate, localDateArgs := chAnalyticsLocalDateExpr("COALESCE(s.started_at, s.created_at)", f)
	return readbase.AnalyticsHeatmapSQL(where, args, localDate, localDateArgs, metric, db.ClickHouseQueryDialect())
}

func (s analyticsSQL) HourOfWeekSQL(f db.AnalyticsFilter) (string, []any) {
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
	return `
		WITH filtered_sessions AS (
			SELECT s.id
			FROM sessions s
			WHERE ` + where + `
		),
		message_buckets AS (
			SELECT toInt64(` + dowExpr + `) AS day_of_week,
				toInt64(` + hourExpr + `) AS hour
			FROM messages m
			JOIN filtered_sessions fs ON fs.id = m.session_id
			WHERE m.timestamp IS NOT NULL
		)
		SELECT day_of_week, hour, toInt64(COUNT(*))
		FROM message_buckets
		GROUP BY day_of_week, hour
		ORDER BY day_of_week, hour`,
		queryArgs
}

func (s analyticsSQL) VisitTools(ctx context.Context, f db.AnalyticsFilter, ids []string, emit func(readbase.AnalyticsToolVisit)) error {
	ctx, ph, err := analyticsSessionIDsContext(ctx, ids)
	if err != nil {
		return err
	}
	pred, args := chToolVisitPredicates(f, true)
	// Evidence links to a call's message, so it keeps one row per minute.
	group, timestamp, groupArgs := chToolVisitGroup(f, f.ToolMetric == "")
	// An unmatched join reads ordinal 0, so only calls whose message exists supply the ordinal.
	// final=1 makes messages unique on (session_id, ordinal), so LEFT ANY
	// returns the same rows as LEFT ALL without a list of matches per key.
	// Both tables are sorted on session_id and the message ordinal, and
	// partial_merge used 15-21% less CPU and 11% less memory than the
	// default hash join over a year of sessions.
	query := `SELECT tc.session_id, tc.category,
			trim(tc.tool_name), toInt64(COUNT(*)),
			MAX(` + timestamp + `), minOrNullIf(tc.message_ordinal, tc.message_facts OR m.session_id != '')` + db.ToolEffectivenessSQL("tc", "s.quality_signal_version") + `
			FROM tool_calls tc
			JOIN (SELECT id, quality_signal_version, COALESCE(started_at, created_at) AS session_ts
				FROM sessions WHERE id IN ` + ph + `) s ON s.id = tc.session_id
			` + chToolMessageJoin(ph) + `
			WHERE tc.session_id IN ` + ph + readbase.AnalyticsAndClause(pred) + `
			GROUP BY tc.session_id, tc.category,
				trim(tc.tool_name), ` + group + `
			SETTINGS join_algorithm = 'partial_merge'`
	rows, qErr := s.store.queryContext(ctx, query, slices.Concat(args, groupArgs)...)
	if qErr != nil {
		return qErr
	}
	defer rows.Close()
	return readbase.ScanAnalyticsTools(rows, "clickhouse", formatDBTime, emit)
}

// chToolMessageTimestamp and chToolMessageModel read a tool call's message
// fields from the call when push copied them there, else from the joined
// message.
const (
	chToolMessageTimestamp = "if(tc.message_facts, tc.message_timestamp, m.timestamp)"
	chToolMessageModel     = "if(tc.message_facts, tc.message_model, m.model)"
)

// chToolMessageJoin left-joins the messages of tool calls that lack copied
// message fields. It reads only the sessions that still have such calls,
// so it reads no messages once every call carries its fields. Under FINAL
// a call matches at most one message, so ANY keeps one row per key.
func chToolMessageJoin(ids string) string {
	return `LEFT ANY JOIN (` + chAnalyticsToolCallMessagesSQL + `(
				SELECT DISTINCT session_id FROM tool_calls
				WHERE session_id IN ` + ids + ` AND NOT message_facts)) m
				ON m.session_id = tc.session_id
				AND m.ordinal = tc.message_ordinal`
}

// chToolVisitGroup returns the time part of a tool or skill visit's GROUP
// BY, the timestamp each row reports, and the GROUP BY's arguments. Rows
// group by minute when perMinute is false or an hour or weekday filter
// needs the time. Otherwise they group by the local date of the call, or
// of its session when the call has no time, and report the latest of
// those times: the Go side resolves each row to that same date and
// range-filters it, and every total it builds sums over rows, so the
// response matches the per-minute rows while ClickHouse sends far fewer.
func chToolVisitGroup(f db.AnalyticsFilter, byDate bool) (group, timestamp string, args []any) {
	if !byDate || f.HasTimeFilter() {
		return "toStartOfMinute(" + chToolMessageTimestamp + ")", chToolMessageTimestamp, nil
	}
	used := "COALESCE(" + chToolMessageTimestamp + ", s.session_ts)"
	date, dateArgs := chAnalyticsLocalDateExpr(used, f)
	return date, used, dateArgs
}

// chToolVisitPredicates returns the model, tool selection, and window
// predicates of a tool or skill visit read. toolSelection adds the tool
// metric predicates.
func chToolVisitPredicates(f db.AnalyticsFilter, toolSelection bool) (string, []any) {
	pred, args := readbase.AnalyticsCSVPredicate(chToolMessageModel, f.Model, db.ClickHouseQueryDialect())
	args = slices.Clone(args)
	preds := []string{}
	if pred != "" {
		preds = append(preds, pred)
	}
	if toolSelection {
		preds = append(preds, db.ToolSelectionPredicates("tc", "s.quality_signal_version", f, func(value string) string { args = append(args, value); return "?" })...)
	}
	from, to := readbase.AnalyticsWindowBounds(f)
	if windowPred, windowArgs := chAnalyticsMessageWindowPred(chToolMessageTimestamp, from, to); windowPred != "" {
		preds = append(preds, windowPred)
		args = append(args, windowArgs...)
	}
	return strings.Join(preds, " AND "), args
}

func (s analyticsSQL) VisitSkills(ctx context.Context, f db.AnalyticsFilter, ids []string, emit func(sessionID, name, timestamp string, count int)) error {
	ctx, ph, err := analyticsSessionIDsContext(ctx, ids)
	if err != nil {
		return err
	}
	pred, args := chToolVisitPredicates(f, false)
	group, timestamp, groupArgs := chToolVisitGroup(f, true)
	rows, qErr := s.store.queryContext(ctx,
		`SELECT tc.session_id, trim(tc.skill_name),
			toInt64(COUNT(*)), MAX(`+timestamp+`)
			FROM tool_calls tc
			JOIN (SELECT id, COALESCE(started_at, created_at) AS session_ts
				FROM sessions WHERE id IN `+ph+`) s ON s.id = tc.session_id
			`+chToolMessageJoin(ph)+`
			WHERE tc.session_id IN `+ph+`
				AND trim(tc.skill_name) != ''
				`+readbase.AnalyticsAndClause(pred)+`
			GROUP BY tc.session_id, trim(tc.skill_name),
				`+group, slices.Concat(args, groupArgs)...)
	if qErr != nil {
		return qErr
	}
	defer rows.Close()
	return readbase.ScanAnalyticsSkills(rows, "clickhouse", formatDBTime, emit)
}

func (s analyticsSQL) ToolSessionWindow(f db.AnalyticsFilter) (string, []any) {
	return chAnalyticsToolSessionWindow(f)
}

func (s analyticsSQL) TopSessionsSQL(f db.AnalyticsFilter, metric string, includeTime bool) (string, []any) {
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, includeTime)
	durationSelectExpr := `COALESCE((toUnixTimestamp64Micro(s.ended_at) - toUnixTimestamp64Micro(s.started_at)) / 60000000.0, 0)`
	activeDurationSelectExpr := "ad.active_duration_min"
	orderExpr := "s.message_count DESC, s.id ASC"
	switch metric {
	case "duration":
		where += " AND s.started_at IS NOT NULL AND s.ended_at IS NOT NULL AND s.ended_at >= s.started_at"
		orderExpr = activeDurationSelectExpr + " DESC, s.id ASC"
	case "output_tokens":
		where += " AND s.has_total_output_tokens = true"
		orderExpr = "s.total_output_tokens DESC, s.id ASC"
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
				WHERE ` + db.ExcludeClaudeHooksSQL("source_subtype") + `
			)
			GROUP BY session_id
		) ad ON ad.session_id = s.id
		WHERE ` + where + `
		ORDER BY ` + orderExpr
	return query, args
}

func (s analyticsSQL) ScanTopSession(rows *sql.Rows) (db.TopSession, error) {
	var row db.TopSession
	var startedRaw, endedRaw any
	if err := rows.Scan(
		&row.ID, &row.Project, &row.FirstMessage, &row.DisplayName,
		&row.MessageCount,
		&row.OutputTokens, &row.DurationMin, &row.ActiveDurationMin,
		&startedRaw, &endedRaw,
		&row.TerminationStatus,
	); err != nil {
		return db.TopSession{}, fmt.Errorf("scanning clickhouse analytics top session: %w", err)
	}
	startedAt := formatDBTime(startedRaw)
	endedAt := formatDBTime(endedRaw)
	row.StartedAt = &startedAt
	row.EndedAt = &endedAt
	return row, nil
}

func (s analyticsSQL) TrendsSQL(f, sessionFilter db.AnalyticsFilter, ordered bool) (string, []any) {
	sessionPred, args := chAnalyticsSessionSet(sessionFilter).in("m.session_id")
	query, args := readbase.AnalyticsTrendsSQL(
		f, chAnalyticsMessageWindowPred,
		db.ClickHouseSystemPrefixSQL("m.content", "m.role"), db.ClickHouseQueryDialect(),
		sessionPred, args, ordered)
	return query + systemPrefixSettings, args
}

func (s analyticsSQL) FormatTime(v any) string { return formatDBTime(v) }

func (s analyticsSQL) PopulateFrustrationMarkers(ctx context.Context, rows []db.SignalRow, sessions []readbase.AnalyticsSession) error {
	return s.store.chPopulateFrustrationMarkers(ctx, rows, sessions)
}

func (s *Store) GetAnalyticsSummary(
	ctx context.Context, f db.AnalyticsFilter,
) (db.AnalyticsSummary, error) {
	return s.analytics().GetAnalyticsSummary(ctx, f)
}

func (s *Store) GetAnalyticsActivity(
	ctx context.Context, f db.AnalyticsFilter, granularity string,
) (db.ActivityResponse, error) {
	return s.analytics().GetAnalyticsActivity(ctx, f, granularity)
}

func (s *Store) GetAnalyticsHeatmap(
	ctx context.Context, f db.AnalyticsFilter, metric string,
) (db.HeatmapResponse, error) {
	return s.analytics().GetAnalyticsHeatmap(ctx, f, metric)
}

func (s *Store) GetAnalyticsProjects(
	ctx context.Context, f db.AnalyticsFilter,
) (db.ProjectsAnalyticsResponse, error) {
	return s.analytics().GetAnalyticsProjects(ctx, f)
}

func (s *Store) GetAnalyticsHourOfWeek(
	ctx context.Context, f db.AnalyticsFilter,
) (db.HourOfWeekResponse, error) {
	return s.analytics().GetAnalyticsHourOfWeek(ctx, f)
}

func (s *Store) GetAnalyticsSessionShape(
	ctx context.Context, f db.AnalyticsFilter,
) (db.SessionShapeResponse, error) {
	return s.analytics().GetAnalyticsSessionShape(ctx, f)
}

func (s *Store) GetAnalyticsTools(
	ctx context.Context, f db.AnalyticsFilter,
) (db.ToolsAnalyticsResponse, error) {
	return s.analytics().GetAnalyticsTools(ctx, f)
}

func (s *Store) GetAnalyticsSkills(
	ctx context.Context, f db.AnalyticsFilter, granularity string,
) (db.SkillsAnalyticsResponse, error) {
	return s.analytics().GetAnalyticsSkills(ctx, f, granularity)
}

func (s *Store) GetAnalyticsVelocity(
	ctx context.Context, f db.AnalyticsFilter,
) (db.VelocityResponse, error) {
	return s.analytics().GetAnalyticsVelocity(ctx, f)
}

func (s *Store) GetAnalyticsTopSessions(
	ctx context.Context, f db.AnalyticsFilter, metric string,
) (db.TopSessionsResponse, error) {
	return s.analytics().GetAnalyticsTopSessions(ctx, f, metric)
}

func (s *Store) GetAnalyticsSignals(
	ctx context.Context, f db.AnalyticsFilter,
) (db.SignalsAnalyticsResponse, error) {
	return s.analytics().GetAnalyticsSignals(ctx, f)
}

func (s *Store) GetAnalyticsSignalSessions(
	ctx context.Context,
	f db.AnalyticsFilter,
	signal string,
	limit int,
) (db.SignalSessionsResponse, error) {
	return s.analytics().GetAnalyticsSignalSessions(ctx, f, signal, limit)
}

func (s *Store) GetTrendsTerms(
	ctx context.Context, f db.AnalyticsFilter,
	terms []db.TrendTermInput, granularity string,
) (db.TrendsTermsResponse, error) {
	return s.analytics().GetTrendsTerms(ctx, f, terms, granularity)
}
