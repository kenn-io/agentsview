package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
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
	SessionID        string          `bun:"session_id"`
	Kind             string          `bun:"kind"`
	Label            string          `bun:"label"`
	Sessions         int             `bun:"sessions"`
	MessageCount     int             `bun:"message_count"`
	AssistantChars   int             `bun:"assistant_chars"`
	ToolCount        int             `bun:"tool_count"`
	ActiveSeconds    float64         `bun:"active_seconds"`
	TurnCycleSeconds sql.NullFloat64 `bun:"turn_cycle_seconds"`
	FirstResponseSec sql.NullFloat64 `bun:"first_response_seconds"`
}

type bunVelocityAggregate struct {
	accum          velocityAccumulator
	throughputSeen map[string]struct{}
	responseSeen   map[string]struct{}
}

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
	velocityMessages := BunCTEFragment{
		Name: bunAnalyticsScopedMessagesCTE,
		Query: BunSQL(`SELECT message.*
		FROM messages AS message
		JOIN ` + bunAnalyticsFilteredSessionsCTE + ` AS session
			ON session.id = message.session_id`),
	}
	if len(builder.models) > 0 {
		velocityMessages = BunCTEFragment{}
	} else if f.HasTimeFilter() {
		velocityMessages.Query = BunSQL(`SELECT message.*
		FROM messages AS message
		JOIN ` + bunAnalyticsQualifiedSessionsCTE + ` AS qualified
			ON qualified.session_id = message.session_id`)
	}
	ctes = append(ctes, velocityMessages)
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
	}
	if f.DayOfWeek != nil || f.Hour != nil {
		toolLocal := builder.dialect.LocalTimestamp("tool_message.timestamp", builder.zone)
		for _, predicate := range builder.messageDayHourPredicates(toolLocal) {
			toolPredicates += " AND " + predicate.SQL
			toolArgs = append(toolArgs, predicate.Args...)
		}
	}
	query := bunAnalyticsVelocityAggregateSQL(
		gapDuration.SQL, turnDuration.SQL, firstResponseDuration.SQL, toolPredicates,
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
	gapDuration, turnDuration, firstResponseDuration, toolPredicates string,
) string {
	return `ordered AS (
	SELECT scoped.*,
		LAG(role) OVER (PARTITION BY session_id ORDER BY ordinal) AS previous_role,
		LAG(timestamp) OVER (PARTITION BY session_id ORDER BY ordinal) AS previous_timestamp,
		ROW_NUMBER() OVER (PARTITION BY session_id, role ORDER BY ordinal) AS role_number,
		COUNT(*) OVER (PARTITION BY session_id) AS scoped_count
	FROM ` + bunAnalyticsScopedMessagesCTE + ` AS scoped
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
facts AS (
	SELECT session_facts.*,
		COALESCE(tool_counts.tool_count, 0) AS tool_count,
		CASE WHEN message_facts.turn_cycle_seconds > 0
			AND message_facts.turn_cycle_seconds <= 1800
			THEN message_facts.turn_cycle_seconds END AS turn_cycle_seconds,
		first_responses.first_response_seconds
	FROM session_facts
	JOIN message_facts ON message_facts.session_id = session_facts.session_id
	LEFT JOIN tool_counts ON tool_counts.session_id = session_facts.session_id
	LEFT JOIN first_responses ON first_responses.session_id = session_facts.session_id
)
SELECT session_id, 'overall' AS kind, '' AS label,
	CAST(sessions AS BIGINT) AS sessions,
	CAST(message_count AS BIGINT) AS message_count,
	CAST(assistant_chars AS BIGINT) AS assistant_chars,
	CAST(tool_count AS BIGINT) AS tool_count,
	active_seconds, turn_cycle_seconds, first_response_seconds FROM facts
UNION ALL
SELECT session_id, 'agent', agent,
	CAST(sessions AS BIGINT) AS sessions,
	CAST(message_count AS BIGINT) AS message_count,
	CAST(assistant_chars AS BIGINT) AS assistant_chars,
	CAST(tool_count AS BIGINT) AS tool_count,
	active_seconds, turn_cycle_seconds, first_response_seconds FROM facts
UNION ALL
SELECT session_id, 'complexity', complexity,
	CAST(sessions AS BIGINT) AS sessions,
	CAST(message_count AS BIGINT) AS message_count,
	CAST(assistant_chars AS BIGINT) AS assistant_chars,
	CAST(tool_count AS BIGINT) AS tool_count,
	active_seconds, turn_cycle_seconds, first_response_seconds FROM facts
ORDER BY kind, label`
}

func buildBunAnalyticsVelocity(rows []bunVelocityAggregateRow) VelocityResponse {
	groups := map[string]*bunVelocityAggregate{}
	for _, row := range rows {
		key := row.Kind + "\x00" + row.Label
		group := groups[key]
		if group == nil {
			group = &bunVelocityAggregate{
				throughputSeen: map[string]struct{}{}, responseSeen: map[string]struct{}{},
			}
			groups[key] = group
		}
		// Every session emits one row per scoped message. Count its session and
		// throughput totals only once; timing samples remain one row per event.
		sessionKey := row.SessionID
		if _, ok := group.throughputSeen[sessionKey]; !ok {
			group.throughputSeen[sessionKey] = struct{}{}
			group.accum.sessions += row.Sessions
			if row.ActiveSeconds > 0 {
				group.accum.totalMsgs += row.MessageCount
				group.accum.totalChars += row.AssistantChars
				group.accum.totalToolCalls += row.ToolCount
				group.accum.activeMinutes += row.ActiveSeconds / 60
			}
		}
		if row.TurnCycleSeconds.Valid {
			group.accum.turnCycles = append(group.accum.turnCycles, row.TurnCycleSeconds.Float64)
		}
		if row.FirstResponseSec.Valid {
			if _, ok := group.responseSeen[row.SessionID]; ok {
				continue
			}
			group.responseSeen[row.SessionID] = struct{}{}
			group.accum.firstResponses = append(group.accum.firstResponses, row.FirstResponseSec.Float64)
		}
	}
	result := VelocityResponse{ByAgent: []VelocityBreakdown{}, ByComplexity: []VelocityBreakdown{}}
	if group := groups["overall\x00"]; group != nil {
		result.Overall = group.accum.computeOverview()
	}
	for key, group := range groups {
		parts := strings.SplitN(key, "\x00", 2)
		switch parts[0] {
		case "agent":
			result.ByAgent = append(result.ByAgent, VelocityBreakdown{
				Label: parts[1], Sessions: group.accum.sessions,
				Overview: group.accum.computeOverview(),
			})
		case "complexity":
			result.ByComplexity = append(result.ByComplexity, VelocityBreakdown{
				Label: parts[1], Sessions: group.accum.sessions,
				Overview: group.accum.computeOverview(),
			})
		}
	}
	sort.Slice(result.ByAgent, func(i, j int) bool {
		return result.ByAgent[i].Label < result.ByAgent[j].Label
	})
	order := map[string]int{"1-15": 0, "16-60": 1, "61+": 2}
	sort.Slice(result.ByComplexity, func(i, j int) bool {
		return order[result.ByComplexity[i].Label] < order[result.ByComplexity[j].Label]
	})
	return result
}
