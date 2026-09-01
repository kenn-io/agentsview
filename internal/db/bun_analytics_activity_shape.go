package db

import (
	"context"
	"fmt"
	"math"

	"github.com/uptrace/bun"
)

const (
	bunAnalyticsActivityMessageStatsCTE = "analytics_activity_message_stats"
	bunAnalyticsActivityToolStatsCTE    = "analytics_activity_tool_stats"
	bunAnalyticsShapeMessageStatsCTE    = "analytics_shape_message_stats"
)

type bunAnalyticsActivityRow struct {
	Date              string `bun:"date"`
	Agent             string `bun:"agent"`
	Sessions          int    `bun:"sessions"`
	Messages          int    `bun:"messages"`
	UserMessages      int    `bun:"user_messages"`
	AssistantMessages int    `bun:"assistant_messages"`
	ToolCalls         int    `bun:"tool_calls"`
	ThinkingMessages  int    `bun:"thinking_messages"`
}

func (s *BunStore) bunAnalyticsActivityFrom(
	ctx context.Context, store bun.IDB, f AnalyticsFilter, granularity string,
) (ActivityResponse, error) {
	builder, err := newBunAnalyticsSQL(s.backend.Capabilities().AnalyticsDialect, f)
	if err != nil {
		return ActivityResponse{}, err
	}
	filtered := builder.filteredSessionsCTE(
		store, s.backend.TimestampOrderExpr, bunAnalyticsSessionDateScope,
	)
	ctes := []BunCTEFragment{filtered}
	if builder.needsMessageQualification() {
		ctes = append(ctes, builder.scopedMessageCTEs()...)
	}
	ctes = append(ctes,
		builder.activityMessageStatsCTE(), builder.activityToolStatsCTE(),
	)
	with := renderBunCTEs(ctes...)

	instant := "COALESCE(" + bunNullableTimestamp("session.started_at") +
		", session.created_at)"
	local := builder.dialect.LocalTimestamp(instant, builder.zone)
	bucket := builder.dialect.Bucket(local, granularity)
	qualifiedJoin := builder.qualifiedSessionJoin("session")
	query := BunSQL(`WITH `+with.SQL+`
SELECT `+bucket.SQL+` AS date,
	session.agent AS agent,
	COUNT(*) AS sessions,
	CAST(COALESCE(SUM(message_stats.messages), 0) AS BIGINT) AS messages,
	CAST(COALESCE(SUM(message_stats.user_messages), 0) AS BIGINT) AS user_messages,
	CAST(COALESCE(SUM(message_stats.assistant_messages), 0) AS BIGINT) AS assistant_messages,
	CAST(COALESCE(SUM(tool_stats.tool_calls), 0) AS BIGINT) AS tool_calls,
	CAST(COALESCE(SUM(message_stats.thinking_messages), 0) AS BIGINT) AS thinking_messages
FROM `+bunAnalyticsFilteredSessionsCTE+` AS session
`+qualifiedJoin+`
LEFT JOIN `+bunAnalyticsActivityMessageStatsCTE+` AS message_stats
	ON message_stats.session_id = session.id
LEFT JOIN `+bunAnalyticsActivityToolStatsCTE+` AS tool_stats
	ON tool_stats.session_id = session.id
GROUP BY `+bucket.SQL+`, session.agent
ORDER BY date ASC, session.agent ASC`, append(
		append(append([]any(nil), with.Args...), bucket.Args...), bucket.Args...,
	)...)

	var rows []bunAnalyticsActivityRow
	if err := store.NewRaw(query.SQL, query.Args...).Scan(ctx, &rows); err != nil {
		return ActivityResponse{}, fmt.Errorf("querying Bun analytics activity: %w", err)
	}
	return shapeBunAnalyticsActivity(rows, granularity), nil
}

func (b *bunAnalyticsSQL) needsMessageQualification() bool {
	return len(b.models) > 0 || b.filter.HasTimeFilter()
}

func (b *bunAnalyticsSQL) qualifiedSessionJoin(alias string) string {
	if !b.needsMessageQualification() {
		return ""
	}
	return "JOIN " + bunAnalyticsQualifiedSessionsCTE +
		" AS qualified ON qualified.session_id = " + alias + ".id"
}

func (b *bunAnalyticsSQL) activityMessageStatsCTE() BunCTEFragment {
	from := "messages AS message\nJOIN " + bunAnalyticsFilteredSessionsCTE +
		" AS session ON session.id = message.session_id"
	if len(b.models) > 0 {
		from = bunAnalyticsScopedMessagesCTE + " AS message"
	}
	return BunCTEFragment{
		Name: bunAnalyticsActivityMessageStatsCTE,
		Query: BunSQL(`SELECT message.session_id,
	CAST(COUNT(*) AS BIGINT) AS messages,
	CAST(SUM(CASE WHEN message.role = 'user' AND NOT message.is_system
		THEN 1 ELSE 0 END) AS BIGINT) AS user_messages,
	CAST(SUM(CASE WHEN message.role = 'assistant' THEN 1 ELSE 0 END)
		AS BIGINT) AS assistant_messages,
	CAST(SUM(CASE WHEN message.has_thinking THEN 1 ELSE 0 END)
		AS BIGINT) AS thinking_messages
FROM ` + from + `
GROUP BY message.session_id`),
	}
}

func (b *bunAnalyticsSQL) activityToolStatsCTE() BunCTEFragment {
	where := BunSQL("1 = 1")
	if len(b.models) > 0 {
		predicates := []BunSQLFragment{BunSQL("message.model IN (?)", bun.List(b.models))}
		local := b.dialect.LocalTimestamp("message.timestamp", b.zone)
		predicates = append(predicates, b.messageDayHourPredicates(local)...)
		where = JoinBunSQLFragments(" AND ", predicates...)
	}
	return BunCTEFragment{
		Name: bunAnalyticsActivityToolStatsCTE,
		Query: BunSQL(`SELECT tool_call.session_id,
	CAST(COUNT(*) AS BIGINT) AS tool_calls
FROM tool_calls AS tool_call
JOIN messages AS message
	ON message.session_id = tool_call.session_id
	AND message.ordinal = tool_call.message_ordinal
JOIN `+bunAnalyticsFilteredSessionsCTE+` AS session
	ON session.id = tool_call.session_id
WHERE `+where.SQL+`
GROUP BY tool_call.session_id`, where.Args...),
	}
}

func shapeBunAnalyticsActivity(
	rows []bunAnalyticsActivityRow, granularity string,
) ActivityResponse {
	result := ActivityResponse{Granularity: granularity}
	for _, row := range rows {
		if len(result.Series) == 0 || result.Series[len(result.Series)-1].Date != row.Date {
			result.Series = append(result.Series, ActivityEntry{
				Date: row.Date, ByAgent: map[string]int{},
			})
		}
		entry := &result.Series[len(result.Series)-1]
		entry.Sessions += row.Sessions
		entry.Messages += row.Messages
		entry.UserMessages += row.UserMessages
		entry.AssistantMessages += row.AssistantMessages
		entry.ToolCalls += row.ToolCalls
		entry.ThinkingMessages += row.ThinkingMessages
		entry.ByAgent[row.Agent] += row.Messages
	}
	return result
}

type bunAnalyticsSessionShapeRow struct {
	MessageCount    int      `bun:"message_count"`
	UserMessages    int      `bun:"user_messages"`
	ToolUseMessages int      `bun:"tool_use_messages"`
	DurationSeconds *float64 `bun:"duration_seconds"`
}

func (s *BunStore) bunAnalyticsSessionShapeFrom(
	ctx context.Context, store bun.IDB, f AnalyticsFilter,
) (SessionShapeResponse, error) {
	builder, err := newBunAnalyticsSQL(s.backend.Capabilities().AnalyticsDialect, f)
	if err != nil {
		return SessionShapeResponse{}, err
	}
	ctes := []BunCTEFragment{builder.filteredSessionsCTE(
		store, s.backend.TimestampOrderExpr, bunAnalyticsSessionDateScope,
	)}
	if builder.needsMessageQualification() {
		ctes = append(ctes, builder.scopedMessageCTEs()...)
	}
	ctes = append(ctes, builder.sessionShapeMessageStatsCTE())
	with := renderBunCTEs(ctes...)

	start := BunSQL(bunNullableTimestamp("session.started_at"))
	end := BunSQL(bunNullableTimestamp("session.ended_at"))
	duration := builder.dialect.DurationSeconds(start, end)
	messageCount := "session.message_count"
	if len(builder.models) > 0 {
		messageCount = "COALESCE(message_stats.messages, 0)"
	}
	qualifiedJoin := builder.qualifiedSessionJoin("session")
	query := BunSQL(`WITH `+with.SQL+`
SELECT `+messageCount+` AS message_count,
	CAST(COALESCE(message_stats.user_messages, 0) AS BIGINT) AS user_messages,
	CAST(COALESCE(message_stats.tool_use_messages, 0) AS BIGINT) AS tool_use_messages,
	CASE
		WHEN `+start.SQL+` IS NOT NULL
			AND `+end.SQL+` IS NOT NULL
			AND `+s.backend.TimestampOrderExpr(end.SQL)+` >= `+
		s.backend.TimestampOrderExpr(start.SQL)+`
		THEN `+duration.SQL+`
		ELSE NULL
	END AS duration_seconds
FROM `+bunAnalyticsFilteredSessionsCTE+` AS session
`+qualifiedJoin+`
LEFT JOIN `+bunAnalyticsShapeMessageStatsCTE+` AS message_stats
	ON message_stats.session_id = session.id
ORDER BY session.id ASC`, append(append([]any(nil), with.Args...), duration.Args...)...)

	var rows []bunAnalyticsSessionShapeRow
	if err := store.NewRaw(query.SQL, query.Args...).Scan(ctx, &rows); err != nil {
		return SessionShapeResponse{}, fmt.Errorf("querying Bun analytics session shape: %w", err)
	}
	return shapeBunAnalyticsSessionShape(rows), nil
}

func (b *bunAnalyticsSQL) sessionShapeMessageStatsCTE() BunCTEFragment {
	from := "messages AS message\nJOIN " + bunAnalyticsFilteredSessionsCTE +
		" AS session ON session.id = message.session_id"
	if len(b.models) > 0 {
		from = bunAnalyticsScopedMessagesCTE + " AS message"
	}
	return BunCTEFragment{
		Name: bunAnalyticsShapeMessageStatsCTE,
		Query: BunSQL(`SELECT message.session_id,
	CAST(COUNT(*) AS BIGINT) AS messages,
	CAST(SUM(CASE WHEN message.role = 'user' AND NOT message.is_system
		THEN 1 ELSE 0 END) AS BIGINT) AS user_messages,
	CAST(SUM(CASE WHEN message.role = 'assistant' AND message.has_tool_use
		THEN 1 ELSE 0 END) AS BIGINT) AS tool_use_messages
FROM ` + from + `
GROUP BY message.session_id`),
	}
}

func shapeBunAnalyticsSessionShape(
	rows []bunAnalyticsSessionShapeRow,
) SessionShapeResponse {
	lengths, durations, autonomy := map[string]int{}, map[string]int{}, map[string]int{}
	for _, row := range rows {
		lengths[lengthBucket(row.MessageCount)]++
		if row.DurationSeconds != nil {
			seconds := *row.DurationSeconds
			if nearest := math.Round(seconds); math.Abs(seconds-nearest) < 0.0001 {
				seconds = nearest
			}
			durations[durationBucket(seconds/60)]++
		}
		if row.UserMessages > 0 {
			autonomy[autonomyBucket(
				float64(row.ToolUseMessages)/float64(row.UserMessages),
			)]++
		}
	}
	return SessionShapeResponse{
		Count:                len(rows),
		LengthDistribution:   mapToBuckets(lengths, lengthOrder),
		DurationDistribution: mapToBuckets(durations, durationOrder),
		AutonomyDistribution: mapToBuckets(autonomy, autonomyOrder),
	}
}
