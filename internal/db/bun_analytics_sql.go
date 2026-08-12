package db

import (
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

const (
	bunAnalyticsFilteredSessionsCTE  = "analytics_filtered_sessions"
	bunAnalyticsMessageCandidatesCTE = "analytics_message_candidates"
	bunAnalyticsScopedMessagesCTE    = "analytics_scoped_messages"
	bunAnalyticsQualifiedSessionsCTE = "analytics_qualified_sessions"
	bunAnalyticsToolFactsCTE         = "analytics_tool_facts"
)

// bunAnalyticsSQL builds the relational input shared by analytics panels.
// It owns filtering and pairing; a BunAnalyticsDialect supplies only scalar
// expressions whose spelling differs between engines.
type bunAnalyticsSQL struct {
	dialect BunAnalyticsDialect
	filter  AnalyticsFilter
	zone    string
	models  []string
}

func newBunAnalyticsSQL(
	dialect BunAnalyticsDialect, f AnalyticsFilter,
) (*bunAnalyticsSQL, error) {
	if dialect == nil {
		return nil, fmt.Errorf("db: analytics SQL dialect is unavailable")
	}
	zone, err := NormalizeSessionTimezone(f.Timezone)
	if err != nil {
		return nil, err
	}
	return &bunAnalyticsSQL{
		dialect: dialect, filter: f, zone: zone, models: csvFilterValues(f.Model),
	}, nil
}

// filteredSessionsCTE applies canonical session-grain filters. Date and
// day/hour filters intentionally remain outside this CTE: panels choose
// whether those predicates qualify a session or individual message facts.
func (b *bunAnalyticsSQL) filteredSessionsCTE(
	store bun.IDB, timestampOrderExpr func(string) string, applyDate bool,
) BunCTEFragment {
	query := store.NewSelect().TableExpr("sessions AS session").
		ColumnExpr("session.*").
		Where("session.message_count > 0").
		Where("session.deleted_at IS NULL")

	switch {
	case b.filter.IncludeSubagents && b.filter.IncludeForks:
	case b.filter.IncludeSubagents:
		query = query.Where("session.relationship_type != 'fork'")
	case b.filter.IncludeForks:
		query = query.Where("session.relationship_type != 'subagent'")
	default:
		query = query.Where("session.relationship_type NOT IN ('subagent', 'fork')")
	}
	query = appendBunAnalyticsCSVFilter(query, "session.machine", b.filter.Machine)
	query = appendBunAnalyticsCSVFilter(query, "session.agent", b.filter.Agent)
	if b.filter.Project != "" {
		query = query.Where("session.project = ?", b.filter.Project)
	}
	if b.filter.GitBranch != "" {
		clause, args := BranchPairClauseArgs(
			"session.project", "session.git_branch", b.filter.GitBranch, nil,
		)
		query = query.Where(clause, args...)
	}
	if b.filter.MinUserMessages > 0 {
		query = query.Where("session.user_message_count >= ?", b.filter.MinUserMessages)
	}
	scope := normalizeAutomatedScope(
		b.filter.AutomatedScope, b.filter.ExcludeAutomated,
	)
	if b.filter.ExcludeOneShot {
		if b.filter.IncludeSubagents {
			query = query.Where("(session.user_message_count > 1 OR "+
				"session.relationship_type = 'subagent' OR session.is_automated = ?)", true)
		} else if scope == "human" {
			query = query.Where("session.user_message_count > 1")
		} else {
			query = query.Where("(session.user_message_count > 1 OR "+
				"session.is_automated = ?)", true)
		}
	}
	switch scope {
	case "human":
		query = query.Where("session.is_automated = ?", false)
	case "automated":
		query = query.Where("session.is_automated = ?", true)
	}
	if b.filter.ExcludeInteractive {
		query = query.Where("session.is_automated = ?", true)
	}
	query = appendBunAnalyticsTerminationFilter(
		query, b.filter.Termination, timestampOrderExpr, time.Now().UTC(),
	)
	if len(b.models) > 0 {
		query = query.Where("EXISTS (SELECT 1 FROM messages AS analytics_model "+
			"WHERE analytics_model.session_id = session.id "+
			"AND analytics_model.model IN (?))", bun.List(b.models))
	}
	if b.filter.ActiveSince != "" {
		activity := timestampOrderExpr("COALESCE(" +
			bunNullableTimestamp("session.ended_at") + ", " +
			bunNullableTimestamp("session.started_at") + ", session.created_at)")
		query = query.Where(activity+" >= "+timestampOrderExpr("?"), b.filter.ActiveSince)
	}
	if applyDate {
		local := b.dialect.LocalTimestamp(
			"COALESCE("+bunNullableTimestamp("session.started_at")+
				", session.created_at)", b.zone,
		)
		query = appendBunAnalyticsFragmentRange(
			query, b.dialect.Date(local), b.filter.From, b.filter.To,
		)
	}

	return BunCTEFragment{
		Name:  bunAnalyticsFilteredSessionsCTE,
		Query: BunSQL("?", query),
	}
}

func appendBunAnalyticsFragmentRange(
	query *bun.SelectQuery, expression BunSQLFragment, from, to string,
) *bun.SelectQuery {
	if from != "" {
		query = query.Where(expression.SQL+" >= ?",
			append(append([]any(nil), expression.Args...), from)...)
	}
	if to != "" {
		query = query.Where(expression.SQL+" <= ?",
			append(append([]any(nil), expression.Args...), to)...)
	}
	return query
}

// scopedMessageCTEs preserves the existing model ownership rule in SQL. A
// blank-model real user turn belongs to the next assistant turn; a non-selected
// assistant clears it. Selected non-assistant rows retain their direct model.
func (b *bunAnalyticsSQL) scopedMessageCTEs() []BunCTEFragment {
	local := b.dialect.LocalTimestamp("candidate.timestamp", b.zone)
	predicates := b.messageDayHourPredicates(local)
	var ownership BunSQLFragment
	if len(b.models) == 0 {
		ownership = BunSQL("1 = 1")
	} else {
		ownership = BunSQL(`(
		candidate.model IN (?)
		OR (
			candidate.role = 'user'
			AND candidate.is_system = FALSE
			AND COALESCE(candidate.model, '') = ''
			AND next_assistant.model IN (?)
		)
	)`, bun.List(b.models), bun.List(b.models))
	}
	predicates = append([]BunSQLFragment{ownership}, predicates...)
	where := JoinBunSQLFragments(" AND ", predicates...)

	return []BunCTEFragment{
		{
			Name: bunAnalyticsMessageCandidatesCTE,
			Query: BunSQL(`SELECT message.*,
		MIN(CASE WHEN message.role = 'assistant' THEN message.ordinal END) OVER (
			PARTITION BY message.session_id
			ORDER BY message.ordinal
			ROWS BETWEEN 1 FOLLOWING AND UNBOUNDED FOLLOWING
		) AS next_assistant_ordinal
	FROM messages AS message
	JOIN ` + bunAnalyticsFilteredSessionsCTE + ` AS session
		ON session.id = message.session_id`),
		},
		{
			Name: bunAnalyticsScopedMessagesCTE,
			Query: BunSQL(`SELECT candidate.*
	FROM `+bunAnalyticsMessageCandidatesCTE+` AS candidate
	LEFT JOIN `+bunAnalyticsMessageCandidatesCTE+` AS next_assistant
		ON next_assistant.session_id = candidate.session_id
		AND next_assistant.ordinal = candidate.next_assistant_ordinal
	WHERE `+where.SQL, where.Args...),
		},
		b.qualifiedSessionsCTE(),
	}
}

// qualifiedSessionsCTE turns message-grain time/model scope into a compact
// session set. Panels whose contract says a matching message qualifies the
// whole session join this CTE and then aggregate all facts for those sessions.
func (b *bunAnalyticsSQL) qualifiedSessionsCTE() BunCTEFragment {
	return BunCTEFragment{
		Name: bunAnalyticsQualifiedSessionsCTE,
		Query: BunSQL("SELECT session_id FROM " +
			bunAnalyticsScopedMessagesCTE + " GROUP BY session_id"),
	}
}

// directToolFactsCTE deliberately scopes a tool by its owning message's direct
// model and timestamp. Paired blank-model user ownership does not apply to
// tool calls.
func (b *bunAnalyticsSQL) directToolFactsCTE() BunCTEFragment {
	local := b.dialect.LocalTimestamp("COALESCE("+
		bunNullableTimestamp("message.timestamp")+", "+
		bunNullableTimestamp("session.started_at")+", session.created_at)", b.zone)
	predicates := append(
		b.timestampRangePredicates(local), b.messageDayHourPredicates(local)...,
	)
	if len(b.models) > 0 {
		predicates = append([]BunSQLFragment{
			BunSQL("message.model IN (?)", bun.List(b.models)),
		}, predicates...)
	}
	where := JoinBunSQLFragments(" AND ", predicates...)
	if where.SQL == "" {
		where = BunSQL("1 = 1")
	}
	return BunCTEFragment{
		Name: bunAnalyticsToolFactsCTE,
		Query: BunSQL(`SELECT tool_call.*, message.timestamp AS message_timestamp,
		message.model AS message_model
	FROM tool_calls AS tool_call
	JOIN messages AS message
		ON tool_call.session_id = message.session_id
		AND tool_call.message_ordinal = message.ordinal
	JOIN `+bunAnalyticsFilteredSessionsCTE+` AS session
		ON session.id = tool_call.session_id
	WHERE `+where.SQL, where.Args...),
	}
}

func (b *bunAnalyticsSQL) timestampRangePredicates(
	local BunSQLFragment,
) []BunSQLFragment {
	var predicates []BunSQLFragment
	if b.filter.From != "" {
		date := b.dialect.Date(local)
		predicates = append(predicates,
			BunSQL(date.SQL+" >= ?", append(date.Args, b.filter.From)...))
	}
	if b.filter.To != "" {
		date := b.dialect.Date(local)
		predicates = append(predicates,
			BunSQL(date.SQL+" <= ?", append(date.Args, b.filter.To)...))
	}
	return predicates
}

func (b *bunAnalyticsSQL) messageDayHourPredicates(
	local BunSQLFragment,
) []BunSQLFragment {
	var predicates []BunSQLFragment
	if b.filter.DayOfWeek != nil {
		weekday := b.dialect.ISOWeekday(local)
		predicates = append(predicates,
			BunSQL(weekday.SQL+" = ?", append(weekday.Args, *b.filter.DayOfWeek)...))
	}
	if b.filter.Hour != nil {
		hour := b.dialect.Hour(local)
		predicates = append(predicates,
			BunSQL(hour.SQL+" = ?", append(hour.Args, *b.filter.Hour)...))
	}
	return predicates
}

// renderBunCTEs composes named CTE fragments while preserving argument order.
func renderBunCTEs(ctes ...BunCTEFragment) BunSQLFragment {
	parts := make([]BunSQLFragment, 0, len(ctes))
	for _, cte := range ctes {
		if strings.TrimSpace(cte.Name) == "" || cte.Query.SQL == "" {
			continue
		}
		parts = append(parts, BunSQL(cte.Name+" AS ("+cte.Query.SQL+")", cte.Query.Args...))
	}
	return JoinBunSQLFragments(",\n", parts...)
}
