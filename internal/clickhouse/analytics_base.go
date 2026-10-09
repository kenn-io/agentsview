package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"

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

func (s analyticsSQL) Summary(ctx context.Context, f db.AnalyticsFilter) (db.AnalyticsSummary, bool, error) {
	query, queryArgs := s.SummarySQL(f)
	rows, err := s.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return db.AnalyticsSummary{}, false, fmt.Errorf("querying clickhouse analytics summary: %w", err)
	}
	defer rows.Close()
	resp := db.AnalyticsSummary{Agents: map[string]*db.AgentSummary{}}
	if !rows.Next() {
		return resp, false, rows.Err()
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
		return db.AnalyticsSummary{}, false, fmt.Errorf("scanning clickhouse analytics summary: %w", err)
	}
	if err := rows.Err(); err != nil {
		return db.AnalyticsSummary{}, false, fmt.Errorf("iterating clickhouse analytics summary: %w", err)
	}
	if err := rows.Close(); err != nil {
		return db.AnalyticsSummary{}, false, fmt.Errorf("closing clickhouse analytics summary rows: %w", err)
	}

	return resp, true, nil
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
	if _, modelArgs := chAnalyticsCSVPredicate("m.model", f.Model); len(modelArgs) > 0 {
		queryArgs = append(queryArgs, modelArgs...)
		queryArgs = append(queryArgs, modelArgs...)
	}
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
					AND COALESCE(m.source_subtype, '') != 'tool_result')) AS user_messages,
				toInt64(countIf(m.role = 'assistant')) AS assistant_messages,
				toInt64(countIf(m.has_thinking = true)) AS thinking_messages
			FROM filtered_sessions fs
			JOIN messages m ON m.session_id = fs.id
			` + chAnalyticsMessageFilterClause("m.model", f.Model) + `
			GROUP BY bucket
		),
		tool_rows AS (
			SELECT ` + bucketExpr + ` AS bucket, toInt64(COUNT(*)) AS tool_calls
			FROM filtered_sessions fs
			JOIN tool_calls tc ON tc.session_id = fs.id
			` + chAnalyticsToolMessageJoin("tc", f.Model) + `
			` + chAnalyticsMessageFilterClause("m.model", f.Model) + `
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
		queryArgs
}

func (s analyticsSQL) ActivityAgentsSQL(f db.AnalyticsFilter, granularity string) (string, []any) {
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
	return `
		WITH filtered_sessions AS (
			SELECT s.id, s.agent, ` + localDate + ` AS local_date
			FROM sessions s
			WHERE ` + where + `
		)
		SELECT ` + bucketExpr + ` AS bucket, fs.agent, toInt64(COUNT(*)) AS messages
		FROM filtered_sessions fs
		JOIN messages m ON m.session_id = fs.id
		` + chAnalyticsMessageFilterClause("m.model", f.Model) + `
		GROUP BY bucket, fs.agent
		ORDER BY bucket, fs.agent`,
		queryArgs
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

func (s analyticsSQL) VisitTools(ctx context.Context, f db.AnalyticsFilter, ids []string, emit func(sessionID, category, name, timestamp string, count int)) error {
	ctx, ph, err := analyticsSessionIDsContext(ctx, ids)
	if err != nil {
		return err
	}
	modelPred, modelArgs := chAnalyticsCSVPredicate("m.model", f.Model)
	from, to := readbase.AnalyticsWindowBounds(f)
	windowPred, windowArgs := chAnalyticsMessageWindowPred("m.timestamp", from, to)
	args := slices.Concat(modelArgs, windowArgs)
	query := `SELECT tc.session_id, tc.category,
			trim(COALESCE(tc.tool_name, '')), toInt64(COUNT(*)),
			MAX(m.timestamp)
			FROM tool_calls tc
			LEFT JOIN (` + chAnalyticsToolCallMessagesSQL + ph + `) m
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
	rows, qErr := s.store.queryContext(ctx, query, args...)
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
		emit(sid, cat, toolName, formatDBTime(ts), count)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

func (s analyticsSQL) VisitSkills(ctx context.Context, f db.AnalyticsFilter, ids []string, emit func(sessionID, name, timestamp string, count int)) error {
	ctx, ph, err := analyticsSessionIDsContext(ctx, ids)
	if err != nil {
		return err
	}
	modelPred, modelArgs := chAnalyticsCSVPredicate("m.model", f.Model)
	from, to := readbase.AnalyticsWindowBounds(f)
	windowPred, windowArgs := chAnalyticsMessageWindowPred("m.timestamp", from, to)
	args := slices.Concat(modelArgs, windowArgs)
	rows, qErr := s.store.queryContext(ctx,
		`SELECT tc.session_id, trim(COALESCE(tc.skill_name, '')),
			toInt64(COUNT(*)), MAX(m.timestamp)
			FROM tool_calls tc
			LEFT JOIN (`+chAnalyticsToolCallMessagesSQL+ph+`) m
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
		emit(sid, skill, formatDBTime(msgTS), count)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

func (s analyticsSQL) ToolSessionWindow(f db.AnalyticsFilter) (string, []any) {
	return chAnalyticsToolSessionWindow(f)
}

func (s analyticsSQL) TopSessionsSQL(f db.AnalyticsFilter, metric string, includeTime, unlimited bool) (string, []any) {
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
	// The shared base takes ten rows after filtering the paired sessions.
	limitClause := "\n\t\tLIMIT 10"
	if unlimited {
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

func (s analyticsSQL) TrendsSQL() string {
	return `
		SELECT m.session_id, m.ordinal, m.role, m.is_system,
			COALESCE(m.model, ''), m.content, m.timestamp,
			s.started_at, s.created_at
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE s.deleted_at IS NULL
			AND m.role IN ('user', 'assistant')
			AND m.is_system = false
			AND ` + db.ClickHouseSystemPrefixSQL("m.content", "m.role") + `
		ORDER BY m.session_id, m.ordinal`
}

func (s analyticsSQL) FormatTime(v any) string { return formatDBTime(v) }

func (s analyticsSQL) MessageScope(ctx context.Context, ids []string, f db.AnalyticsFilter, includeContent bool) (db.MessageScope, error) {
	return s.store.resolveAnalyticsMessageScope(ctx, ids, f, includeContent)
}

func (s analyticsSQL) PopulateFrustrationMarkers(ctx context.Context, rows []db.SignalRow, sessions []readbase.AnalyticsSession) error {
	return s.store.chPopulateFrustrationMarkers(ctx, rows, chSessionPushVersions(sessions))
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
