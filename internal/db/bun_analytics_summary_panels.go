package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/uptrace/bun"
)

const (
	bunAnalyticsPanelSessionsCTE = "analytics_panel_sessions"
	bunAnalyticsMessageStatsCTE  = "analytics_message_stats"
)

func (s *BunStore) bunAnalyticsPanelCTEs(
	store bun.IDB, f AnalyticsFilter,
) (*bunAnalyticsSQL, []BunCTEFragment, error) {
	builder, err := newBunAnalyticsSQL(
		s.backend.Capabilities().AnalyticsDialect, f,
	)
	if err != nil {
		return nil, nil, err
	}
	ctes := []BunCTEFragment{builder.filteredSessionsCTE(
		store, s.backend.TimestampOrderExpr, bunAnalyticsSessionDateScope,
	)}
	needsQualification := len(builder.models) > 0 || f.HasTimeFilter()
	if needsQualification {
		ctes = append(ctes, builder.scopedMessageCTEs()...)
	}
	if len(builder.models) > 0 {
		ctes = append(ctes, BunCTEFragment{
			Name: bunAnalyticsMessageStatsCTE,
			Query: BunSQL(`SELECT session_id,
			COUNT(*) AS message_count,
			CAST(COALESCE(SUM(CASE WHEN has_output_tokens
				THEN output_tokens ELSE 0 END), 0) AS BIGINT) AS output_tokens,
			MAX(CASE WHEN has_output_tokens THEN 1 ELSE 0 END) AS has_output_tokens
		FROM ` + bunAnalyticsScopedMessagesCTE + `
		GROUP BY session_id`),
		})
	}

	instant := "COALESCE(" + bunNullableTimestamp("session.started_at") +
		", session.created_at)"
	localDate := builder.dialect.Date(
		builder.dialect.LocalTimestamp(instant, builder.zone),
	)
	messageCount := "session.message_count"
	outputTokens := "session.total_output_tokens"
	hasOutputTokens := "CASE WHEN session.has_total_output_tokens THEN 1 ELSE 0 END"
	joins := ""
	if needsQualification {
		joins = `
	JOIN ` + bunAnalyticsQualifiedSessionsCTE + ` AS qualified
		ON qualified.session_id = session.id`
	}
	if len(builder.models) > 0 {
		messageCount = "COALESCE(message_stats.message_count, 0)"
		outputTokens = "COALESCE(message_stats.output_tokens, 0)"
		hasOutputTokens = "COALESCE(message_stats.has_output_tokens, 0)"
		joins += `
	JOIN ` + bunAnalyticsMessageStatsCTE + ` AS message_stats
		ON message_stats.session_id = session.id`
	}
	ctes = append(ctes, BunCTEFragment{
		Name: bunAnalyticsPanelSessionsCTE,
		Query: BunSQL(`SELECT session.id, session.project, session.agent,
		`+messageCount+` AS message_count,
		`+outputTokens+` AS output_tokens,
		`+hasOutputTokens+` AS has_output_tokens,
		`+localDate.SQL+` AS local_date
	FROM `+bunAnalyticsFilteredSessionsCTE+` AS session`+joins,
			localDate.Args...),
	})
	return builder, ctes, nil
}

func bunAnalyticsModelsCTE(builder *bunAnalyticsSQL) BunCTEFragment {
	if len(builder.models) > 0 || builder.filter.HasTimeFilter() {
		return BunCTEFragment{
			Name: "analytics_panel_models",
			Query: BunSQL(`SELECT model
		FROM ` + bunAnalyticsScopedMessagesCTE + `
		WHERE COALESCE(model, '') != ''
		GROUP BY model`),
		}
	}
	return BunCTEFragment{
		Name: "analytics_panel_models",
		Query: BunSQL(`SELECT message.model
		FROM messages AS message
		JOIN ` + bunAnalyticsPanelSessionsCTE + ` AS session
			ON session.id = message.session_id
		WHERE COALESCE(message.model, '') != ''
		GROUP BY message.model`),
	}
}

func (s *BunStore) getBunAnalyticsSummaryAggregateAll(
	ctx context.Context, store bun.IDB, f AnalyticsFilter,
) (AnalyticsSummary, error) {
	builder, ctes, err := s.bunAnalyticsPanelCTEs(store, f)
	if err != nil {
		return AnalyticsSummary{}, err
	}
	ctes = append(ctes, bunAnalyticsModelsCTE(builder))
	with := renderBunCTEs(ctes...)
	rows := newBunAnalyticsSummaryStreamModel()
	query := `WITH ` + with.SQL + `,
ranked AS (
	SELECT message_count,
		ROW_NUMBER() OVER (ORDER BY message_count ASC) AS rn,
		COUNT(*) OVER () AS n
	FROM ` + bunAnalyticsPanelSessionsCTE + `
),
project_totals AS (
	SELECT project, SUM(message_count) AS messages
	FROM ` + bunAnalyticsPanelSessionsCTE + `
	GROUP BY project
),
summary AS (
	SELECT COUNT(*) AS total_sessions,
		CAST(COALESCE(SUM(message_count), 0) AS BIGINT) AS total_messages,
		CAST(COALESCE(SUM(CASE WHEN has_output_tokens != 0
			THEN output_tokens ELSE 0 END), 0) AS BIGINT) AS total_output_tokens,
		CAST(COALESCE(SUM(CASE WHEN has_output_tokens != 0
			THEN 1 ELSE 0 END), 0) AS BIGINT) AS token_reporting_sessions,
		COUNT(DISTINCT project) AS active_projects,
		COUNT(DISTINCT local_date) AS active_days,
		CAST(COALESCE(ROUND(AVG(message_count), 1), 0)
			AS DOUBLE PRECISION) AS avg_messages,
		COALESCE((
			SELECT CAST((SUM(message_count) -
				(SUM(message_count) % COUNT(*))) / COUNT(*) AS BIGINT)
			FROM ranked
			WHERE rn IN (
				CAST(((n + 1) - ((n + 1) % 2)) / 2 AS BIGINT),
				CAST(((n + 2) - ((n + 2) % 2)) / 2 AS BIGINT)
			)
		), 0) AS median_messages,
		COALESCE((
			SELECT message_count FROM ranked
			WHERE rn = CASE
				WHEN CAST((n * 9 - ((n * 9) % 10)) / 10 AS BIGINT) + 1 < n
				THEN CAST((n * 9 - ((n * 9) % 10)) / 10 AS BIGINT) + 1
				ELSE n END
			LIMIT 1
		), 0) AS p90_messages,
		COALESCE((SELECT project FROM project_totals
			ORDER BY messages DESC, project ASC LIMIT 1), '') AS most_active,
		CAST(COALESCE(ROUND((SELECT SUM(messages) FROM (
			SELECT messages FROM project_totals
			ORDER BY messages DESC LIMIT 3
		) AS top_projects) * 1.0 / NULLIF(SUM(message_count), 0), 3), 0)
			AS DOUBLE PRECISION) AS concentration
	FROM ` + bunAnalyticsPanelSessionsCTE + `
),
dimensions AS (
	SELECT 'agent' AS kind, agent AS name, COUNT(*) AS sessions,
		CAST(COALESCE(SUM(message_count), 0) AS BIGINT) AS messages
	FROM ` + bunAnalyticsPanelSessionsCTE + ` GROUP BY agent
	UNION ALL
	SELECT 'model' AS kind, model AS name,
		CAST(0 AS BIGINT) AS sessions, CAST(0 AS BIGINT) AS messages
	FROM analytics_panel_models
)
SELECT summary.total_sessions, summary.total_messages,
	summary.total_output_tokens, summary.token_reporting_sessions,
	summary.active_projects, summary.active_days, summary.avg_messages,
	summary.median_messages, summary.p90_messages, summary.most_active,
	summary.concentration, dimensions.kind, dimensions.name,
	dimensions.sessions AS dimension_sessions,
	dimensions.messages AS dimension_messages
FROM summary LEFT JOIN dimensions ON 1 = 1
ORDER BY dimensions.kind ASC, dimensions.name ASC`
	if err := store.NewRaw(query, with.Args...).Scan(ctx, rows); err != nil {
		return AnalyticsSummary{}, fmt.Errorf("querying Bun analytics summary: %w", err)
	}
	if !rows.seen {
		return AnalyticsSummary{Models: []string{}, Agents: map[string]*AgentSummary{}}, nil
	}
	return rows.summary, nil
}

type bunAnalyticsDailyAggregate struct {
	Date         string `bun:"local_date"`
	Value        int    `bun:"value"`
	HasReporting int    `bun:"has_reporting"`
}

func (s *BunStore) getBunAnalyticsHeatmapAggregate(
	ctx context.Context, store bun.IDB, f AnalyticsFilter, metric string,
) (HeatmapResponse, error) {
	_, ctes, err := s.bunAnalyticsPanelCTEs(store, f)
	if err != nil {
		return HeatmapResponse{}, err
	}
	value := "SUM(message_count)"
	hasReporting := "0"
	switch metric {
	case "sessions":
		value = "COUNT(*)"
	case "output_tokens":
		value = "SUM(CASE WHEN has_output_tokens != 0 THEN output_tokens ELSE 0 END)"
		hasReporting = "MAX(has_output_tokens)"
	default:
		metric = "messages"
	}
	with := renderBunCTEs(ctes...)
	var rows []bunAnalyticsDailyAggregate
	query := `WITH ` + with.SQL + `
SELECT local_date, CAST(COALESCE(` + value + `, 0) AS BIGINT) AS value,
	CAST(COALESCE(` + hasReporting + `, 0) AS BIGINT) AS has_reporting
FROM ` + bunAnalyticsPanelSessionsCTE + `
GROUP BY local_date ORDER BY local_date`
	if err := store.NewRaw(query, with.Args...).Scan(ctx, &rows); err != nil {
		return HeatmapResponse{}, fmt.Errorf("querying Bun analytics heatmap: %w", err)
	}
	counts := make(map[string]int, len(rows))
	anyReporting := false
	entriesFrom := clampFrom(f.From, f.To)
	values := make([]int, 0, len(rows))
	for _, row := range rows {
		counts[row.Date] = row.Value
		anyReporting = anyReporting || row.HasReporting != 0
		if row.Value > 0 && inDateRange(row.Date, entriesFrom, f.To) {
			values = append(values, row.Value)
		}
	}
	sort.Ints(values)
	levels := computeQuartileLevels(values)
	result := HeatmapResponse{Metric: metric, EntriesFrom: entriesFrom, Levels: levels}
	if metric != "output_tokens" || anyReporting {
		result.Entries = buildDateEntries(entriesFrom, f.To, counts, levels)
	}
	return result, nil
}

type bunAnalyticsProjectAggregate struct {
	Name           string         `bun:"project"`
	Sessions       int            `bun:"sessions"`
	Messages       int            `bun:"messages"`
	FirstSession   string         `bun:"first_session"`
	LastSession    string         `bun:"last_session"`
	AvgMessages    float64        `bun:"avg_messages"`
	MedianMessages int            `bun:"median_messages"`
	DailyTrend     float64        `bun:"daily_trend"`
	Agent          sql.NullString `bun:"agent"`
	AgentSessions  int            `bun:"agent_sessions"`
}

func (s *BunStore) getBunAnalyticsProjectsAggregate(
	ctx context.Context, store bun.IDB, f AnalyticsFilter,
) (ProjectsAnalyticsResponse, error) {
	_, ctes, err := s.bunAnalyticsPanelCTEs(store, f)
	if err != nil {
		return ProjectsAnalyticsResponse{}, err
	}
	with := renderBunCTEs(ctes...)
	var rows []bunAnalyticsProjectAggregate
	query := `WITH ` + with.SQL + `,
ranked AS (
	SELECT project, message_count,
		ROW_NUMBER() OVER (PARTITION BY project ORDER BY message_count ASC) AS rn,
		COUNT(*) OVER (PARTITION BY project) AS n
	FROM ` + bunAnalyticsPanelSessionsCTE + `
),
projects AS (
	SELECT panel.project, COUNT(*) AS sessions,
		CAST(COALESCE(SUM(panel.message_count), 0) AS BIGINT) AS messages,
		MIN(panel.local_date) AS first_session,
		MAX(panel.local_date) AS last_session,
		CAST(COALESCE(ROUND(AVG(panel.message_count), 1), 0)
			AS DOUBLE PRECISION) AS avg_messages,
		COALESCE((SELECT CAST((SUM(ranked.message_count) -
			(SUM(ranked.message_count) % COUNT(*))) / COUNT(*) AS BIGINT)
			FROM ranked WHERE ranked.project = panel.project AND ranked.rn IN (
				CAST(((ranked.n + 1) - ((ranked.n + 1) % 2)) / 2 AS BIGINT),
				CAST(((ranked.n + 2) - ((ranked.n + 2) % 2)) / 2 AS BIGINT)
			)), 0) AS median_messages,
		CAST(COALESCE(ROUND(SUM(panel.message_count) * 1.0 /
			NULLIF(COUNT(DISTINCT panel.local_date), 0), 1), 0)
			AS DOUBLE PRECISION) AS daily_trend
	FROM ` + bunAnalyticsPanelSessionsCTE + ` AS panel
	GROUP BY panel.project
),
agents AS (
	SELECT project, agent, COUNT(*) AS sessions
	FROM ` + bunAnalyticsPanelSessionsCTE + `
	GROUP BY project, agent
)
SELECT projects.project, projects.sessions, projects.messages,
	projects.first_session, projects.last_session, projects.avg_messages,
	projects.median_messages, projects.daily_trend,
	agents.agent, COALESCE(agents.sessions, 0) AS agent_sessions
FROM projects LEFT JOIN agents ON agents.project = projects.project
ORDER BY projects.messages DESC, projects.project ASC, agents.agent ASC`
	if err := store.NewRaw(query, with.Args...).Scan(ctx, &rows); err != nil {
		return ProjectsAnalyticsResponse{}, fmt.Errorf("querying Bun analytics projects: %w", err)
	}
	result := ProjectsAnalyticsResponse{Projects: []ProjectAnalytics{}}
	indexes := make(map[string]int)
	for _, row := range rows {
		index, ok := indexes[row.Name]
		if !ok {
			index = len(result.Projects)
			indexes[row.Name] = index
			result.Projects = append(result.Projects, ProjectAnalytics{
				Name: row.Name, Sessions: row.Sessions, Messages: row.Messages,
				FirstSession: row.FirstSession, LastSession: row.LastSession,
				AvgMessages: row.AvgMessages, MedianMessages: row.MedianMessages,
				DailyTrend: row.DailyTrend, Agents: map[string]int{},
			})
		}
		if row.Agent.Valid {
			result.Projects[index].Agents[row.Agent.String] = row.AgentSessions
		}
	}
	return result, nil
}

type bunAnalyticsHourAggregate struct {
	Day      int `bun:"day_of_week"`
	Hour     int `bun:"hour"`
	Messages int `bun:"messages"`
}

func (s *BunStore) getBunAnalyticsHourOfWeekAggregate(
	ctx context.Context, store bun.IDB, f AnalyticsFilter,
) (HourOfWeekResponse, error) {
	f.DayOfWeek = nil
	f.Hour = nil
	builder, ctes, err := s.bunAnalyticsPanelCTEs(store, f)
	if err != nil {
		return HourOfWeekResponse{}, err
	}
	if len(builder.models) == 0 {
		ctes = append(ctes, builder.scopedMessageCTEs()...)
	}
	local := builder.dialect.LocalTimestamp("message.timestamp", builder.zone)
	ctes = append(ctes, BunCTEFragment{
		Name: "analytics_local_messages",
		Query: BunSQL(`SELECT `+local.SQL+` AS local_time
		FROM `+bunAnalyticsScopedMessagesCTE+` AS message
		WHERE message.timestamp IS NOT NULL`, local.Args...),
	})
	day := builder.dialect.ISOWeekday(BunSQL("local_time"))
	hour := builder.dialect.Hour(BunSQL("local_time"))
	with := renderBunCTEs(ctes...)
	var rows []bunAnalyticsHourAggregate
	query := `WITH ` + with.SQL + `
SELECT ` + day.SQL + ` AS day_of_week, ` + hour.SQL + ` AS hour,
	COUNT(*) AS messages
FROM analytics_local_messages
GROUP BY 1, 2 ORDER BY 1, 2`
	args := append(append(append([]any(nil), with.Args...), day.Args...), hour.Args...)
	if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return HourOfWeekResponse{}, fmt.Errorf("querying Bun analytics hour of week: %w", err)
	}
	var grid [7][24]int
	for _, row := range rows {
		if row.Day >= 0 && row.Day < len(grid) && row.Hour >= 0 && row.Hour < len(grid[0]) {
			grid[row.Day][row.Hour] = row.Messages
		}
	}
	return HourOfWeekResponseFromGrid(grid), nil
}
