package duckdb

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
)

type analyticsSQL struct{ store *Store }

func (s *Store) analytics() *readbase.Analytics {
	return readbase.NewAnalytics(analyticsSQL{s}, "duckdb")
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
		return db.AnalyticsSummary{}, false, fmt.Errorf("querying duckdb analytics summary: %w", err)
	}
	defer rows.Close()
	resp := db.AnalyticsSummary{Agents: map[string]*db.AgentSummary{}}
	if !rows.Next() {
		return resp, false, nil
	}
	resp, err = readbase.ScanAnalyticsSummary(rows, resp, "duckdb")
	return resp, true, err
}

func (s analyticsSQL) SummarySQL(f db.AnalyticsFilter) (string, []any) {
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
	return query, queryArgs
}

func (s analyticsSQL) SummaryAgentsSQL(f db.AnalyticsFilter) (string, []any) {
	where, args := duckBuildAnalyticsWhere(f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	return readbase.AnalyticsSummaryAgentsSQL(where, args, db.DuckDBQueryDialect())
}

func (s analyticsSQL) ActivityBucketsSQL(f db.AnalyticsFilter, granularity string) (string, []any) {
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
	return `
		WITH filtered_sessions AS (
			SELECT s.id, s.message_count, ` + localDate + ` AS local_date
			FROM sessions s
			WHERE ` + where + `
		),
		session_rows AS (
			SELECT ` + bucketExpr + ` AS bucket,
				COUNT(*) AS sessions
			FROM filtered_sessions
			GROUP BY bucket
		),
		message_rows AS (
			SELECT ` + bucketExpr + ` AS bucket,
				COUNT(*) AS messages,
				COUNT(*) FILTER (WHERE m.role = 'user' AND m.is_system = FALSE
					AND COALESCE(m.source_subtype, '') != 'tool_result') AS user_messages,
				COUNT(*) FILTER (WHERE m.role = 'assistant') AS assistant_messages,
				COUNT(*) FILTER (WHERE m.has_thinking = TRUE) AS thinking_messages
			FROM filtered_sessions fs
			JOIN messages m ON m.session_id = fs.id
			` + duckAnalyticsMessageFilterClause("m.model", f.Model) + `
			GROUP BY bucket
		),
		tool_rows AS (
			SELECT ` + bucketExpr + ` AS bucket, COUNT(*) AS tool_calls
			FROM filtered_sessions fs
			JOIN tool_calls tc ON tc.session_id = fs.id
			` + duckAnalyticsToolMessageJoin("tc", f.Model) + `
			` + duckAnalyticsMessageFilterClause("m.model", f.Model) + `
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
		queryArgs
}

func (s analyticsSQL) ActivityAgentsSQL(f db.AnalyticsFilter, granularity string) (string, []any) {
	where, args := duckBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	localDate, localDateArgs := duckAnalyticsLocalDateExpr(
		"COALESCE(s.started_at, s.created_at)", f)
	bucketExpr := duckAnalyticsBucketExpr("local_date", granularity)
	_, modelArgs := duckAnalyticsCSVPredicate("m.model", f.Model)
	return readbase.AnalyticsActivityAgentsSQL(where, args, localDate, localDateArgs, bucketExpr, duckAnalyticsMessageFilterClause("m.model", f.Model), modelArgs, db.DuckDBQueryDialect())
}

func (s analyticsSQL) HeatmapSQL(f db.AnalyticsFilter, metric string) (string, []any) {
	where, args := duckBuildAnalyticsWhere(f, "COALESCE(s.started_at, s.created_at)", "s.", true, true)
	localDate, localDateArgs := duckAnalyticsLocalDateExpr("COALESCE(s.started_at, s.created_at)", f)
	return readbase.AnalyticsHeatmapSQL(where, args, localDate, localDateArgs, metric, db.DuckDBQueryDialect())
}

func (s analyticsSQL) HourOfWeekSQL(f db.AnalyticsFilter) (string, []any) {
	sessionFilter := f
	sessionFilter.DayOfWeek = nil
	sessionFilter.Hour = nil
	where, args := duckBuildAnalyticsWhere(
		sessionFilter, "COALESCE(s.started_at, s.created_at)", "s.", true, false)
	localTime, localTimeArgs := duckAnalyticsLocalTimeExpr("m.timestamp", f)
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, localTimeArgs...)
	return `
		WITH filtered_sessions AS (
			SELECT s.id
			FROM sessions s
			WHERE ` + where + `
		),
		message_times AS (
			SELECT ` + localTime + ` AS local_ts
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
		queryArgs
}

func (s analyticsSQL) VisitTools(ctx context.Context, f db.AnalyticsFilter, ids []string, emit func(sessionID, category, name, timestamp string, count int)) error {
	err := duckQueryChunked(ids, func(chunk []string) error {
		ph, args := db.InPlaceholders(chunk)
		modelPred, modelArgs := duckAnalyticsCSVPredicate("m.model", f.Model)
		args = append(args, modelArgs...)
		from, to := readbase.AnalyticsWindowBounds(f)
		windowPred, windowArgs := duckAnalyticsMessageWindowPred("m.timestamp", from, to)
		args = append(args, windowArgs...)
		query := `SELECT tc.session_id, tc.category,
				TRIM(COALESCE(tc.tool_name, '')), COUNT(*),
				MAX(m.timestamp)
				FROM tool_calls tc
				LEFT JOIN messages m
					ON m.session_id = tc.session_id
					AND m.id = tc.message_id
				WHERE tc.session_id IN ` + ph
		if modelPred != "" {
			query += `
				AND ` + modelPred
		}
		query += duckAnalyticsAndClause(windowPred)
		query += `
				GROUP BY tc.session_id, tc.category,
					TRIM(COALESCE(tc.tool_name, '')), date_trunc('minute', m.timestamp)`
		observeAnalyticsQuery(query)
		rows, qErr := s.store.queryContext(ctx, query, args...)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		return readbase.ScanAnalyticsTools(rows, formatDBTime, emit)
	})
	if err != nil {
		return err
	}
	return nil
}

func (s analyticsSQL) VisitSkills(ctx context.Context, f db.AnalyticsFilter, ids []string, emit func(sessionID, name, timestamp string, count int)) error {
	err := duckQueryChunked(ids, func(chunk []string) error {
		ph, args := db.InPlaceholders(chunk)
		modelPred, modelArgs := duckAnalyticsCSVPredicate("m.model", f.Model)
		args = append(args, modelArgs...)
		from, to := readbase.AnalyticsWindowBounds(f)
		windowPred, windowArgs := duckAnalyticsMessageWindowPred("m.timestamp", from, to)
		args = append(args, windowArgs...)
		rows, qErr := s.store.queryContext(ctx,
			`SELECT tc.session_id, TRIM(COALESCE(tc.skill_name, '')),
				COUNT(*), MAX(m.timestamp)
				FROM tool_calls tc
				LEFT JOIN messages m
					ON m.session_id = tc.session_id
					AND m.id = tc.message_id
				WHERE tc.session_id IN `+ph+`
					AND TRIM(COALESCE(tc.skill_name, '')) != ''
					`+duckAnalyticsAndClause(modelPred)+duckAnalyticsAndClause(windowPred)+`
				GROUP BY tc.session_id, TRIM(COALESCE(tc.skill_name, '')),
					date_trunc('minute', m.timestamp)`, args...)
		if qErr != nil {
			return qErr
		}
		defer rows.Close()
		return readbase.ScanAnalyticsSkills(rows, formatDBTime, emit)
	})
	if err != nil {
		return err
	}
	return nil
}

func (s analyticsSQL) ToolSessionWindow(f db.AnalyticsFilter) (string, []any) {
	return duckAnalyticsToolSessionWindow(f)
}

func (s analyticsSQL) TopSessionsSQL(f db.AnalyticsFilter, metric string, includeTime bool) (string, []any) {
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
	// SQL-ranked responses omit display names for compatibility; model-scoped rows include them.
	query := `
		SELECT s.id, s.project, s.first_message, s.message_count,
			s.total_output_tokens, ` + durationSelectExpr + ` AS duration_min,
			` + activeDurationSelectExpr + ` AS active_duration_min,
			s.started_at, s.ended_at, s.termination_status
		FROM sessions s
		WHERE ` + where + `
		ORDER BY ` + orderExpr
	return query, args
}

func (s analyticsSQL) ScanTopSession(rows *sql.Rows) (db.TopSession, error) {
	var row db.TopSession
	var startedRaw, endedRaw any
	if err := rows.Scan(
		&row.ID, &row.Project, &row.FirstMessage, &row.MessageCount,
		&row.OutputTokens, &row.DurationMin, &row.ActiveDurationMin,
		&startedRaw, &endedRaw,
		&row.TerminationStatus,
	); err != nil {
		return db.TopSession{}, fmt.Errorf("scanning duckdb analytics top session: %w", err)
	}
	startedAt := formatDBTime(startedRaw)
	endedAt := formatDBTime(endedRaw)
	row.StartedAt = &startedAt
	row.EndedAt = &endedAt
	return row, nil
}

func (s analyticsSQL) TrendsSQL() string {
	return readbase.AnalyticsTrendsSQL(db.DuckDBSystemPrefixSQL("m.content", "m.role"), db.DuckDBQueryDialect())
}

func (s analyticsSQL) FormatTime(v any) string { return formatDBTime(v) }

func (s analyticsSQL) MessageScope(ctx context.Context, ids []string, f db.AnalyticsFilter, includeContent bool) (db.MessageScope, error) {
	return s.store.resolveAnalyticsMessageScope(ctx, ids, f, includeContent)
}

func (s analyticsSQL) PopulateFrustrationMarkers(ctx context.Context, rows []db.SignalRow, sessions []readbase.AnalyticsSession) error {
	return s.store.duckPopulateFrustrationMarkers(ctx, rows)
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
