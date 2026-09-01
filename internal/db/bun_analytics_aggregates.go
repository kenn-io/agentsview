package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/uptrace/bun"
)

func (s *BunStore) bunAnalyticsBuilder(
	store bun.IDB, f AnalyticsFilter, dateScope bunAnalyticsDateScope,
) (*bunAnalyticsSQL, BunCTEFragment, error) {
	builder, err := newBunAnalyticsSQL(s.backend.Capabilities().AnalyticsDialect, f)
	if err != nil {
		return nil, BunCTEFragment{}, err
	}
	return builder, builder.filteredSessionsCTE(
		store, s.backend.TimestampOrderExpr, dateScope,
	), nil
}

type bunAnalyticsToolAggregateRow struct {
	SessionID string `bun:"session_id"`
	ToolName  string `bun:"tool_name"`
	Category  string `bun:"category"`
	Agent     string `bun:"agent"`
	Week      string `bun:"week"`
	Count     int    `bun:"count"`
}

func (s *BunStore) getBunAnalyticsToolsAggregate(
	ctx context.Context, store bun.IDB, f AnalyticsFilter,
) (ToolsAnalyticsResponse, error) {
	builder, sessions, err := s.bunAnalyticsBuilder(
		store, f, bunAnalyticsFactDateScope,
	)
	if err != nil {
		return ToolsAnalyticsResponse{}, err
	}
	with := renderBunCTEs(sessions, builder.directToolFactsCTE())
	local := builder.dialect.LocalTimestamp("COALESCE("+
		bunNullableTimestamp("tool.message_timestamp")+", "+
		bunNullableTimestamp("session.started_at")+", session.created_at)", builder.zone)
	week := builder.dialect.Bucket(local, "week")
	args := append([]any{}, with.Args...)
	args = append(args, week.Args...)
	query := `WITH ` + with.SQL + `
SELECT tool.session_id,
	TRIM(COALESCE(tool.tool_name, '')) AS tool_name,
	tool.category,
	session.agent,
	` + week.SQL + ` AS week,
	COUNT(*) AS count
FROM ` + bunAnalyticsToolFactsCTE + ` AS tool
JOIN ` + bunAnalyticsFilteredSessionsCTE + ` AS session
	ON session.id = tool.session_id
GROUP BY tool.session_id, TRIM(COALESCE(tool.tool_name, '')),
	tool.category, session.agent, ` + week.SQL
	args = append(args, week.Args...)

	var rows []bunAnalyticsToolAggregateRow
	if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return ToolsAnalyticsResponse{}, fmt.Errorf(
			"querying Bun analytics tool aggregates: %w", err,
		)
	}
	compact := make([]ToolAnalyticsRow, 0, len(rows))
	for _, row := range rows {
		compact = append(compact, ToolAnalyticsRow{
			SessionID: row.SessionID, ToolName: row.ToolName,
			Category: row.Category, Agent: row.Agent, Date: row.Week, Count: row.Count,
		})
	}
	return BuildToolsAnalytics(compact), nil
}

type bunAnalyticsSkillAggregateRow struct {
	SessionID  string         `bun:"session_id"`
	SkillName  string         `bun:"skill_name"`
	Agent      string         `bun:"agent"`
	Project    string         `bun:"project"`
	Bucket     string         `bun:"bucket"`
	LastUsedAt sql.NullString `bun:"last_used_at"`
	Count      int            `bun:"count"`
}

func (s *BunStore) getBunAnalyticsSkillsAggregate(
	ctx context.Context, store bun.IDB, f AnalyticsFilter, granularity string,
) (SkillsAnalyticsResponse, error) {
	builder, sessions, err := s.bunAnalyticsBuilder(
		store, f, bunAnalyticsFactDateScope,
	)
	if err != nil {
		return SkillsAnalyticsResponse{}, err
	}
	with := renderBunCTEs(sessions, builder.directToolFactsCTE())
	instant := "COALESCE(" + bunNullableTimestamp("tool.message_timestamp") + ", " +
		bunNullableTimestamp("session.started_at") + ", session.created_at)"
	local := builder.dialect.LocalTimestamp(instant, builder.zone)
	bucket := builder.dialect.Bucket(local, skillsTrendGranularity(granularity))
	lastUsed := builder.dialect.LocalTimestamp(instant, "UTC")
	args := append([]any{}, with.Args...)
	args = append(args, bucket.Args...)
	args = append(args, lastUsed.Args...)
	query := `WITH ` + with.SQL + `
SELECT tool.session_id,
	TRIM(tool.skill_name) AS skill_name,
	session.agent,
	session.project,
	` + bucket.SQL + ` AS bucket,
	MAX(` + lastUsed.SQL + `) AS last_used_at,
	COUNT(*) AS count
FROM ` + bunAnalyticsToolFactsCTE + ` AS tool
JOIN ` + bunAnalyticsFilteredSessionsCTE + ` AS session
	ON session.id = tool.session_id
WHERE TRIM(COALESCE(tool.skill_name, '')) != ''
GROUP BY tool.session_id, TRIM(tool.skill_name), session.agent, session.project,
	` + bucket.SQL
	args = append(args, bucket.Args...)

	var rows []bunAnalyticsSkillAggregateRow
	if err := store.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return SkillsAnalyticsResponse{}, fmt.Errorf(
			"querying Bun analytics skill aggregates: %w", err,
		)
	}
	compact := make([]SkillAnalyticsRow, 0, len(rows))
	for _, row := range rows {
		lastUsedAt := row.LastUsedAt.String
		parsed, ok := localTime(lastUsedAt, time.UTC)
		if !ok {
			for _, layout := range []string{
				"2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05",
			} {
				value, parseErr := time.ParseInLocation(layout, lastUsedAt, time.UTC)
				if parseErr == nil {
					parsed, ok = value, true
					break
				}
			}
		}
		if ok {
			lastUsedAt = parsed.UTC().Format(time.RFC3339Nano)
		}
		compact = append(compact, SkillAnalyticsRow{
			SessionID: row.SessionID, SkillName: row.SkillName,
			Agent: row.Agent, Project: row.Project, Date: row.Bucket,
			LastUsedAt: lastUsedAt, Count: row.Count,
		})
	}
	return BuildSkillsAnalytics(compact, f.From, f.To, granularity), nil
}

type bunVelocityAggregateRow struct {
	Kind                  string  `bun:"kind"`
	Label                 string  `bun:"label"`
	Sessions              int     `bun:"sessions"`
	TurnCycleP50          float64 `bun:"turn_cycle_p50"`
	TurnCycleP90          float64 `bun:"turn_cycle_p90"`
	FirstResponseP50      float64 `bun:"first_response_p50"`
	FirstResponseP90      float64 `bun:"first_response_p90"`
	MsgsPerActiveMin      float64 `bun:"msgs_per_active_min"`
	CharsPerActiveMin     float64 `bun:"chars_per_active_min"`
	ToolCallsPerActiveMin float64 `bun:"tool_calls_per_active_min"`
}

const bunAnalyticsVelocityMessagesCTE = "analytics_velocity_messages"

func (s *BunStore) getBunAnalyticsVelocityAggregate(
	ctx context.Context, store bun.IDB, f AnalyticsFilter,
) (VelocityResponse, error) {
	builder, sessions, err := s.bunAnalyticsBuilder(
		store, f, bunAnalyticsSessionDateScope,
	)
	if err != nil {
		return VelocityResponse{}, err
	}
	ctes := []BunCTEFragment{sessions}
	if builder.needsMessageQualification() {
		ctes = append(ctes, builder.scopedMessageCTEs()...)
	}
	velocityMessagesName := bunAnalyticsScopedMessagesCTE
	if len(builder.models) == 0 {
		velocityMessagesName = bunAnalyticsVelocityMessagesCTE
		velocityMessages := BunCTEFragment{
			Name: bunAnalyticsVelocityMessagesCTE,
			Query: BunSQL(`SELECT message.*
		FROM messages AS message
		JOIN ` + bunAnalyticsFilteredSessionsCTE + ` AS session
			ON session.id = message.session_id`),
		}
		if f.HasTimeFilter() {
			velocityMessages.Query = BunSQL(`SELECT message.*
		FROM messages AS message
		JOIN ` + bunAnalyticsQualifiedSessionsCTE + ` AS qualified
			ON qualified.session_id = message.session_id`)
		}
		ctes = append(ctes, velocityMessages)
	}
	with := renderBunCTEs(ctes...)
	gapDuration := builder.dialect.DurationSeconds(
		BunSQL("previous_timestamp"), BunSQL("timestamp"),
	)
	turnDuration := builder.dialect.DurationSeconds(
		BunSQL("previous_timestamp"), BunSQL("timestamp"),
	)
	firstResponseDuration := builder.dialect.DurationSeconds(
		BunSQL("prompt.timestamp"), BunSQL("response.timestamp"),
	)
	toolPredicates := ""
	toolArgs := []any{}
	if len(builder.models) > 0 {
		toolPredicates = " AND tool_message.model IN (?)"
		toolArgs = append(toolArgs, bun.List(builder.models))
		if f.DayOfWeek != nil || f.Hour != nil {
			toolLocal := builder.dialect.LocalTimestamp(
				"tool_message.timestamp", builder.zone,
			)
			for _, predicate := range builder.messageDayHourPredicates(toolLocal) {
				toolPredicates += " AND " + predicate.SQL
				toolArgs = append(toolArgs, predicate.Args...)
			}
		}
	}
	query := bunAnalyticsVelocityAggregateSQL(
		velocityMessagesName, gapDuration.SQL, turnDuration.SQL,
		firstResponseDuration.SQL, toolPredicates,
	)
	args := append([]any{}, with.Args...)
	args = append(args, gapDuration.Args...)
	args = append(args, turnDuration.Args...)
	args = append(args, firstResponseDuration.Args...)
	args = append(args, toolArgs...)
	var rows []bunVelocityAggregateRow
	if err := store.NewRaw("WITH "+with.SQL+",\n"+query, args...).Scan(ctx, &rows); err != nil {
		return VelocityResponse{}, fmt.Errorf(
			"querying Bun analytics velocity aggregates: %w", err,
		)
	}
	return buildBunAnalyticsVelocity(rows), nil
}

func bunAnalyticsVelocityAggregateSQL(
	messageSource, gapDuration, turnDuration, firstResponseDuration,
	toolPredicates string,
) string {
	return `ordered AS (
	SELECT scoped.*,
		LAG(role) OVER (PARTITION BY session_id ORDER BY ordinal) AS previous_role,
		LAG(timestamp) OVER (PARTITION BY session_id ORDER BY ordinal) AS previous_timestamp,
		ROW_NUMBER() OVER (PARTITION BY session_id, role ORDER BY ordinal) AS role_number,
		COUNT(*) OVER (PARTITION BY session_id) AS scoped_count
	FROM ` + messageSource + ` AS scoped
),
message_facts AS (
	SELECT ordered.*, CASE WHEN timestamp IS NOT NULL AND previous_timestamp IS NOT NULL
		THEN ` + gapDuration + ` END AS gap_seconds,
		CASE WHEN timestamp IS NOT NULL AND previous_timestamp IS NOT NULL
			AND previous_role = 'user' AND role = 'assistant'
			THEN ` + turnDuration + ` END AS turn_cycle_seconds
	FROM ordered
),
session_facts AS (
	SELECT session.id AS session_id, session.agent,
		CASE WHEN MAX(scoped_count) <= 15 THEN '1-15'
			WHEN MAX(scoped_count) <= 60 THEN '16-60' ELSE '61+' END AS complexity,
		CAST(COUNT(*) AS BIGINT) AS message_count,
		CAST(SUM(CASE WHEN role = 'assistant' THEN content_length ELSE 0 END)
			AS BIGINT) AS assistant_chars,
		CAST(SUM(CASE WHEN gap_seconds > 0 THEN
			CASE WHEN gap_seconds > 300 THEN 300.0 ELSE gap_seconds END ELSE 0.0 END)
			AS DOUBLE PRECISION) AS active_seconds,
		CAST(1 AS BIGINT) AS sessions
	FROM message_facts
	JOIN ` + bunAnalyticsFilteredSessionsCTE + ` AS session ON session.id = message_facts.session_id
	GROUP BY session.id, session.agent
	HAVING COUNT(*) >= 2
),
tool_counts AS (
	SELECT tool_call.session_id, CAST(COUNT(*) AS BIGINT) AS tool_count
	FROM tool_calls AS tool_call
	JOIN messages AS tool_message
		ON tool_message.session_id = tool_call.session_id
		AND tool_message.ordinal = tool_call.message_ordinal
	JOIN session_facts ON session_facts.session_id = tool_call.session_id
	WHERE 1 = 1` + toolPredicates + `
	GROUP BY tool_call.session_id
),
first_users AS (
	SELECT session_id, MIN(ordinal) AS ordinal
	FROM message_facts WHERE role = 'user' AND timestamp IS NOT NULL GROUP BY session_id
),
first_responses AS (
	SELECT first_users.session_id,
		CASE WHEN response.timestamp IS NULL THEN NULL
			WHEN response.timestamp < prompt.timestamp THEN 0
			ELSE ` + firstResponseDuration + ` END AS first_response_seconds
	FROM first_users
	JOIN message_facts AS prompt ON prompt.session_id = first_users.session_id
		AND prompt.ordinal = first_users.ordinal
	LEFT JOIN message_facts AS response ON response.session_id = first_users.session_id
		AND response.ordinal = (SELECT MIN(candidate.ordinal) FROM message_facts AS candidate
			WHERE candidate.session_id = first_users.session_id
			AND candidate.ordinal > first_users.ordinal
			AND candidate.role = 'assistant' AND candidate.timestamp IS NOT NULL)
),
session_metrics AS (
	SELECT session_facts.*,
		COALESCE(tool_counts.tool_count, 0) AS tool_count,
		first_responses.first_response_seconds
	FROM session_facts
	LEFT JOIN tool_counts ON tool_counts.session_id = session_facts.session_id
	LEFT JOIN first_responses ON first_responses.session_id = session_facts.session_id
),
grouped_sessions AS (
	SELECT session_metrics.*, 'overall' AS kind, '' AS label FROM session_metrics
	UNION ALL
	SELECT session_metrics.*, 'agent', agent FROM session_metrics
	UNION ALL
	SELECT session_metrics.*, 'complexity', complexity FROM session_metrics
),
group_totals AS (
	SELECT kind, label, CAST(COUNT(*) AS BIGINT) AS sessions,
		CAST(SUM(CASE WHEN active_seconds > 0 THEN message_count ELSE 0 END)
			AS BIGINT) AS message_count,
		CAST(SUM(CASE WHEN active_seconds > 0 THEN assistant_chars ELSE 0 END)
			AS BIGINT) AS assistant_chars,
		CAST(SUM(CASE WHEN active_seconds > 0 THEN tool_count ELSE 0 END)
			AS BIGINT) AS tool_count,
		CAST(SUM(active_seconds) AS DOUBLE PRECISION) AS active_seconds
	FROM grouped_sessions
	GROUP BY kind, label
),
turn_samples AS (
	SELECT grouped_sessions.kind, grouped_sessions.label,
		message_facts.turn_cycle_seconds AS sample
	FROM grouped_sessions
	JOIN message_facts
		ON message_facts.session_id = grouped_sessions.session_id
	WHERE message_facts.turn_cycle_seconds > 0
		AND message_facts.turn_cycle_seconds <= 1800
),
ranked_turn_samples AS (
	SELECT kind, label, sample,
		ROW_NUMBER() OVER (PARTITION BY kind, label ORDER BY sample) AS sample_rank,
		COUNT(*) OVER (PARTITION BY kind, label) AS sample_count
	FROM turn_samples
),
turn_percentiles AS (
	SELECT kind, label,
		MAX(CASE WHEN sample_rank =
			((sample_count - sample_count % 2) / 2) + 1
			THEN sample END) AS p50,
		MAX(CASE WHEN sample_rank =
			((sample_count * 9 - (sample_count * 9) % 10) / 10) + 1
			THEN sample END) AS p90
	FROM ranked_turn_samples
	GROUP BY kind, label
),
first_response_samples AS (
	SELECT kind, label, first_response_seconds AS sample
	FROM grouped_sessions
	WHERE first_response_seconds IS NOT NULL
),
ranked_first_response_samples AS (
	SELECT kind, label, sample,
		ROW_NUMBER() OVER (PARTITION BY kind, label ORDER BY sample) AS sample_rank,
		COUNT(*) OVER (PARTITION BY kind, label) AS sample_count
	FROM first_response_samples
),
first_response_percentiles AS (
	SELECT kind, label,
		MAX(CASE WHEN sample_rank =
			((sample_count - sample_count % 2) / 2) + 1
			THEN sample END) AS p50,
		MAX(CASE WHEN sample_rank =
			((sample_count * 9 - (sample_count * 9) % 10) / 10) + 1
			THEN sample END) AS p90
	FROM ranked_first_response_samples
	GROUP BY kind, label
)
SELECT group_totals.kind, group_totals.label, group_totals.sessions,
	COALESCE(ROUND(turn_percentiles.p50 * 10.0) / 10.0, 0.0) AS turn_cycle_p50,
	COALESCE(ROUND(turn_percentiles.p90 * 10.0) / 10.0, 0.0) AS turn_cycle_p90,
	COALESCE(ROUND(first_response_percentiles.p50 * 10.0) / 10.0, 0.0)
		AS first_response_p50,
	COALESCE(ROUND(first_response_percentiles.p90 * 10.0) / 10.0, 0.0)
		AS first_response_p90,
	CASE WHEN group_totals.active_seconds > 0 THEN
		ROUND((group_totals.message_count * 60.0 /
			group_totals.active_seconds) * 10.0) / 10.0
		ELSE 0.0 END AS msgs_per_active_min,
	CASE WHEN group_totals.active_seconds > 0 THEN
		ROUND((group_totals.assistant_chars * 60.0 /
			group_totals.active_seconds) * 10.0) / 10.0
		ELSE 0.0 END AS chars_per_active_min,
	CASE WHEN group_totals.active_seconds > 0 THEN
		ROUND((group_totals.tool_count * 60.0 /
			group_totals.active_seconds) * 10.0) / 10.0
		ELSE 0.0 END AS tool_calls_per_active_min
FROM group_totals
LEFT JOIN turn_percentiles ON turn_percentiles.kind = group_totals.kind
	AND turn_percentiles.label = group_totals.label
LEFT JOIN first_response_percentiles
	ON first_response_percentiles.kind = group_totals.kind
	AND first_response_percentiles.label = group_totals.label
ORDER BY group_totals.kind, group_totals.label`
}

func buildBunAnalyticsVelocity(rows []bunVelocityAggregateRow) VelocityResponse {
	result := VelocityResponse{ByAgent: []VelocityBreakdown{}, ByComplexity: []VelocityBreakdown{}}
	for _, row := range rows {
		overview := VelocityOverview{
			TurnCycleSec: Percentiles{
				P50: row.TurnCycleP50, P90: row.TurnCycleP90,
			},
			FirstResponseSec: Percentiles{
				P50: row.FirstResponseP50, P90: row.FirstResponseP90,
			},
			MsgsPerActiveMin:      row.MsgsPerActiveMin,
			CharsPerActiveMin:     row.CharsPerActiveMin,
			ToolCallsPerActiveMin: row.ToolCallsPerActiveMin,
		}
		switch row.Kind {
		case "overall":
			result.Overall = overview
		case "agent":
			result.ByAgent = append(result.ByAgent, VelocityBreakdown{
				Label: row.Label, Sessions: row.Sessions, Overview: overview,
			})
		case "complexity":
			result.ByComplexity = append(result.ByComplexity, VelocityBreakdown{
				Label: row.Label, Sessions: row.Sessions, Overview: overview,
			})
		}
	}
	return result
}
