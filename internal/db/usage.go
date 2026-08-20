package db

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// CopilotReportedCostSource identifies the authoritative cumulative cost
// reported by Copilot CLI shutdown records.
const CopilotReportedCostSource = "copilot-reported"

// aiCreditUSD is the USD value of one AI credit for agents whose cost
// is denominated in AI credits (the AICreditsDenominated capability).
const microdollarsPerAICredit = 10_000

// AICreditsFromCost converts a USD cost into AI credits when the
// agent's cost is denominated in AI credits, and returns 0 otherwise.
// It is the single home of the credit conversion shared by the SQLite,
// PostgreSQL, and DuckDB usage paths; a per-agent credit rate would
// slot in here rather than at each accumulation site.
func AICreditsFromCost(agent string, cost money.Money) float64 {
	if cost.Microdollars == 0 || !parser.AgentNameUsesAICredits(agent) {
		return 0
	}
	return float64(cost.Microdollars) / microdollarsPerAICredit
}

// microdollarsPerDollar converts Money.Microdollars to float64 dollars
// for the deprecated CostUSD compatibility field.
const microdollarsPerDollar = 1_000_000.0

// CostUSDFromCost renders cost as float64 dollars for the deprecated
// SessionUsage.CostUSD compatibility field, or nil when hasCost is
// false. It is the single place that performs the microdollars-to-
// dollars conversion, so the SQLite, PostgreSQL, and DuckDB session
// usage paths and the subagent rollup combiner all report an
// identical value for the same cost.
func CostUSDFromCost(hasCost bool, cost money.Money) *float64 {
	if !hasCost {
		return nil
	}
	usd := float64(cost.Microdollars) / microdollarsPerDollar
	return &usd
}

// NoTokenData reports whether a daily-usage total carries neither token
// data nor cost: every token counter, the cost total, and any Copilot AI
// credits are zero. It distinguishes a window whose sessions simply do not
// record token usage from one that genuinely has no sessions.
func NoTokenData(t UsageTotals) bool {
	return t.InputTokens == 0 &&
		t.OutputTokens == 0 &&
		t.CacheCreationTokens == 0 &&
		t.CacheReadTokens == 0 &&
		t.TotalCost.Microdollars == 0 &&
		t.CopilotAICredits == 0
}

// UsageFilter controls the date range, agent, and timezone
// for daily usage aggregation queries.
type UsageFilter struct {
	From    string // YYYY-MM-DD, inclusive
	To      string // YYYY-MM-DD, inclusive
	Agent   string // "" for all; supports comma-separated
	Project string // "" for all; supports comma-separated
	Machine string // "" for all; supports comma-separated
	// ProjectLabels and ExcludeProjectLabels preserve exact internal labels
	// that may themselves contain commas. A non-nil slice takes precedence
	// over the legacy comma-separated string field.
	ProjectLabels        []string
	ExcludeProjectLabels []string
	// GitBranch is a branchListSep-joined list of opaque (project, branch) tokens (EncodeBranchFilterToken).
	GitBranch         string
	Model             string // "" for all; supports comma-separated
	ExcludeProject    string // comma-separated projects to exclude
	ExcludeAgent      string // comma-separated agents to exclude
	ExcludeModel      string // comma-separated models to exclude
	Timezone          string // IANA timezone, "" for UTC
	MinUserMessages   int    // user_message_count >= N
	ExcludeOneShot    bool   // user_message_count > 1
	ExcludeAutomated  bool   // is_automated = false
	AutomatedScope    string // "", "human", "all", or "automated"
	ActiveSince       string // RFC3339 session recency cutoff
	Termination       string // "", "clean", "unclean", "active", or "stale"
	Breakdowns        bool   // populate Project/AgentBreakdowns per day
	SkipSessionCounts bool   // skip distinct session counts when callers do not need them
	// TopSessionsSort ranks GetTopSessionsByCost results: ""/"cost"
	// (default) or "tokens". Ignored by other usage queries.
	TopSessionsSort string
	// TopSessionsTokenTypes selects the counters used for token ranking.
	// The zero value means all token types.
	TopSessionsTokenTypes UsageTokenTypes
}

// ProjectFilterLabels returns exact include labels when present, otherwise it
// decodes the legacy comma-separated project filter.
func (f UsageFilter) ProjectFilterLabels() []string {
	if f.ProjectLabels != nil {
		return f.ProjectLabels
	}
	if f.Project == "" {
		return nil
	}
	return strings.Split(f.Project, ",")
}

// ExcludedProjectFilterLabels returns exact exclude labels when present,
// otherwise it decodes the legacy comma-separated project filter.
func (f UsageFilter) ExcludedProjectFilterLabels() []string {
	if f.ExcludeProjectLabels != nil {
		return f.ExcludeProjectLabels
	}
	if f.ExcludeProject == "" {
		return nil
	}
	return strings.Split(f.ExcludeProject, ",")
}

func (f UsageFilter) appendUsageBranchFilterClauses(
	where string, args []any, modelCol string,
) (string, []any) {
	where, args = f.appendUsageSourceFilterClauses(where, args, modelCol)
	return f.appendUsageSessionFilterClauses(where, args)
}

func (f UsageFilter) appendUsageSourceFilterClauses(
	where string, args []any, modelCol string,
) (string, []any) {
	appendCSV := func(
		q string, a []any, col, csv string, include bool,
	) (string, []any) {
		if csv == "" {
			return q, a
		}
		vals := strings.Split(csv, ",")
		op := "IN"
		if !include {
			op = "NOT IN"
		}
		if len(vals) == 1 {
			if include {
				q += "\n\tAND " + col + " = ?"
			} else {
				q += "\n\tAND " + col + " != ?"
			}
			a = append(a, vals[0])
		} else {
			ph := make([]string, len(vals))
			for i, v := range vals {
				ph[i] = "?"
				a = append(a, v)
			}
			q += "\n\tAND " + col + " " + op +
				" (" + strings.Join(ph, ",") + ")"
		}
		return q, a
	}

	where, args = appendCSV(where, args, modelCol, f.Model, true)
	where, args = appendCSV(where, args, modelCol, f.ExcludeModel, false)

	return where, args
}

func (f UsageFilter) appendUsageSessionFilterClauses(
	where string, args []any,
) (string, []any) {
	appendValues := func(
		q string, a []any, col string, vals []string, include bool,
	) (string, []any) {
		if len(vals) == 0 {
			return q, a
		}
		op := "IN"
		if !include {
			op = "NOT IN"
		}
		if len(vals) == 1 {
			if include {
				q += "\n\tAND " + col + " = ?"
			} else {
				q += "\n\tAND " + col + " != ?"
			}
			a = append(a, vals[0])
		} else {
			ph := make([]string, len(vals))
			for i, v := range vals {
				ph[i] = "?"
				a = append(a, v)
			}
			q += "\n\tAND " + col + " " + op +
				" (" + strings.Join(ph, ",") + ")"
		}
		return q, a
	}
	appendCSV := func(
		q string, a []any, col, csv string, include bool,
	) (string, []any) {
		if csv == "" {
			return q, a
		}
		return appendValues(q, a, col, strings.Split(csv, ","), include)
	}

	where, args = appendCSV(where, args, "s.agent", f.Agent, true)
	where, args = appendValues(
		where, args, "s.project", f.ProjectFilterLabels(), true,
	)
	where, args = appendCSV(where, args, "s.machine", f.Machine, true)
	if f.GitBranch != "" {
		var clause string
		clause, args = BranchPairClauseArgs("s.project", "s.git_branch", f.GitBranch, args)
		where += "\n\tAND " + clause
	}
	where, args = appendValues(
		where, args, "s.project", f.ExcludedProjectFilterLabels(), false,
	)
	where, args = appendCSV(where, args, "s.agent", f.ExcludeAgent, false)

	if f.MinUserMessages > 0 {
		where += "\n\tAND s.user_message_count >= ?"
		args = append(args, f.MinUserMessages)
	}
	scope := normalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	if f.ExcludeOneShot {
		if scope == "human" {
			where += "\n\tAND s.user_message_count > 1"
		} else {
			where += "\n\tAND (s.user_message_count > 1 OR COALESCE(s.is_automated, 0) = 1)"
		}
	}
	if pred := automatedScopePredicate(scope, "COALESCE(s.is_automated, 0)"); pred != "" {
		where += "\n\tAND " + pred
	}
	if f.ActiveSince != "" {
		where += "\n\tAND COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, ''), s.created_at) >= ?"
		args = append(args, f.ActiveSince)
	}
	if pred, pargs := buildUsageTerminationPredSQLite(f.Termination); pred != "" {
		where += "\n\tAND " + pred
		args = append(args, pargs...)
	}

	return where, args
}

// appendUsageMatchingActivityClauses requires the session to have at
// least one row that GetUsageMatchingSessionCount's bounded branch would
// count: an assistant, non-synthetic message (model optional — some
// Copilot assistant messages parse before a model name is known) or a
// usage_events row with a model. Model/ExcludeModel narrow those same
// rows. Seeding the EXISTS subqueries with the matching eligibility
// predicates keeps the unbounded branch's semantics aligned with the
// bounded branch's per-row predicates, so the same filter matches the
// same sessions whether or not a date range is set.
func (f UsageFilter) appendUsageMatchingActivityClauses(
	where string, args []any,
) (string, []any) {
	var messageArgs []any
	messageWhere, messageArgs := f.appendUsageSourceFilterClauses(
		usageMatchingMessageSourceEligibility, messageArgs, "m.model",
	)
	var eventArgs []any
	eventWhere, eventArgs := f.appendUsageSourceFilterClauses(
		usageEventSourceEligibility, eventArgs, "ue.model",
	)

	where += `
	AND (
		EXISTS (
			SELECT 1
			FROM messages m
			WHERE m.session_id = s.id
				AND ` + messageWhere + `
		)
		OR EXISTS (
			SELECT 1
			FROM usage_events ue
			WHERE ue.session_id = s.id
				AND ` + eventWhere + `
		)
	)`
	args = append(args, messageArgs...)
	args = append(args, eventArgs...)
	return where, args
}

func buildUsageTerminationPredSQLite(status string) (string, []any) {
	if status == "" || status == "all" {
		return "", nil
	}
	now := time.Now().Unix()
	activeCutoff := now - int64(activeWindow.Seconds())
	staleCutoff := now - int64(staleWindow.Seconds())
	const activityExpr = "CAST(strftime('%s', COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, ''), s.created_at)) AS INTEGER)"
	const flagged = "s.termination_status IN ('tool_call_pending', 'truncated')"

	parts := strings.Split(status, ",")
	preds := make([]string, 0, len(parts))
	args := make([]any, 0, len(parts)*2)
	for _, p := range parts {
		switch strings.TrimSpace(p) {
		case "active":
			preds = append(preds, activityExpr+" > ?")
			args = append(args, activeCutoff)
		case "stale":
			preds = append(preds, "("+activityExpr+" > ? AND "+
				activityExpr+" <= ? AND "+flagged+")")
			args = append(args, staleCutoff, activeCutoff)
		case "unclean":
			preds = append(preds, "("+activityExpr+" <= ? AND "+flagged+")")
			args = append(args, staleCutoff)
		case "clean":
			preds = append(preds, "s.termination_status = 'clean'")
		case "awaiting_user":
			preds = append(preds, "s.termination_status = 'awaiting_user'")
		}
	}
	if len(preds) == 0 {
		return "", nil
	}
	if len(preds) == 1 {
		return preds[0], args
	}
	return "(" + strings.Join(preds, " OR ") + ")", args
}

// location loads the timezone or returns the system local timezone.
var usageLocationCache sync.Map

func (f UsageFilter) location() *time.Location {
	if f.Timezone == "" {
		return time.Local
	}
	if cached, ok := usageLocationCache.Load(f.Timezone); ok {
		return cached.(*time.Location)
	}
	loc, err := time.LoadLocation(f.Timezone)
	if err != nil {
		return time.Local
	}
	actual, _ := usageLocationCache.LoadOrStore(f.Timezone, loc)
	return actual.(*time.Location)
}

// usageMessageEligibility is the WHERE-clause fragment that selects
// messages eligible for usage / cost aggregation. Every usage query
// (GetDailyUsage, GetUsageSessionCounts, GetTopSessionsByCost) MUST
// reference this constant so the set of counted messages stays
// identical across queries. Drift here is the bug that makes
// sessionCounts and daily totals disagree.
//
// Note: this does NOT filter by s.relationship_type. Duplicate
// messages across fork/subagent boundaries are handled by the
// per-query usage dedup in GetDailyUsage, which prefers the
// Claude message/request pair and falls back to persisted source
// identity when the pair is incomplete. That is more precise than
// a blanket exclusion: a fork session can legitimately contribute
// unique-keyed messages that should still be counted (see
// TestGetDailyUsage_DedupesByClaudeMessageAndRequestID).
const usageMessageEligibility = `
    m.token_usage != ''
    AND m.model != ''
    AND m.model != '<synthetic>'
    AND s.deleted_at IS NULL`

const usageMessageSourceEligibility = `
    m.token_usage != ''
    AND m.model != ''
    AND m.model != '<synthetic>'`

// usageMatchingMessageEligibility is usageMessageEligibility with the
// token-presence requirement removed and the model-presence requirement
// relaxed to a role check. GetUsageMatchingSessionCount counts sessions
// that have usage-shaped activity even when the agent (e.g. Copilot)
// never records per-message tokens or, for some assistant messages, a
// model name, so it must not gate on m.token_usage or m.model != ” the
// way every token/cost query does; Model/ExcludeModel filters are applied
// separately and still narrow the match when set. Do not reuse this for
// usageRowQuery or its callers — see the usageMessageEligibility doc
// comment above.
const usageMatchingMessageEligibility = `
    m.role = 'assistant'
    AND m.model != '<synthetic>'
    AND s.deleted_at IS NULL`

const usageMatchingMessageSourceEligibility = `
    m.role = 'assistant'
    AND m.model != '<synthetic>'`

const usageEventEligibility = `
    ue.model != ''
    AND s.deleted_at IS NULL`

const usageEventSourceEligibility = `
    ue.model != ''`

const usageSessionEligibility = `s.deleted_at IS NULL`

const usageRowsSQLTemplate = `
SELECT
	m.session_id,
	m.ordinal AS message_ordinal,
	'message' AS usage_source,
	COALESCE(NULLIF(m.timestamp, ''), s.started_at, '') AS ts,
	m.model,
	m.token_usage,
	0 AS input_tokens,
	0 AS output_tokens,
	0 AS cache_creation_input_tokens,
	0 AS cache_read_input_tokens,
	CASE
		WHEN json_valid(m.token_usage) THEN COALESCE(CAST(json_extract(m.token_usage, '$.reasoning_tokens') AS INTEGER), 0)
		ELSE 0
	END AS reasoning_tokens,
	NULL AS cost_microdollars,
	'' AS cost_status,
	'' AS cost_source,
	m.claude_message_id,
	m.claude_request_id,
	m.source_uuid,
	'' AS usage_dedup_key,
	s.project,
	s.agent,
	s.machine,
	s.user_message_count,
	COALESCE(s.is_automated, 0) AS is_automated,
	COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, ''), s.created_at) AS session_activity_at,
	COALESCE(s.termination_status, '') AS termination_status,
	COALESCE(NULLIF(COALESCE(s.display_name, s.session_name), ''), NULLIF(s.first_message, ''), NULLIF(s.project, ''), s.id) AS display_name,
	COALESCE(s.started_at, '') AS started_at
FROM messages m
JOIN sessions s ON m.session_id = s.id
WHERE %s

UNION ALL

SELECT
	ue.session_id,
	ue.message_ordinal,
	ue.source AS usage_source,
	COALESCE(ue.occurred_at, s.started_at, '') AS ts,
	ue.model,
	'' AS token_usage,
	ue.input_tokens,
	ue.output_tokens,
	ue.cache_creation_input_tokens,
	ue.cache_read_input_tokens,
	ue.reasoning_tokens,
	ue.cost_microdollars,
	ue.cost_status,
	ue.cost_source,
	'' AS claude_message_id,
	'' AS claude_request_id,
	'' AS source_uuid,
	CASE
		WHEN ue.dedup_key != '' THEN ue.session_id || ':' || ue.source || ':' || ue.dedup_key
		ELSE ue.session_id || ':' || ue.source || ':id:' || ue.id
	END AS usage_dedup_key,
	s.project,
	s.agent,
	s.machine,
	s.user_message_count,
	COALESCE(s.is_automated, 0) AS is_automated,
	COALESCE(NULLIF(s.ended_at, ''), NULLIF(s.started_at, ''), s.created_at) AS session_activity_at,
	COALESCE(s.termination_status, '') AS termination_status,
	COALESCE(NULLIF(COALESCE(s.display_name, s.session_name), ''), NULLIF(s.first_message, ''), NULLIF(s.project, ''), s.id) AS display_name,
	COALESCE(s.started_at, '') AS started_at
FROM usage_events ue
JOIN sessions s ON s.id = ue.session_id
WHERE %s`

func usageRowsSQLWithWhere(
	messageWhere, usageEventWhere string,
) string {
	return fmt.Sprintf(
		usageRowsSQLTemplate,
		messageWhere,
		usageEventWhere,
	)
}

const dailyUsageRowsSQLTemplate = `
SELECT
	m.session_id,
	m.ordinal AS message_ordinal,
	'message' AS usage_source,
	COALESCE(NULLIF(m.timestamp, ''), s.started_at, '') AS ts,
	m.model,
	m.token_usage,
	0 AS input_tokens,
	0 AS output_tokens,
	0 AS cache_creation_input_tokens,
	0 AS cache_read_input_tokens,
	CASE
		WHEN json_valid(m.token_usage) THEN COALESCE(CAST(json_extract(m.token_usage, '$.reasoning_tokens') AS INTEGER), 0)
		ELSE 0
	END AS reasoning_tokens,
	NULL AS cost_microdollars,
	'' AS cost_source,
	m.claude_message_id,
	m.claude_request_id,
	m.source_uuid,
	'' AS usage_dedup_key,
	s.project,
	s.agent,
	s.machine
FROM messages m
JOIN sessions s ON m.session_id = s.id
WHERE %s

UNION ALL

SELECT
	ue.session_id,
	ue.message_ordinal,
	ue.source AS usage_source,
	COALESCE(ue.occurred_at, s.started_at, '') AS ts,
	ue.model,
	'' AS token_usage,
	ue.input_tokens,
	ue.output_tokens,
	ue.cache_creation_input_tokens,
	ue.cache_read_input_tokens,
	ue.reasoning_tokens,
	ue.cost_microdollars,
	ue.cost_source,
	'' AS claude_message_id,
	'' AS claude_request_id,
	'' AS source_uuid,
	CASE
		WHEN ue.dedup_key != '' THEN ue.session_id || ':' || ue.source || ':' || ue.dedup_key
		ELSE ue.session_id || ':' || ue.source || ':id:' || ue.id
	END AS usage_dedup_key,
	s.project,
	s.agent,
	s.machine
FROM usage_events ue
JOIN sessions s ON s.id = ue.session_id
WHERE %s`

const dailyUsageMessageRowsSQLTemplate = `
SELECT
	m.session_id,
	m.ordinal AS message_ordinal,
	'message' AS usage_source,
	COALESCE(NULLIF(m.timestamp, ''), s.started_at, '') AS ts,
	m.model,
	m.token_usage,
	0 AS input_tokens,
	0 AS output_tokens,
	0 AS cache_creation_input_tokens,
	0 AS cache_read_input_tokens,
	CASE
		WHEN json_valid(m.token_usage) THEN COALESCE(CAST(json_extract(m.token_usage, '$.reasoning_tokens') AS INTEGER), 0)
		ELSE 0
	END AS reasoning_tokens,
	NULL AS cost_microdollars,
	'' AS cost_source,
	m.claude_message_id,
	m.claude_request_id,
	m.source_uuid,
	'' AS usage_dedup_key,
	s.project,
	s.agent,
	s.machine
FROM %s m
JOIN sessions s ON m.session_id = s.id
WHERE %s`

const dailyUsageEventRowsSQLTemplate = `
SELECT
	ue.session_id,
	ue.message_ordinal,
	ue.source AS usage_source,
	COALESCE(ue.occurred_at, s.started_at, '') AS ts,
	ue.model,
	'' AS token_usage,
	ue.input_tokens,
	ue.output_tokens,
	ue.cache_creation_input_tokens,
	ue.cache_read_input_tokens,
	ue.reasoning_tokens,
	ue.cost_microdollars,
	ue.cost_source,
	'' AS claude_message_id,
	'' AS claude_request_id,
	'' AS source_uuid,
	CASE
		WHEN ue.dedup_key != '' THEN ue.session_id || ':' || ue.source || ':' || ue.dedup_key
		ELSE ue.session_id || ':' || ue.source || ':id:' || ue.id
	END AS usage_dedup_key,
	s.project,
	s.agent,
	s.machine
FROM %s ue
JOIN sessions s ON s.id = ue.session_id
WHERE %s`

func dailyUsageRowsSQLWithWhere(
	messageWhere, usageEventWhere string,
) string {
	return fmt.Sprintf(
		dailyUsageRowsSQLTemplate,
		messageWhere,
		usageEventWhere,
	)
}

func dailyUsageRowsSQLWithTimestampCTEs(
	messageTimestampWhere, eventTimestampWhere string,
	messageTimestampJoinWhere, eventTimestampJoinWhere string,
	messageFallbackWhere, eventFallbackWhere string,
) string {
	return `
WITH
message_timestamp_rows AS MATERIALIZED (
	SELECT
		m.session_id,
		m.ordinal,
		NULLIF(m.timestamp, '') AS timestamp,
		m.model,
		m.token_usage,
		m.claude_message_id,
		m.claude_request_id,
		m.source_uuid
	FROM messages m
	WHERE ` + messageTimestampWhere + `
),
usage_event_timestamp_rows AS MATERIALIZED (
	SELECT
		ue.id,
		ue.session_id,
		ue.message_ordinal,
		ue.source,
		ue.occurred_at,
		ue.model,
		ue.input_tokens,
			ue.output_tokens,
			ue.cache_creation_input_tokens,
			ue.cache_read_input_tokens,
			ue.reasoning_tokens,
			ue.cost_microdollars,
			ue.cost_source,
		ue.dedup_key
	FROM usage_events ue
	WHERE ` + eventTimestampWhere + `
)
` + fmt.Sprintf(
		dailyUsageMessageRowsSQLTemplate,
		"message_timestamp_rows",
		messageTimestampJoinWhere,
	) + `

UNION ALL

` + fmt.Sprintf(
		dailyUsageEventRowsSQLTemplate,
		"usage_event_timestamp_rows",
		eventTimestampJoinWhere,
	) + `

UNION ALL

` + fmt.Sprintf(
		dailyUsageMessageRowsSQLTemplate,
		"messages",
		messageFallbackWhere,
	) + `

UNION ALL

` + fmt.Sprintf(
		dailyUsageEventRowsSQLTemplate,
		"usage_events",
		eventFallbackWhere,
	)
}

type usageScanRow struct {
	sessionID                string
	messageOrdinal           sql.NullInt64
	usageSource              string
	ts                       string
	model                    string
	tokenJSON                string
	inputTokens              int
	outputTokens             int
	cacheCreationInputTokens int
	cacheReadInputTokens     int
	reasoningTokens          int
	cost                     sql.NullInt64
	costStatus               string
	costSource               string
	claudeMessageID          string
	claudeRequestID          string
	sourceUUID               string
	usageDedupKey            string
	project                  string
	agent                    string
	machine                  string
	userMessageCount         int
	isAutomated              int
	sessionActivityAt        string
	terminationStatus        string
	displayName              string
	startedAt                string
}

type dailyUsageScanRow struct {
	sessionID                string
	messageOrdinal           sql.NullInt64
	usageSource              string
	ts                       string
	model                    string
	tokenJSON                string
	webSearchRequests        sql.NullInt64
	inputTokens              int
	outputTokens             int
	cacheCreationInputTokens int
	cacheReadInputTokens     int
	reasoningTokens          int
	cost                     sql.NullInt64
	costSource               string
	claudeMessageID          string
	claudeRequestID          string
	sourceUUID               string
	usageDedupKey            string
	project                  string
	agent                    string
	machine                  string
}

type topSessionMetadata struct {
	displayName string
	agent       string
	project     string
	startedAt   string
}

func usageRowSelectFromRows(rowsSQL string) string {
	return `
SELECT
	u.session_id,
	u.message_ordinal,
	u.usage_source,
	u.ts,
	u.model,
	u.token_usage,
	u.input_tokens,
	u.output_tokens,
	u.cache_creation_input_tokens,
	u.cache_read_input_tokens,
	u.reasoning_tokens,
	u.cost_microdollars,
	u.cost_status,
	u.cost_source,
	u.claude_message_id,
	u.claude_request_id,
	u.source_uuid,
	u.usage_dedup_key,
	u.project,
	u.agent,
	u.machine,
	u.user_message_count,
	u.is_automated,
	u.session_activity_at,
	u.termination_status,
	u.display_name,
	u.started_at
FROM (` + rowsSQL + `) u
WHERE 1=1`
}

func usageRowSelect() string {
	return usageRowSelectFromRows(usageRowsSQLWithWhere(
		usageMessageEligibility,
		usageEventEligibility,
	))
}

func dailyUsageRowSelectFromRows(rowsSQL string) string {
	return dailyUsageRowSelectFromRowsWithMachine(rowsSQL, false)
}

// dailyUsageRowColumns names the per-row sources the daily usage select
// reads: the session a row is attributed to, its billed web-search count,
// and the session metadata reported alongside it.
type dailyUsageRowColumns struct {
	session   string
	webSearch string
	project   string
	agent     string
	machine   string
}

func dailyUsageRowSelectFromRowsWithMachine(
	rowsSQL string, includeMachine bool,
) string {
	return dailyUsageRowSelectFromRowsWithColumns(
		rowsSQL, includeMachine, dailyUsageRowColumns{
			session: "u.session_id",
			webSearch: `CASE
		WHEN u.usage_source = 'message' THEN MAX(COALESCE(CASE
			WHEN json_valid(u.token_usage) THEN CAST(json_extract(
				u.token_usage, '$.server_tool_use.web_search_requests'
			) AS INTEGER)
			ELSE agentsview_usage_web_search_requests(u.token_usage)
		END, 0), 0)
		ELSE 0
	END`,
			project: "u.project",
			agent:   "u.agent",
			machine: "u.machine",
		})
}

// dailyUsageRowSelectFromSnapshotRowsWithMachine reads rows produced by
// snapshotRankedDailyUsageRowsSQL, which already carry the attributed
// session, its metadata, and the partition-wide web-search count (NULL
// for rows that were not ranked, which the scanner parses in Go).
func dailyUsageRowSelectFromSnapshotRowsWithMachine(
	rowsSQL string, includeMachine bool,
) string {
	return dailyUsageRowSelectFromRowsWithColumns(
		rowsSQL, includeMachine, dailyUsageRowColumns{
			session:   "u.snapshot_attribution_session_id",
			webSearch: "u.snapshot_web_search_requests",
			project:   "u.snapshot_project",
			agent:     "u.snapshot_agent",
			machine:   "u.snapshot_machine",
		})
}

func dailyUsageRowSelectFromRowsWithColumns(
	rowsSQL string, includeMachine bool, cols dailyUsageRowColumns,
) string {
	machineColumn := ""
	if includeMachine {
		machineColumn = ",\n\t" + cols.machine + " AS machine"
	}
	return `
SELECT
	` + cols.session + `,
	u.message_ordinal,
	u.usage_source,
	u.ts,
	u.model,
	u.token_usage,
	` + cols.webSearch + ` AS web_search_requests,
	u.input_tokens,
		u.output_tokens,
		u.cache_creation_input_tokens,
		u.cache_read_input_tokens,
	u.reasoning_tokens,
	u.cost_microdollars,
	u.cost_source,
	u.claude_message_id,
	u.claude_request_id,
	u.source_uuid,
	u.usage_dedup_key,
	` + cols.project + ` AS project,
	` + cols.agent + ` AS agent` + machineColumn + `
FROM (` + rowsSQL + `) u
WHERE 1=1`
}

type usageBounds struct {
	from string
	to   string
}

func (b usageBounds) bounded() bool {
	return b.from != "" || b.to != ""
}

func usageBoundsForFilter(f UsageFilter) usageBounds {
	var b usageBounds
	if f.From != "" {
		b.from = paddedUTCBound(f.From+"T00:00:00Z", -14)
	}
	if f.To != "" {
		b.to = paddedUTCBound(f.To+"T23:59:59Z", 14)
	}
	return b
}

func appendUsageColumnBounds(
	where, col string, b usageBounds, args []any,
) (string, []any) {
	if b.from != "" {
		where += "\n\tAND " + col + " >= ?"
		args = append(args, b.from)
	}
	if b.to != "" {
		where += "\n\tAND " + col + " <= ?"
		args = append(args, b.to)
	}
	return where, args
}

func usageRowsSQLForBounds(
	f UsageFilter, b usageBounds,
) (string, []any) {
	if !b.bounded() {
		var messageArgs []any
		messageWhere, messageArgs := f.appendUsageBranchFilterClauses(
			usageMessageEligibility, messageArgs, "m.model")
		var eventArgs []any
		eventWhere, eventArgs := f.appendUsageBranchFilterClauses(
			usageEventEligibility, eventArgs, "ue.model")
		rowsSQL := dailyUsageRowsSQLWithWhere(messageWhere, eventWhere)
		args := make([]any, 0, len(messageArgs)+len(eventArgs))
		args = append(args, messageArgs...)
		args = append(args, eventArgs...)
		return rowsSQL, args
	}

	return usageBoundedRowsSQL(
		f, b, usageMessageSourceEligibility, usageMessageEligibility)
}

// usageBoundedRowsSQL builds the bounded-branch CTE row source shared by
// usageRowsSQLForBounds (token-eligible rows) and
// usageMatchingSessionRowsSQLForBounds (relaxed matching rows). The two
// callers differ only in the message eligibility predicates.
func usageBoundedRowsSQL(
	f UsageFilter, b usageBounds,
	messageSourceEligibility, messageEligibility string,
) (string, []any) {
	messageTimestampSourceWhere := messageSourceEligibility +
		"\n\tAND m.timestamp IS NOT NULL" +
		"\n\tAND m.timestamp != ''"
	var messageTimestampArgs []any
	messageTimestampSourceWhere, messageTimestampArgs =
		f.appendUsageSourceFilterClauses(
			messageTimestampSourceWhere, messageTimestampArgs, "m.model")
	messageTimestampSourceWhere, messageTimestampArgs = appendUsageColumnBounds(
		messageTimestampSourceWhere, "m.timestamp", b, messageTimestampArgs)
	var messageTimestampJoinArgs []any
	messageTimestampJoinWhere, messageTimestampJoinArgs :=
		f.appendUsageSessionFilterClauses(
			usageSessionEligibility, messageTimestampJoinArgs)

	eventTimestampSourceWhere := usageEventSourceEligibility +
		"\n\tAND ue.occurred_at IS NOT NULL"
	var eventTimestampArgs []any
	eventTimestampSourceWhere, eventTimestampArgs =
		f.appendUsageSourceFilterClauses(
			eventTimestampSourceWhere, eventTimestampArgs, "ue.model")
	eventTimestampSourceWhere, eventTimestampArgs = appendUsageColumnBounds(
		eventTimestampSourceWhere, "ue.occurred_at", b, eventTimestampArgs)
	var eventTimestampJoinArgs []any
	eventTimestampJoinWhere, eventTimestampJoinArgs :=
		f.appendUsageSessionFilterClauses(
			usageSessionEligibility, eventTimestampJoinArgs)

	messageFallbackWhere := messageEligibility +
		"\n\tAND NULLIF(m.timestamp, '') IS NULL"
	var messageFallbackArgs []any
	messageFallbackWhere, messageFallbackArgs =
		f.appendUsageBranchFilterClauses(
			messageFallbackWhere, messageFallbackArgs, "m.model")
	messageFallbackWhere, messageFallbackArgs = appendUsageColumnBounds(
		messageFallbackWhere, "s.started_at", b, messageFallbackArgs)

	eventFallbackWhere := usageEventEligibility +
		"\n\tAND ue.occurred_at IS NULL"
	var eventFallbackArgs []any
	eventFallbackWhere, eventFallbackArgs =
		f.appendUsageBranchFilterClauses(
			eventFallbackWhere, eventFallbackArgs, "ue.model")
	eventFallbackWhere, eventFallbackArgs = appendUsageColumnBounds(
		eventFallbackWhere, "s.started_at", b, eventFallbackArgs)

	rowsSQL := dailyUsageRowsSQLWithTimestampCTEs(
		messageTimestampSourceWhere,
		eventTimestampSourceWhere,
		messageTimestampJoinWhere,
		eventTimestampJoinWhere,
		messageFallbackWhere,
		eventFallbackWhere,
	)
	args := make(
		[]any, 0,
		len(messageTimestampArgs)+len(eventTimestampArgs)+
			len(messageTimestampJoinArgs)+len(eventTimestampJoinArgs)+
			len(messageFallbackArgs)+len(eventFallbackArgs),
	)
	args = append(args, messageTimestampArgs...)
	args = append(args, eventTimestampArgs...)
	args = append(args, messageTimestampJoinArgs...)
	args = append(args, eventTimestampJoinArgs...)
	args = append(args, messageFallbackArgs...)
	args = append(args, eventFallbackArgs...)
	return rowsSQL, args
}

// usageMatchingSessionRowsSQLForBounds is usageRowsSQLForBounds's bounded
// branch built from the relaxed usageMatchingMessageEligibility predicates,
// so GetUsageMatchingSessionCount only relaxes the token-usage and
// model-presence requirements and keeps the same per-row
// Model/ExcludeModel filtering as the normal bounded path.
func usageMatchingSessionRowsSQLForBounds(
	f UsageFilter, b usageBounds,
) (string, []any) {
	return usageBoundedRowsSQL(
		f, b,
		usageMatchingMessageSourceEligibility, usageMatchingMessageEligibility)
}

func usageRowQuery(f UsageFilter) (string, []any) {
	rowsSQL, args := usageRowsSQLForBounds(f, usageBoundsForFilter(f))
	query := dailyUsageRowSelectFromRows(rowsSQL)
	return query, args
}

func topSessionsUsageRowQuery(f UsageFilter) (string, []any) {
	bounds := usageBoundsForFilter(f)
	rowsSQL, rowsArgs := usageRowsSQLForBounds(
		usageSnapshotInputFilter(f), bounds)
	rowsSQL, args := snapshotRankedDailyUsageRowsSQL(
		rowsSQL, rowsArgs, f, bounds)
	return dailyUsageRowSelectFromSnapshotRowsWithMachine(rowsSQL, false), args
}

func usageSnapshotInputFilter(f UsageFilter) UsageFilter {
	return UsageFilter{From: f.From, To: f.To, Timezone: f.Timezone}
}

const dailyCursorUsageRowsSQLTemplate = `
SELECT
	'' AS session_id,
	NULL AS message_ordinal,
	'cursor' AS usage_source,
	cu.occurred_at AS ts,
	cu.model,
	'' AS token_usage,
	cu.input_tokens,
	cu.output_tokens,
	cu.cache_write_tokens AS cache_creation_input_tokens,
	cu.cache_read_tokens AS cache_read_input_tokens,
	0 AS reasoning_tokens,
	cu.charged_microdollars AS cost_microdollars,
	'cursor-reported' AS cost_source,
	'' AS claude_message_id,
	'' AS claude_request_id,
	'' AS source_uuid,
	cu.dedup_key AS usage_dedup_key,
	'' AS project,
	'cursor' AS agent,
	'' AS machine
FROM cursor_usage_events cu
WHERE %s`

func cursorUsageRowsSQLForBounds(
	f UsageFilter, b usageBounds,
) (string, []any, bool) {
	termPred, _ := buildUsageTerminationPredSQLite(f.Termination)
	// Cursor usage rows carry no project or git branch and bypass the session
	// filter, so any filter they cannot satisfy (project, machine, branch)
	// must exclude them entirely rather than let them leak into totals.
	if len(f.ProjectFilterLabels()) > 0 ||
		len(f.ExcludedProjectFilterLabels()) > 0 ||
		f.Machine != "" || f.GitBranch != "" || f.MinUserMessages > 0 ||
		f.ExcludeOneShot || termPred != "" ||
		f.ActiveSince != "" {
		return "", nil, false
	}
	if f.Agent != "" {
		vals := strings.Split(f.Agent, ",")
		for i := range vals {
			vals[i] = strings.TrimSpace(vals[i])
		}
		if !slices.Contains(vals, "cursor") {
			return "", nil, false
		}
	}
	if f.ExcludeAgent != "" {
		vals := strings.Split(f.ExcludeAgent, ",")
		for i := range vals {
			vals[i] = strings.TrimSpace(vals[i])
		}
		if slices.Contains(vals, "cursor") {
			return "", nil, false
		}
	}

	where := "cu.model != ''"
	var args []any
	scope := normalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	if pred := automatedScopePredicate(scope, "cu.is_headless"); pred != "" {
		where += "\n\tAND " + pred
	}
	where, args = f.appendUsageSourceFilterClauses(
		where, args, "cu.model",
	)
	where, args = appendUsageColumnBounds(where, "cu.occurred_at", b, args)
	rowsSQL := fmt.Sprintf(dailyCursorUsageRowsSQLTemplate, where)
	return rowsSQL, args, true
}

func dailyUsageRowsSQLForBounds(
	f UsageFilter, b usageBounds, hasCursorTable bool,
) (string, []any) {
	sessionRowsSQL, sessionArgs := usageRowsSQLForBounds(
		usageSnapshotInputFilter(f), b)
	if !hasCursorTable {
		return sessionRowsSQL, sessionArgs
	}
	cursorRowsSQL, cursorArgs, ok := cursorUsageRowsSQLForBounds(f, b)
	if !ok {
		return sessionRowsSQL, sessionArgs
	}
	rowsSQL := sessionRowsSQL + "\n\nUNION ALL\n\n" + cursorRowsSQL
	args := make([]any, 0, len(sessionArgs)+len(cursorArgs))
	args = append(args, sessionArgs...)
	args = append(args, cursorArgs...)
	return rowsSQL, args
}

func exactUsageUTCWindow(f UsageFilter) usageBounds {
	loc := f.location()
	var out usageBounds
	if f.From != "" {
		if from, err := time.ParseInLocation("2006-01-02", f.From, loc); err == nil {
			out.from = from.UTC().Format(time.RFC3339Nano)
		}
	}
	if f.To != "" {
		if to, err := time.ParseInLocation("2006-01-02", f.To, loc); err == nil {
			out.to = to.AddDate(0, 0, 1).UTC().Format(time.RFC3339Nano)
		}
	}
	return out
}

// snapshotRankedDailyUsageRowsSQL wraps rowsSQL so that each Claude request
// (claude_message_id, claude_request_id) contributes one row: the greatest
// output snapshot, attributed to the session that streamed the request
// first, carrying the maximum billed web-search count across its snapshots.
// Rows without complete Claude request identity bypass the ranking.
//
// Only requests that appear more than once are ranked. usage_snapshot_dups
// finds them with an index-only pass over messages, usage_snapshot_ranked
// runs the window functions over just those rows, and every other row
// passes through with itself as attribution and a NULL web-search count that
// the scanner parses from token_usage in Go. Ranking every row through the
// window functions cost two to five times the underlying scan, because
// SQLite sorts and materializes the full-width rows once per window.
//
// The pass-through rows are selected with an IN probe against
// usage_snapshot_dups rather than a LEFT JOIN onto usage_snapshot_ranked.
// SQLite always builds an ephemeral index for an IN subquery, whereas
// joining onto a materialized CTE relies on the planner choosing an
// automatic index; if it scans instead, the join is quadratic in the row
// source. The ranked survivors are appended with UNION ALL.
//
// rowsArgs are the placeholders of rowsSQL; the returned args carry them in
// position with the ranking's own placeholders. Callers finish with
// dailyUsageRowSelectFromSnapshotRowsWithMachine.
func snapshotRankedDailyUsageRowsSQL(
	rowsSQL string, rowsArgs []any, f UsageFilter, b usageBounds,
) (string, []any) {
	windowWhere, windowArgs := usageSnapshotWindowWhere(f)
	dupsSQL, dupsArgs := usageSnapshotDuplicateRequestsSQL(b)
	claudeRowsSQL, claudeArgs := usageSnapshotClaudeMessageRowsSQL(b)
	filterWhere := "1=1"
	var filterArgs []any
	filterWhere, filterArgs = f.appendUsageSourceFilterClauses(
		filterWhere, filterArgs, "survivor.model")
	filterWhere, filterArgs = f.appendUsageSessionFilterClauses(
		filterWhere, filterArgs)
	survivorFilter := ""
	if filterWhere != "1=1" {
		survivorFilter = `
		LEFT JOIN sessions s
			ON s.id = survivor.snapshot_attribution_session_id
		WHERE survivor.snapshot_attribution_session_id = ''
			OR (` + filterWhere + `)`
	}
	outputTokens := fmt.Sprintf(`MIN(MAX(COALESCE(CASE
						WHEN json_valid(u.token_usage) THEN CAST(json_extract(
							u.token_usage, '$.output_tokens') AS INTEGER)
						ELSE agentsview_usage_output_tokens(u.token_usage)
					END, 0), 0), %d)`, MaxPlausibleTokens)
	webSearchRequests := `MAX(COALESCE(CASE
					WHEN json_valid(u.token_usage) THEN CAST(json_extract(
						u.token_usage, '$.server_tool_use.web_search_requests'
					) AS INTEGER)
					ELSE agentsview_usage_web_search_requests(u.token_usage)
				END, 0), 0)`

	args := make([]any, 0,
		len(dupsArgs)+len(claudeArgs)+2*len(windowArgs)+
			len(rowsArgs)+len(filterArgs))
	args = append(args, dupsArgs...)
	args = append(args, claudeArgs...)
	args = append(args, windowArgs...)
	args = append(args, rowsArgs...)
	args = append(args, windowArgs...)
	args = append(args, filterArgs...)
	return fmt.Sprintf(`
		WITH usage_snapshot_dups AS (%[1]s),
		usage_snapshot_ranked AS (
			SELECT u.*,
				FIRST_VALUE(u.session_id) OVER attribution
					AS snapshot_attribution_session_id,
				FIRST_VALUE(u.project) OVER attribution AS snapshot_project,
				FIRST_VALUE(u.agent) OVER attribution AS snapshot_agent,
				FIRST_VALUE(u.machine) OVER attribution AS snapshot_machine,
				MAX(%[5]s) OVER (
					ranking ROWS BETWEEN UNBOUNDED PRECEDING
						AND UNBOUNDED FOLLOWING
				) AS snapshot_web_search_requests,
				ROW_NUMBER() OVER ranking AS snapshot_rank
			FROM (%[2]s) u
			WHERE %[3]s
			WINDOW attribution AS (
				PARTITION BY u.claude_message_id, u.claude_request_id
				ORDER BY julianday(u.ts) IS NULL ASC,
					julianday(u.ts) ASC, u.session_id ASC,
					COALESCE(u.message_ordinal, -1) ASC,
					CASE WHEN julianday(u.ts) IS NULL THEN u.ts ELSE '' END ASC
			), ranking AS (
				PARTITION BY u.claude_message_id, u.claude_request_id
				ORDER BY %[4]s DESC,
					julianday(u.ts) IS NULL ASC, julianday(u.ts) DESC,
					u.session_id DESC, COALESCE(u.message_ordinal, -1) DESC,
					CASE WHEN julianday(u.ts) IS NULL THEN u.ts ELSE '' END DESC
			)
		),
		usage_snapshot_survivors AS (
			SELECT u.*,
				u.session_id AS snapshot_attribution_session_id,
				u.project AS snapshot_project,
				u.agent AS snapshot_agent,
				u.machine AS snapshot_machine,
				NULL AS snapshot_web_search_requests,
				1 AS snapshot_rank
			FROM (%[6]s) u
			WHERE %[3]s
				AND NOT (
					u.usage_source = 'message'
					AND u.claude_message_id != ''
					AND u.claude_request_id != ''
					AND (%[8]s) IN (
						SELECT %[9]s FROM usage_snapshot_dups
					)
				)
			UNION ALL
			SELECT r.*
			FROM usage_snapshot_ranked r
			WHERE r.snapshot_rank = 1
		)
		SELECT survivor.*
		FROM usage_snapshot_survivors survivor%[7]s`,
		dupsSQL, claudeRowsSQL, windowWhere, outputTokens,
		webSearchRequests, rowsSQL, survivorFilter,
		usageSnapshotRequestKey("u."), usageSnapshotRequestKey("")), args
}

// usageSnapshotRequestKey encodes a Claude request identity as one text
// value so the pass-through IN probe hashes a single column: SQLite builds
// a Bloom filter for a single-column IN list but not for a row-value IN,
// which made the probe cost more than the ranking it guards. The
// length prefix keeps the encoding injective for any identifier content.
// prefix qualifies the column references, for example "u.".
func usageSnapshotRequestKey(prefix string) string {
	return "length(CAST(" + prefix + "claude_message_id AS BLOB)) || ':' || " +
		prefix + "claude_message_id || " + prefix + "claude_request_id"
}

// usageSnapshotWindowWhere restricts rows to the filter's exact UTC window
// so snapshots outside the requested dates neither win a partition nor
// reach the scanner. Rows whose timestamp julianday cannot parse fall back
// to a date-prefix comparison, mirroring the scanner's local-date filter.
func usageSnapshotWindowWhere(f UsageFilter) (string, []any) {
	window := exactUsageUTCWindow(f)
	where := "1=1"
	var args []any
	if window.from != "" {
		where += `
			AND (
				julianday(u.ts) >= julianday(?)
				OR (julianday(u.ts) IS NULL AND substr(u.ts, 1, 10) >= ?)
			)`
		args = append(args, window.from, f.From)
	}
	if window.to != "" {
		where += `
			AND (
				julianday(u.ts) < julianday(?)
				OR (julianday(u.ts) IS NULL AND substr(u.ts, 1, 10) <= ?)
			)`
		args = append(args, window.to, f.To)
	}
	return where, args
}

// usageSnapshotClaudeIdentity selects the message rows that carry complete
// Claude request identity and could enter the usage row source.
const usageSnapshotClaudeIdentity = usageMessageSourceEligibility + `
	AND m.claude_message_id != ''
	AND m.claude_request_id != ''`

// usageSnapshotDuplicateRequestsSQL lists the Claude requests that appear on
// more than one eligible message. It over-approximates the ranked set (it
// ignores session eligibility and the fallback session bounds), which only
// sends extra rows through the ranking; the ranking itself applies the exact
// row-source predicates. Bounded filters seek idx_messages_usage_timestamp
// per timestamp branch so the pass stays proportional to the window;
// unbounded filters read idx_messages_claude_snapshot in partition order.
func usageSnapshotDuplicateRequestsSQL(b usageBounds) (string, []any) {
	const key = `m.claude_message_id, m.claude_request_id`
	if !b.bounded() {
		return `
			SELECT ` + key + `
			FROM messages m
			WHERE ` + usageSnapshotClaudeIdentity + `
			GROUP BY ` + key + `
			HAVING COUNT(*) > 1`, nil
	}
	timestampWhere, args := appendUsageColumnBounds(
		usageSnapshotClaudeIdentity, "m.timestamp", b, nil)
	return `
			SELECT claude_message_id, claude_request_id
			FROM (
				SELECT ` + key + `
				FROM messages m
				WHERE ` + timestampWhere + `
				UNION ALL
				SELECT ` + key + `
				FROM messages m
				WHERE ` + usageSnapshotClaudeIdentity + `
					AND m.timestamp IS NULL
				UNION ALL
				SELECT ` + key + `
				FROM messages m
				WHERE ` + usageSnapshotClaudeIdentity + `
					AND m.timestamp = ''
			)
			GROUP BY claude_message_id, claude_request_id
			HAVING COUNT(*) > 1`, args
}

// usageSnapshotClaudeMessageRowsSQL produces the row-source shape for the
// messages of duplicated Claude requests, using the same eligibility and
// bounds as usageRowsSQLForBounds's message branches so the ranked rows are
// exactly the row source's Claude rows for those requests.
func usageSnapshotClaudeMessageRowsSQL(b usageBounds) (string, []any) {
	where := usageMessageEligibility + `
	AND m.claude_message_id != ''
	AND m.claude_request_id != ''
	AND (m.claude_message_id, m.claude_request_id) IN (
		SELECT claude_message_id, claude_request_id FROM usage_snapshot_dups
	)`
	var args []any
	if b.bounded() {
		timestampWhere, timestampArgs := appendUsageColumnBounds(
			"m.timestamp IS NOT NULL AND m.timestamp != ''",
			"m.timestamp", b, nil)
		fallbackWhere, fallbackArgs := appendUsageColumnBounds(
			"NULLIF(m.timestamp, '') IS NULL", "s.started_at", b, nil)
		where += `
	AND ((` + timestampWhere + `) OR (` + fallbackWhere + `))`
		args = append(args, timestampArgs...)
		args = append(args, fallbackArgs...)
	}
	return fmt.Sprintf(dailyUsageMessageRowsSQLTemplate, "messages", where), args
}

func scanUsageRow(rows *sql.Rows) (usageScanRow, error) {
	var r usageScanRow
	err := rows.Scan(
		&r.sessionID,
		&r.messageOrdinal,
		&r.usageSource,
		&r.ts,
		&r.model,
		&r.tokenJSON,
		&r.inputTokens,
		&r.outputTokens,
		&r.cacheCreationInputTokens,
		&r.cacheReadInputTokens,
		&r.reasoningTokens,
		&r.cost,
		&r.costStatus,
		&r.costSource,
		&r.claudeMessageID,
		&r.claudeRequestID,
		&r.sourceUUID,
		&r.usageDedupKey,
		&r.project,
		&r.agent,
		&r.machine,
		&r.userMessageCount,
		&r.isAutomated,
		&r.sessionActivityAt,
		&r.terminationStatus,
		&r.displayName,
		&r.startedAt,
	)
	return r, err
}

func scanDailyUsageRow(rows *sql.Rows) (dailyUsageScanRow, error) {
	return scanDailyUsageRowWithMachine(rows, false)
}

func scanDailyUsageRowWithMachine(
	rows *sql.Rows, includeMachine bool,
) (dailyUsageScanRow, error) {
	var r dailyUsageScanRow
	dest := []any{
		&r.sessionID,
		&r.messageOrdinal,
		&r.usageSource,
		&r.ts,
		&r.model,
		&r.tokenJSON,
		&r.webSearchRequests,
		&r.inputTokens,
		&r.outputTokens,
		&r.cacheCreationInputTokens,
		&r.cacheReadInputTokens,
		&r.reasoningTokens,
		&r.cost,
		&r.costSource,
		&r.claudeMessageID,
		&r.claudeRequestID,
		&r.sourceUUID,
		&r.usageDedupKey,
		&r.project,
		&r.agent,
	}
	if includeMachine {
		dest = append(dest, &r.machine)
	}
	err := rows.Scan(dest...)
	return r, err
}

func parseUsageTokenCounters(
	tokenJSON string,
) (inputTok, outputTok, cacheCrTok, cacheRdTok int) {
	parsed := usagefacts.ParseTokenUsage(tokenJSON)
	return int(parsed.InputTokens), int(parsed.OutputTokens),
		int(parsed.CacheCreationTokens), int(parsed.CacheReadTokens)
}

func parseUsageTokenCountersWithReasoning(
	tokenJSON string,
) (inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok int) {
	parsed := usagefacts.ParseTokenUsage(tokenJSON)
	return int(parsed.InputTokens), int(parsed.OutputTokens),
		int(parsed.CacheCreationTokens), int(parsed.CacheReadTokens),
		int(parsed.ReasoningTokens)
}

func parseUsageWebSearchRequests(tokenJSON string) int {
	return int(usagefacts.ParseTokenUsage(tokenJSON).WebSearchRequests)
}

// usageRowWebSearchRequests returns how many billed Anthropic server-side
// web searches a usage row reports. Only per-message rows carry a usage
// blob; usage events never report server tool use.
func usageRowWebSearchRequests(usageSource, tokenJSON string) int {
	if usageSource != "message" {
		return 0
	}
	return parseUsageWebSearchRequests(tokenJSON)
}

func dailyUsageRowWebSearchRequests(r dailyUsageScanRow) int {
	if r.webSearchRequests.Valid {
		return max(int(r.webSearchRequests.Int64), 0)
	}
	return usageRowWebSearchRequests(r.usageSource, r.tokenJSON)
}

func clampedUsageRowTokens(
	inputTokens, outputTokens, cacheCreationInputTokens,
	cacheReadInputTokens int,
) (inputTok, outputTok, cacheCrTok, cacheRdTok int) {
	return ClampPlausibleTokens(int64(inputTokens)),
		ClampPlausibleTokens(int64(outputTokens)),
		ClampPlausibleTokens(int64(cacheCreationInputTokens)),
		ClampPlausibleTokens(int64(cacheReadInputTokens))
}

func usageEventRowTokens(
	source string,
	inputTokens, outputTokens, cacheCreationInputTokens,
	cacheReadInputTokens int,
) (inputTok, outputTok, cacheCrTok, cacheRdTok int) {
	if source == "session" {
		return floorNegativeTokens(inputTokens),
			floorNegativeTokens(outputTokens),
			floorNegativeTokens(cacheCreationInputTokens),
			floorNegativeTokens(cacheReadInputTokens)
	}
	return clampedUsageRowTokens(
		inputTokens, outputTokens,
		cacheCreationInputTokens, cacheReadInputTokens)
}

func floorNegativeTokens(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

func clampedUsageTokenCounters(
	tokenJSON string,
) (inputTok, outputTok, cacheCrTok, cacheRdTok int) {
	inputTok, outputTok, cacheCrTok, cacheRdTok, _ =
		parseUsageTokenCountersWithReasoning(tokenJSON)
	return ClampPlausibleTokens(int64(inputTok)),
		ClampPlausibleTokens(int64(outputTok)),
		ClampPlausibleTokens(int64(cacheCrTok)),
		ClampPlausibleTokens(int64(cacheRdTok))
}

func clampedUsageTokenCountersWithReasoning(
	tokenJSON string,
) (inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok int) {
	inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok =
		parseUsageTokenCountersWithReasoning(tokenJSON)
	return ClampPlausibleTokens(int64(inputTok)),
		ClampPlausibleTokens(int64(outputTok)),
		ClampPlausibleTokens(int64(cacheCrTok)),
		ClampPlausibleTokens(int64(cacheRdTok)),
		ClampPlausibleTokens(int64(reasoningTok))
}

// usageLookupModel returns the canonical model used to price a usage row.
// Kimi runtime aliases resolve to their fixed or timestamp-selected model;
// all other model names pass through unchanged.
func usageLookupModel(model, ts string) string {
	if canonical := pricingpkg.CanonicalModelForTimestamp(model, ts); canonical != "" {
		return canonical
	}
	return model
}

func dailyUsageAmounts(
	r dailyUsageScanRow, pricing *export.PricingResolver,
) (
	inputTok, outputTok, cacheCrTok, cacheRdTok int,
	cost, savings money.Money,
	err error,
) {
	fact, _ := dailyUsageFact(r)
	if r.webSearchRequests.Valid {
		fact.WebSearchRequests = int64(max(int(r.webSearchRequests.Int64), 0))
	}
	inputTok = int(fact.InputTokens)
	outputTok = int(fact.OutputTokens)
	cacheCrTok = int(fact.CacheCreationTokens)
	cacheRdTok = int(fact.CacheReadTokens)
	priced, err := priceUsageFact(usagePriceInput{
		Fact: fact, Timestamp: r.ts, ReportedModel: r.model,
	}, pricing)
	if err != nil {
		return 0, 0, 0, 0, money.Money{}, money.Money{}, err
	}
	_, lookup := pricing.Resolve(r.model, usageLookupModel(r.model, r.ts))
	if priced.Reported > 0 {
		pricing.RecordResolvedReported(r.model, priced.PricedModel, lookup)
	} else {
		recordComputedUsagePricing(
			pricing, r.model, priced.PricedModel, lookup, fact.RequestScoped,
			inputTok, cacheCrTok, cacheRdTok,
		)
	}
	cost = priced.Cost
	savings = priced.Savings
	return
}

func dailyUsageRowTokens(
	r dailyUsageScanRow,
) (inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok int) {
	fact, _ := dailyUsageFact(r)
	return int(fact.InputTokens), int(fact.OutputTokens),
		int(fact.CacheCreationTokens), int(fact.CacheReadTokens),
		int(fact.ReasoningTokens)
}

func dailyUsageFact(r dailyUsageScanRow) (usagefacts.Fact, bool) {
	if r.usageSource == "message" {
		return usagefacts.FromMessage(usagefacts.MessageInput{
			Ordinal: int(r.messageOrdinal.Int64), Role: "assistant",
			Timestamp: r.ts, Model: r.model, TokenUsage: r.tokenJSON,
			ClaudeMessageID: r.claudeMessageID,
			ClaudeRequestID: r.claudeRequestID,
			SourceUUID:      r.sourceUUID,
		})
	}
	var ordinal *int
	if r.messageOrdinal.Valid {
		value := int(r.messageOrdinal.Int64)
		ordinal = &value
	}
	var reportedCost *int64
	if r.cost.Valid {
		value := r.cost.Int64
		reportedCost = &value
	}
	return usagefacts.FromEvent(usagefacts.EventInput{
		MessageOrdinal: ordinal, Source: r.usageSource,
		Timestamp: r.ts, Model: r.model,
		CostSource: r.costSource, DedupKey: r.usageDedupKey,
		InputTokens:              int64(r.inputTokens),
		OutputTokens:             int64(r.outputTokens),
		ReasoningTokens:          int64(r.reasoningTokens),
		CacheCreationTokens:      int64(r.cacheCreationInputTokens),
		CacheReadTokens:          int64(r.cacheReadInputTokens),
		ReportedCostMicrodollars: reportedCost,
	})
}

func usageRowIsRequestScoped(
	usageSource string, messageOrdinal sql.NullInt64,
) bool {
	return UsageSourceIsRequestScoped(usageSource) || messageOrdinal.Valid
}

// UsageSourceIsRequestScoped reports whether a usage source represents one
// provider request even when the provider cannot attach it to a message.
func UsageSourceIsRequestScoped(source string) bool {
	return source == "message" ||
		source == "goose-request" ||
		source == "deepseek-harness"
}

func recordComputedUsagePricing(
	pricing *export.PricingResolver,
	reportedModel, pricedModel string,
	lookup export.PricingLookup,
	requestScoped bool,
	inputTokens, cacheWriteTokens, cacheReadTokens int,
) {
	if requestScoped {
		pricing.RecordResolvedComputedRequest(
			reportedModel,
			pricedModel,
			lookup,
			inputTokens,
			cacheWriteTokens,
			cacheReadTokens,
		)
		return
	}
	pricing.RecordResolvedComputedAggregate(
		reportedModel, pricedModel, lookup)
}

type usageDedupToken struct {
	kind  string
	value string
}

func usageDedupTokenForRow(
	usageSource, agent, claudeMessageID, claudeRequestID, sourceUUID, usageDedupKey string,
) (usageDedupToken, bool) {
	if claudeMessageID != "" && claudeRequestID != "" {
		return usageDedupToken{
			kind:  "claude",
			value: claudeMessageID + ":" + claudeRequestID,
		}, true
	}
	if usageSource == "message" && agent != "" && sourceUUID != "" {
		return usageDedupToken{
			kind:  "source",
			value: agent + ":" + sourceUUID,
		}, true
	}
	if usageDedupKey != "" {
		return usageDedupToken{
			kind:  "usage",
			value: usageDedupKey,
		}, true
	}
	return usageDedupToken{}, false
}

func (db *DB) loadTopSessionMetadata(
	ctx context.Context, sessionIDs []string,
) (map[string]topSessionMetadata, error) {
	out := make(map[string]topSessionMetadata, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}

	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}

	query := `
SELECT
	id,
	COALESCE(NULLIF(COALESCE(display_name, session_name), ''), NULLIF(first_message, ''), NULLIF(project, ''), id) AS display_name,
	agent,
	project,
	COALESCE(started_at, '') AS started_at
FROM sessions
WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying top session metadata: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var meta topSessionMetadata
		if err := rows.Scan(
			&id,
			&meta.displayName,
			&meta.agent,
			&meta.project,
			&meta.startedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning top session metadata: %w", err)
		}
		out[id] = meta
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating top session metadata: %w", err)
	}
	return out, nil
}

// DailyUsageEntry holds token counts and cost for one day.
type DailyUsageEntry struct {
	Date                string             `json:"date"`
	InputTokens         int                `json:"inputTokens"`
	OutputTokens        int                `json:"outputTokens"`
	CacheCreationTokens int                `json:"cacheCreationTokens"`
	CacheReadTokens     int                `json:"cacheReadTokens"`
	TotalCost           money.Money        `json:"totalCost"`
	ModelsUsed          []string           `json:"modelsUsed"`
	ModelBreakdowns     []ModelBreakdown   `json:"modelBreakdowns"`
	ProjectBreakdowns   []ProjectBreakdown `json:"projectBreakdowns"`
	AgentBreakdowns     []AgentBreakdown   `json:"agentBreakdowns"`
	MachineBreakdowns   []MachineBreakdown `json:"machineBreakdowns"`
}

func (e DailyUsageEntry) MarshalJSON() ([]byte, error) {
	type alias DailyUsageEntry
	out := alias(e)
	if out.ModelsUsed == nil {
		out.ModelsUsed = []string{}
	}
	if out.ModelBreakdowns == nil {
		out.ModelBreakdowns = []ModelBreakdown{}
	}
	if out.ProjectBreakdowns == nil {
		out.ProjectBreakdowns = []ProjectBreakdown{}
	}
	if out.AgentBreakdowns == nil {
		out.AgentBreakdowns = []AgentBreakdown{}
	}
	if out.MachineBreakdowns == nil {
		out.MachineBreakdowns = []MachineBreakdown{}
	}
	return json.Marshal(out)
}

// ModelBreakdown holds per-model token and cost breakdown.
type ModelBreakdown struct {
	ModelName           string      `json:"modelName"`
	InputTokens         int         `json:"inputTokens"`
	OutputTokens        int         `json:"outputTokens"`
	CacheCreationTokens int         `json:"cacheCreationTokens"`
	CacheReadTokens     int         `json:"cacheReadTokens"`
	Cost                money.Money `json:"cost"`
}

// ProjectBreakdown is the per-project slice of a day's usage.
type ProjectBreakdown struct {
	ProjectKey          string      `json:"project_key"`
	Project             string      `json:"project"`
	InputTokens         int         `json:"inputTokens"`
	OutputTokens        int         `json:"outputTokens"`
	CacheCreationTokens int         `json:"cacheCreationTokens"`
	CacheReadTokens     int         `json:"cacheReadTokens"`
	Cost                money.Money `json:"cost"`
}

// AgentBreakdown is the per-agent slice of a day's usage.
type AgentBreakdown struct {
	Agent               string      `json:"agent"`
	InputTokens         int         `json:"inputTokens"`
	OutputTokens        int         `json:"outputTokens"`
	CacheCreationTokens int         `json:"cacheCreationTokens"`
	CacheReadTokens     int         `json:"cacheReadTokens"`
	Cost                money.Money `json:"cost"`
}

// MachineBreakdown is the per-source-machine slice of a day's usage.
type MachineBreakdown struct {
	MachineName         string      `json:"machineName"`
	InputTokens         int         `json:"inputTokens"`
	OutputTokens        int         `json:"outputTokens"`
	CacheCreationTokens int         `json:"cacheCreationTokens"`
	CacheReadTokens     int         `json:"cacheReadTokens"`
	Cost                money.Money `json:"cost"`
}

// UsageTotals holds aggregate token and cost totals.
type UsageTotals struct {
	InputTokens         int         `json:"inputTokens"`
	OutputTokens        int         `json:"outputTokens"`
	CacheCreationTokens int         `json:"cacheCreationTokens"`
	CacheReadTokens     int         `json:"cacheReadTokens"`
	TotalCost           money.Money `json:"totalCost"`
	CopilotAICredits    float64     `json:"copilotAICredits,omitzero"`
	// CacheSavings is the net dollar delta vs an uncached run:
	// cache reads save (input_rate - cache_read_rate) per token,
	// cache creations cost (input_rate - cache_creation_rate)
	// per token (usually negative because creation is billed
	// above the input rate). Computed from per-model rates so
	// mixed-model workloads get the right number, not a fixed
	// Sonnet proxy.
	CacheSavings money.Money `json:"cacheSavings"`
}

// DailyUsageResult wraps the daily entries and totals.
type DailyUsageResult struct {
	SchemaVersion int                               `json:"schema_version,omitempty"`
	Pricing       *export.PricingBlock              `json:"pricing,omitempty"`
	Projects      map[string]export.ProjectMapEntry `json:"projects"`
	Daily         []DailyUsageEntry                 `json:"daily"`
	Totals        UsageTotals                       `json:"totals"`
	SessionCounts UsageSessionCounts                `json:"sessionCounts,omitempty"`
}

func SanitizeDailyUsageProjectLabelsWithCatalog(
	result *DailyUsageResult,
	projects map[string]export.ProjectMapEntry,
) {
	for i := range result.Daily {
		for j := range result.Daily[i].ProjectBreakdowns {
			raw := result.Daily[i].ProjectBreakdowns[j].Project
			result.Daily[i].ProjectBreakdowns[j].ProjectKey =
				export.ProjectKeyForEntry(projects[raw])
			result.Daily[i].ProjectBreakdowns[j].Project =
				export.SafeProjectDisplayLabel(raw)
		}
	}
	if result.SessionCounts.ByProject != nil {
		byProject := make(map[string]int, len(result.SessionCounts.ByProject))
		for raw, count := range result.SessionCounts.ByProject {
			key := export.ProjectKeyForEntry(projects[raw])
			if key != "" {
				byProject[key] += count
			}
		}
		result.SessionCounts.ByProject = byProject
	}
}

// loadPricingMap reads the model_pricing table into a map for
// in-memory joins. This is much faster than a SQL LEFT JOIN
// on every row of the daily usage scan, since the pricing
// table is tiny and repeated resolver lookups are cached.
func (db *DB) loadPricingMap(
	ctx context.Context,
) ([]export.EffectivePricingRow, error) {
	return db.loadPricingMapFrom(ctx, db.getReader())
}

func (db *DB) loadPricingMapFrom(
	ctx context.Context, q sessionExportQuerier,
) ([]export.EffectivePricingRow, error) {
	prices, err := listModelPricingFrom(ctx, q)
	if err != nil {
		return nil, err
	}

	fallback := fallbackRateMap()
	out := make(map[string]export.ModelRates)
	for _, p := range prices {
		if strings.HasPrefix(p.ModelPattern, "_") {
			continue
		}
		rates := modelPricingRates(p)
		rates.Source = modelPricingSource(p, fallback)
		out[p.ModelPattern] = rates
	}

	if len(out) == 0 {
		for model, rates := range db.emptyCatalogPricing {
			rates.Bands = append([]export.PricingBand(nil), rates.Bands...)
			out[model] = rates
		}
	}
	for model, cp := range db.customPricing {
		rates := export.ModelRates{
			InputPerMTok: money.Money{
				Microdollars: cp.InputMicrodollarsPerMTok,
			},
			OutputPerMTok: money.Money{
				Microdollars: cp.OutputMicrodollarsPerMTok,
			},
			CacheWritePerMTok: money.Money{
				Microdollars: cp.CacheCreationMicrodollarsPerMTok,
			},
			CacheReadPerMTok: money.Money{
				Microdollars: cp.CacheReadMicrodollarsPerMTok,
			},
		}
		rates.Source = customPricingSource()
		out[model] = rates
	}
	for model, rates := range db.effectivePricing {
		rates.Bands = append([]export.PricingBand(nil), rates.Bands...)
		out[model] = rates
	}

	return pricingMapRows(out), nil
}

func customPricingSource() export.PricingRowSource {
	return export.PricingRowSourceCustom
}

func fallbackRateMap() map[string]export.ModelRates {
	fallback := pricingpkg.FallbackPricing()
	out := make(map[string]export.ModelRates, len(fallback))
	for _, p := range fallback {
		rates := export.ModelRates{
			InputPerMTok:      p.InputPerMTok,
			OutputPerMTok:     p.OutputPerMTok,
			CacheWritePerMTok: p.CacheCreationPerMTok,
			CacheReadPerMTok:  p.CacheReadPerMTok,
			Source:            export.PricingRowSourceEmbedded,
			Bands:             catalogPricingBands(p.Bands),
		}
		out[p.ModelPattern] = rates
	}
	return out
}

func modelPricingRates(p ModelPricing) export.ModelRates {
	var updatedAt *time.Time
	if p.UpdatedAt != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, p.UpdatedAt); err == nil {
			t := parsed.UTC()
			updatedAt = &t
		}
	}
	return export.ModelRates{
		InputPerMTok:      p.InputPerMTok,
		OutputPerMTok:     p.OutputPerMTok,
		CacheWritePerMTok: p.CacheCreationPerMTok,
		CacheReadPerMTok:  p.CacheReadPerMTok,
		UpdatedAt:         updatedAt,
		Bands:             storedPricingBands(p.Bands),
	}
}

func catalogPricingBands(bands []pricingpkg.PricingBand) []export.PricingBand {
	out := make([]export.PricingBand, len(bands))
	for i, band := range bands {
		out[i] = export.PricingBand{
			AboveInputTokens:  band.AboveInputTokens,
			InputPerMTok:      band.InputPerMTok,
			OutputPerMTok:     band.OutputPerMTok,
			CacheWritePerMTok: band.CacheCreationPerMTok,
			CacheReadPerMTok:  band.CacheReadPerMTok,
		}
	}
	return out
}

func storedPricingBands(bands []PricingBand) []export.PricingBand {
	out := make([]export.PricingBand, len(bands))
	for i, band := range bands {
		var updatedAt *time.Time
		if parsed, err := time.Parse(time.RFC3339Nano, band.UpdatedAt); err == nil {
			t := parsed.UTC()
			updatedAt = &t
		}
		out[i] = export.PricingBand{
			AboveInputTokens:  band.AboveInputTokens,
			InputPerMTok:      band.InputPerMTok,
			OutputPerMTok:     band.OutputPerMTok,
			CacheWritePerMTok: band.CacheCreationPerMTok,
			CacheReadPerMTok:  band.CacheReadPerMTok,
			UpdatedAt:         updatedAt,
		}
	}
	return out
}

func modelPricingSource(
	p ModelPricing, fallback map[string]export.ModelRates,
) export.PricingRowSource {
	if rates, ok := fallback[p.ModelPattern]; ok &&
		rates.InputPerMTok == p.InputPerMTok &&
		rates.OutputPerMTok == p.OutputPerMTok &&
		rates.CacheWritePerMTok == p.CacheCreationPerMTok &&
		rates.CacheReadPerMTok == p.CacheReadPerMTok &&
		exportPricingBandsEqual(rates.Bands, storedPricingBands(p.Bands)) {
		return export.PricingRowSourceEmbedded
	}
	return export.PricingRowSourceFetched
}

func exportPricingBandsEqual(a, b []export.PricingBand) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].AboveInputTokens != b[i].AboveInputTokens ||
			a[i].InputPerMTok != b[i].InputPerMTok ||
			a[i].OutputPerMTok != b[i].OutputPerMTok ||
			a[i].CacheWritePerMTok != b[i].CacheWritePerMTok ||
			a[i].CacheReadPerMTok != b[i].CacheReadPerMTok {
			return false
		}
	}
	return true
}

func pricingMapRows(
	in map[string]export.ModelRates,
) []export.EffectivePricingRow {
	out := make([]export.EffectivePricingRow, 0, len(in))
	for pattern, rates := range in {
		out = append(out, export.EffectivePricingRow{
			ModelPattern: pattern,
			Rates:        rates,
		})
	}
	return out
}

// paddedUTCBound pads a UTC timestamp by hours to cover timezone
// offsets. Positive hours pad forward, negative pad backward.
func paddedUTCBound(ts string, hours int) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Add(time.Duration(hours) * time.Hour).Format(time.RFC3339)
}

// getDailyUsageLegacy is the wide-row test oracle for the facts-backed path.
// Production callers enter through GetDailyUsage in usage_cache_daily.go.
func (db *DB) getDailyUsageLegacy(
	ctx context.Context, f UsageFilter,
) (DailyUsageResult, error) {
	loc := f.location()

	pricing, err := db.loadPricingMap(ctx)
	if err != nil {
		return DailyUsageResult{},
			fmt.Errorf("loading pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)

	// Filter on usage timestamp (not only session started_at) so
	// long-lived sessions that span date boundaries are included.
	// Pad by +/-14h to cover all timezone offsets; the actual
	// date filtering happens post-query via localDate.
	bounds := usageBoundsForFilter(f)
	query, rowsArgs := dailyUsageRowsSQLForBounds(
		f, bounds, db.hasCursorUsageTable())
	query, args := snapshotRankedDailyUsageRowsSQL(query, rowsArgs, f, bounds)
	query = dailyUsageRowSelectFromSnapshotRowsWithMachine(
		query, f.Breakdowns)
	query += ` ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return DailyUsageResult{},
			fmt.Errorf("querying daily usage: %w", err)
	}
	defer rows.Close()

	type bucket struct {
		inputTok  int
		outputTok int
		cacheCr   int
		cacheRd   int
		cost      money.Money
	}
	type sessionCost struct {
		estimated     map[usageCostAllocationKey]money.Money
		authoritative *money.Money
	}

	accum := make(map[usageCostAllocationKey]*bucket)
	sessionCosts := make(map[string]sessionCost)
	useAuthoritativeCost := f.Model == "" && f.ExcludeModel == ""

	seen := make(map[usageDedupToken]struct{})
	var seenSessions map[string]UsageSessionInfo
	if !f.SkipSessionCounts {
		seenSessions = make(map[string]UsageSessionInfo)
	}
	projectLabels := make(map[string]struct{})

	// totalSavings is the running sum of per-message cache
	// savings using each row's actual per-model rates. We sum
	// at the message level instead of deriving from totals
	// later because the rate mix varies per workload and a
	// single fallback rate would misreport mixed-model periods.
	var totalSavings money.Money

	for rows.Next() {
		r, scanErr := scanDailyUsageRowWithMachine(rows, f.Breakdowns)
		if scanErr != nil {
			return DailyUsageResult{},
				fmt.Errorf("scanning daily usage row: %w", scanErr)
		}

		date := localDate(r.ts, loc)
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}
		// Dedup AFTER the date filter so out-of-range rows
		// (pulled in by the ±14h timezone padding) don't mark
		// a key as seen and suppress the in-range duplicate.
		if key, ok := usageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		if seenSessions != nil && r.usageSource != "cursor" {
			if _, ok := seenSessions[r.sessionID]; !ok {
				seenSessions[r.sessionID] = UsageSessionInfo{
					Project: r.project,
					Agent:   r.agent,
				}
			}
		}
		if r.project != "" {
			projectLabels[r.project] = struct{}{}
		}

		inputTok, outputTok, cacheCrTok, cacheRdTok, cost, savings, priceErr :=
			dailyUsageAmounts(r, rateResolver)
		if priceErr != nil {
			return DailyUsageResult{}, priceErr
		}
		totalSavings, priceErr = money.Add(totalSavings, savings)
		if priceErr != nil {
			return DailyUsageResult{}, fmt.Errorf(
				"summing daily usage cache savings: %w", priceErr)
		}

		key := usageCostAllocationKey{
			date: date, project: r.project,
			agent: r.agent, machine: r.machine, model: r.model,
		}
		b, ok := accum[key]
		if !ok {
			b = &bucket{}
			accum[key] = b
		}
		b.inputTok += inputTok
		b.outputTok += outputTok
		b.cacheCr += cacheCrTok
		b.cacheRd += cacheRdTok

		sc := sessionCosts[r.sessionID]
		if sc.estimated == nil {
			sc.estimated = make(map[usageCostAllocationKey]money.Money)
		}
		sc.estimated[key], priceErr = money.Add(sc.estimated[key], cost)
		if priceErr != nil {
			return DailyUsageResult{}, fmt.Errorf(
				"summing daily usage session cost: %w", priceErr)
		}
		if useAuthoritativeCost &&
			r.costSource == CopilotReportedCostSource && r.cost.Valid {
			v := money.Money{Microdollars: r.cost.Int64}
			sc.authoritative = &v
			rateResolver.RecordUnattributedReported()
		}
		sessionCosts[r.sessionID] = sc
	}
	if err := rows.Err(); err != nil {
		return DailyUsageResult{},
			fmt.Errorf("iterating daily usage rows: %w", err)
	}
	sessionIDs := make([]string, 0, len(sessionCosts))
	for sessionID := range sessionCosts {
		sessionIDs = append(sessionIDs, sessionID)
	}
	sort.Strings(sessionIDs)
	for _, sessionID := range sessionIDs {
		sc := sessionCosts[sessionID]
		if sc.authoritative != nil {
			costs := allocateUsageCostByKey(*sc.authoritative, sc.estimated)
			for key, cost := range costs {
				b := accum[key]
				if b == nil {
					b = &bucket{}
					accum[key] = b
				}
				b.cost, err = money.Add(b.cost, cost)
				if err != nil {
					return DailyUsageResult{}, fmt.Errorf(
						"summing allocated daily usage cost: %w", err)
				}
			}
		} else {
			for key, cost := range sc.estimated {
				b := accum[key]
				if b == nil {
					b = &bucket{}
					accum[key] = b
				}
				b.cost, err = money.Add(b.cost, cost)
				if err != nil {
					return DailyUsageResult{}, fmt.Errorf(
						"summing estimated daily usage cost: %w", err)
				}
			}
		}
	}

	// Two paths: without breakdowns (CLI, fast) and with breakdowns
	// (web UI). The fast path uses the original (date, model)
	// grouping with no extra column reads. The breakdown path adds
	// project/agent dimensions and builds three decomposition slices.

	if !f.Breakdowns {
		// Fast path: group by (date, model) only.
		type dateModelKey struct {
			date  string
			model string
		}
		type modelAccum struct {
			inputTok  int
			outputTok int
			cacheCr   int
			cacheRd   int
			cost      money.Money
		}
		dm := make(map[dateModelKey]*modelAccum)
		for key, b := range accum {
			dmk := dateModelKey{date: key.date, model: key.model}
			ma, ok := dm[dmk]
			if !ok {
				ma = &modelAccum{}
				dm[dmk] = ma
			}
			ma.inputTok += b.inputTok
			ma.outputTok += b.outputTok
			ma.cacheCr += b.cacheCr
			ma.cacheRd += b.cacheRd
			ma.cost, err = money.Add(ma.cost, b.cost)
			if err != nil {
				return DailyUsageResult{}, fmt.Errorf(
					"summing daily model cost: %w", err)
			}
		}

		type dayData struct {
			models map[string]*modelAccum
		}
		days := make(map[string]*dayData)
		for key, ma := range dm {
			dd, ok := days[key.date]
			if !ok {
				dd = &dayData{
					models: make(map[string]*modelAccum),
				}
				days[key.date] = dd
			}
			dd.models[key.model] = ma
		}

		dateKeys := make([]string, 0, len(days))
		for d := range days {
			dateKeys = append(dateKeys, d)
		}
		sort.Strings(dateKeys)

		daily := make([]DailyUsageEntry, 0, len(dateKeys))
		var totals UsageTotals

		for _, date := range dateKeys {
			dd, ok := days[date]
			if !ok || dd == nil {
				continue
			}
			var entry DailyUsageEntry
			entry.Date = date

			modelNames := make([]string, 0, len(dd.models))
			for m := range dd.models {
				modelNames = append(modelNames, m)
			}
			sort.Slice(modelNames, func(i, j int) bool {
				left := dd.models[modelNames[i]]
				right := dd.models[modelNames[j]]
				if left == nil || right == nil {
					return left != nil
				}
				ci := left.cost
				cj := right.cost
				if ci.Microdollars != cj.Microdollars {
					return ci.Microdollars > cj.Microdollars
				}
				return modelNames[i] < modelNames[j]
			})
			entry.ModelsUsed = modelNames
			mbd := make(
				[]ModelBreakdown, 0, len(modelNames),
			)
			for _, m := range modelNames {
				ma, ok := dd.models[m]
				if !ok || ma == nil {
					continue
				}
				entry.InputTokens += ma.inputTok
				entry.OutputTokens += ma.outputTok
				entry.CacheCreationTokens += ma.cacheCr
				entry.CacheReadTokens += ma.cacheRd
				entry.TotalCost, err = money.Add(entry.TotalCost, ma.cost)
				if err != nil {
					return DailyUsageResult{}, fmt.Errorf(
						"summing daily entry cost: %w", err)
				}
				mbd = append(mbd, ModelBreakdown{
					ModelName:           m,
					InputTokens:         ma.inputTok,
					OutputTokens:        ma.outputTok,
					CacheCreationTokens: ma.cacheCr,
					CacheReadTokens:     ma.cacheRd,
					Cost:                ma.cost,
				})
			}
			entry.ModelBreakdowns = mbd
			daily = append(daily, entry)

			totals.InputTokens += entry.InputTokens
			totals.OutputTokens += entry.OutputTokens
			totals.CacheCreationTokens += entry.CacheCreationTokens
			totals.CacheReadTokens += entry.CacheReadTokens
			totals.TotalCost, err = money.Add(totals.TotalCost, entry.TotalCost)
			if err != nil {
				return DailyUsageResult{}, fmt.Errorf(
					"summing daily usage total: %w", err)
			}
		}

		if daily == nil {
			daily = []DailyUsageEntry{}
		}
		totals.CacheSavings = totalSavings

		var aiCredits float64
		for key, b := range accum {
			aiCredits += AICreditsFromCost(key.agent, b.cost)
		}
		if aiCredits > 0 {
			totals.CopilotAICredits = aiCredits
		}
		var sessionCounts UsageSessionCounts
		if seenSessions != nil {
			sessionCounts = NewUsageSessionCounts(seenSessions)
		}
		projects, err := db.BuildProjectIdentityMap(ctx,
			sortedSetKeys(projectLabels))
		if err != nil {
			return DailyUsageResult{}, err
		}
		projectRows := DailyUsageResult{Daily: daily, SessionCounts: sessionCounts}
		SanitizeDailyUsageProjectLabelsWithCatalog(&projectRows, projects)
		daily = projectRows.Daily
		sessionCounts = projectRows.SessionCounts
		pricingBlock, err := rateResolver.BuildBlock()
		if err != nil {
			return DailyUsageResult{}, fmt.Errorf(
				"building pricing block: %w", err)
		}
		return DailyUsageResult{
			SchemaVersion: export.UsageDailySchemaVersion,
			Pricing:       &pricingBlock,
			Projects:      export.ProjectMapForWire(projects),
			Daily:         daily,
			Totals:        totals,
			SessionCounts: sessionCounts,
		}, nil
	}

	// Breakdown path: single walk builds model/project/agent maps.
	type dayMaps struct {
		models   map[string]bucket
		projects map[string]bucket
		agents   map[string]bucket
		machines map[string]bucket
	}
	days := make(map[string]*dayMaps, 64)
	for key, b := range accum {
		dm, ok := days[key.date]
		if !ok {
			dm = &dayMaps{
				models:   make(map[string]bucket, 4),
				projects: make(map[string]bucket, 8),
				agents:   make(map[string]bucket, 4),
				machines: make(map[string]bucket, 4),
			}
			days[key.date] = dm
		}
		cur := dm.models[key.model]
		cur.inputTok += b.inputTok
		cur.outputTok += b.outputTok
		cur.cacheCr += b.cacheCr
		cur.cacheRd += b.cacheRd
		cur.cost, err = money.Add(cur.cost, b.cost)
		if err != nil {
			return DailyUsageResult{}, fmt.Errorf(
				"summing daily model breakdown cost: %w", err)
		}
		dm.models[key.model] = cur

		cur = dm.projects[key.project]
		cur.inputTok += b.inputTok
		cur.outputTok += b.outputTok
		cur.cacheCr += b.cacheCr
		cur.cacheRd += b.cacheRd
		cur.cost, err = money.Add(cur.cost, b.cost)
		if err != nil {
			return DailyUsageResult{}, fmt.Errorf(
				"summing daily project breakdown cost: %w", err)
		}
		dm.projects[key.project] = cur

		cur = dm.agents[key.agent]
		cur.inputTok += b.inputTok
		cur.outputTok += b.outputTok
		cur.cacheCr += b.cacheCr
		cur.cacheRd += b.cacheRd
		cur.cost, err = money.Add(cur.cost, b.cost)
		if err != nil {
			return DailyUsageResult{}, fmt.Errorf(
				"summing daily agent breakdown cost: %w", err)
		}
		dm.agents[key.agent] = cur

		cur = dm.machines[key.machine]
		cur.inputTok += b.inputTok
		cur.outputTok += b.outputTok
		cur.cacheCr += b.cacheCr
		cur.cacheRd += b.cacheRd
		cur.cost, err = money.Add(cur.cost, b.cost)
		if err != nil {
			return DailyUsageResult{}, fmt.Errorf(
				"summing daily machine breakdown cost: %w", err)
		}
		dm.machines[key.machine] = cur
	}

	dateKeys := make([]string, 0, len(days))
	for d := range days {
		dateKeys = append(dateKeys, d)
	}
	sort.Strings(dateKeys)

	daily := make([]DailyUsageEntry, 0, len(dateKeys))
	var totals UsageTotals

	for _, date := range dateKeys {
		dm, ok := days[date]
		if !ok || dm == nil {
			continue
		}
		var entry DailyUsageEntry
		entry.Date = date

		modelNames := make([]string, 0, len(dm.models))
		for m := range dm.models {
			modelNames = append(modelNames, m)
		}
		sort.Slice(modelNames, func(i, j int) bool {
			left := dm.models[modelNames[i]]
			right := dm.models[modelNames[j]]
			ci := left.cost
			cj := right.cost
			if ci.Microdollars != cj.Microdollars {
				return ci.Microdollars > cj.Microdollars
			}
			return modelNames[i] < modelNames[j]
		})
		entry.ModelsUsed = modelNames
		mbd := make(
			[]ModelBreakdown, 0, len(modelNames),
		)
		for _, m := range modelNames {
			b, ok := dm.models[m]
			if !ok {
				continue
			}
			entry.InputTokens += b.inputTok
			entry.OutputTokens += b.outputTok
			entry.CacheCreationTokens += b.cacheCr
			entry.CacheReadTokens += b.cacheRd
			entry.TotalCost, err = money.Add(entry.TotalCost, b.cost)
			if err != nil {
				return DailyUsageResult{}, fmt.Errorf(
					"summing daily breakdown entry cost: %w", err)
			}
			mbd = append(mbd, ModelBreakdown{
				ModelName:           m,
				InputTokens:         b.inputTok,
				OutputTokens:        b.outputTok,
				CacheCreationTokens: b.cacheCr,
				CacheReadTokens:     b.cacheRd,
				Cost:                b.cost,
			})
		}
		entry.ModelBreakdowns = mbd

		pbd := make(
			[]ProjectBreakdown, 0, len(dm.projects),
		)
		for p, b := range dm.projects {
			pbd = append(pbd, ProjectBreakdown{
				Project:             p,
				InputTokens:         b.inputTok,
				OutputTokens:        b.outputTok,
				CacheCreationTokens: b.cacheCr,
				CacheReadTokens:     b.cacheRd,
				Cost:                b.cost,
			})
		}
		sort.Slice(pbd, func(i, j int) bool {
			if pbd[i].Cost.Microdollars != pbd[j].Cost.Microdollars {
				return pbd[i].Cost.Microdollars > pbd[j].Cost.Microdollars
			}
			return pbd[i].Project < pbd[j].Project
		})
		entry.ProjectBreakdowns = pbd

		abd := make(
			[]AgentBreakdown, 0, len(dm.agents),
		)
		for a, b := range dm.agents {
			abd = append(abd, AgentBreakdown{
				Agent:               a,
				InputTokens:         b.inputTok,
				OutputTokens:        b.outputTok,
				CacheCreationTokens: b.cacheCr,
				CacheReadTokens:     b.cacheRd,
				Cost:                b.cost,
			})
		}
		sort.Slice(abd, func(i, j int) bool {
			if abd[i].Cost.Microdollars != abd[j].Cost.Microdollars {
				return abd[i].Cost.Microdollars > abd[j].Cost.Microdollars
			}
			return abd[i].Agent < abd[j].Agent
		})
		entry.AgentBreakdowns = abd

		machineBreakdowns := make(
			[]MachineBreakdown, 0, len(dm.machines),
		)
		for machine, b := range dm.machines {
			machineBreakdowns = append(machineBreakdowns, MachineBreakdown{
				MachineName:         machine,
				InputTokens:         b.inputTok,
				OutputTokens:        b.outputTok,
				CacheCreationTokens: b.cacheCr,
				CacheReadTokens:     b.cacheRd,
				Cost:                b.cost,
			})
		}
		sort.Slice(machineBreakdowns, func(i, j int) bool {
			if machineBreakdowns[i].Cost.Microdollars != machineBreakdowns[j].Cost.Microdollars {
				return machineBreakdowns[i].Cost.Microdollars > machineBreakdowns[j].Cost.Microdollars
			}
			return machineBreakdowns[i].MachineName < machineBreakdowns[j].MachineName
		})
		entry.MachineBreakdowns = machineBreakdowns

		daily = append(daily, entry)

		totals.InputTokens += entry.InputTokens
		totals.OutputTokens += entry.OutputTokens
		totals.CacheCreationTokens += entry.CacheCreationTokens
		totals.CacheReadTokens += entry.CacheReadTokens
		totals.TotalCost, err = money.Add(totals.TotalCost, entry.TotalCost)
		if err != nil {
			return DailyUsageResult{}, fmt.Errorf(
				"summing daily breakdown total: %w", err)
		}
	}

	if daily == nil {
		daily = []DailyUsageEntry{}
	}

	totals.CacheSavings = totalSavings

	var aiCredits float64
	for _, d := range daily {
		for _, ab := range d.AgentBreakdowns {
			aiCredits += AICreditsFromCost(ab.Agent, ab.Cost)
		}
	}
	if aiCredits > 0 {
		totals.CopilotAICredits = aiCredits
	}

	var sessionCounts UsageSessionCounts
	if seenSessions != nil {
		sessionCounts = NewUsageSessionCounts(seenSessions)
	}
	projects, err := db.BuildProjectIdentityMap(ctx, sortedSetKeys(projectLabels))
	if err != nil {
		return DailyUsageResult{}, err
	}
	projectRows := DailyUsageResult{Daily: daily, SessionCounts: sessionCounts}
	SanitizeDailyUsageProjectLabelsWithCatalog(&projectRows, projects)
	daily = projectRows.Daily
	sessionCounts = projectRows.SessionCounts
	pricingBlock, err := rateResolver.BuildBlock()
	if err != nil {
		return DailyUsageResult{}, fmt.Errorf(
			"building pricing block: %w", err)
	}
	return DailyUsageResult{
		SchemaVersion: export.UsageDailySchemaVersion,
		Pricing:       &pricingBlock,
		Projects:      export.ProjectMapForWire(projects),
		Daily:         daily,
		Totals:        totals,
		SessionCounts: sessionCounts,
	}, nil
}

// TopSessionEntry is one row in the "top sessions by cost" result.
type TopSessionEntry struct {
	SessionID           string      `json:"sessionId"`
	DisplayName         string      `json:"displayName"`
	Agent               string      `json:"agent"`
	Project             string      `json:"project"`
	StartedAt           string      `json:"startedAt"`
	InputTokens         int         `json:"inputTokens"`
	OutputTokens        int         `json:"outputTokens"`
	CacheCreationTokens int         `json:"cacheCreationTokens"`
	CacheReadTokens     int         `json:"cacheReadTokens"`
	TotalTokens         int         `json:"totalTokens"`
	Cost                money.Money `json:"cost"`
}

// TopSessionsSortCost and TopSessionsSortTokens select top-session ranking.
const (
	TopSessionsSortCost   = "cost"
	TopSessionsSortTokens = "tokens"
)

// SortAndLimitTopSessions ranks entries by cost (default) or tokens, then
// applies the bounded limit. Ties break by session ID for stable results.
func SortAndLimitTopSessions(
	result []TopSessionEntry, limit int, sortBy string,
	tokenTypes UsageTokenTypes,
) []TopSessionEntry {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	byTokens := strings.EqualFold(sortBy, TopSessionsSortTokens)
	sort.Slice(result, func(i, j int) bool {
		if byTokens {
			left := tokenTypes.Total(
				result[i].InputTokens,
				result[i].OutputTokens,
				result[i].CacheCreationTokens,
				result[i].CacheReadTokens,
			)
			right := tokenTypes.Total(
				result[j].InputTokens,
				result[j].OutputTokens,
				result[j].CacheCreationTokens,
				result[j].CacheReadTokens,
			)
			if left != right {
				return left > right
			}
		} else if result[i].Cost.Microdollars != result[j].Cost.Microdollars {
			return result[i].Cost.Microdollars > result[j].Cost.Microdollars
		}
		return result[i].SessionID < result[j].SessionID
	})
	if len(result) > limit {
		return result[:limit]
	}
	return result
}

// getTopSessionsByCostLegacy is the wide-row test oracle for the facts path.
func (db *DB) getTopSessionsByCostLegacy(
	ctx context.Context, f UsageFilter, limit int,
) ([]TopSessionEntry, error) {
	pricing, err := db.loadPricingMap(ctx)
	if err != nil {
		return nil,
			fmt.Errorf("loading pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)

	query, args := topSessionsUsageRowQuery(f)
	// Deterministic order so the dedup "winner" (the session
	// that gets credit for a duplicate message.id + request.id
	// pair) is stable across runs: earliest timestamp wins,
	// then session_id, then message ordinal.
	query += ` ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return nil,
			fmt.Errorf("querying top sessions: %w", err)
	}
	defer rows.Close()

	loc := f.location()

	type sessAccum struct {
		inputTokens       int
		outputTokens      int
		cacheCreateTokens int
		cacheReadTokens   int
		totalTokens       int
		cost              money.Money
		authoritativeCost *money.Money
	}

	accum := make(map[string]*sessAccum)
	// Track insertion order for stable iteration.
	var order []string

	// Dedup duplicate usage rows across fork/subagent
	// boundaries so per-session totals match the aggregate
	// totals from GetDailyUsage. Same key and ordering rules.
	seen := make(map[usageDedupToken]struct{})

	for rows.Next() {
		r, err := scanDailyUsageRow(rows)
		if err != nil {
			return nil,
				fmt.Errorf("scanning top sessions row: %w", err)
		}

		// Post-query date filter (same as GetDailyUsage).
		date := localDate(r.ts, loc)
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}
		// Dedup AFTER the date filter, matching GetDailyUsage,
		// so out-of-range rows pulled in by the ±14h padding
		// don't claim a key and suppress the in-range duplicate.
		if key, ok := usageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		inputTok, outputTok, cacheCrTok, cacheRdTok, cost, _, priceErr :=
			dailyUsageAmounts(r, rateResolver)
		if priceErr != nil {
			return nil, priceErr
		}

		sa, ok := accum[r.sessionID]
		if !ok {
			sa = &sessAccum{}
			accum[r.sessionID] = sa
			order = append(order, r.sessionID)
		}
		sa.inputTokens += inputTok
		sa.outputTokens += outputTok
		sa.cacheCreateTokens += cacheCrTok
		sa.cacheReadTokens += cacheRdTok
		sa.totalTokens += inputTok + outputTok + cacheCrTok + cacheRdTok
		sa.cost, priceErr = money.Add(sa.cost, cost)
		if priceErr != nil {
			return nil, fmt.Errorf("summing top-session cost: %w", priceErr)
		}
		if f.Model == "" && f.ExcludeModel == "" &&
			r.costSource == CopilotReportedCostSource && r.cost.Valid {
			v := money.Money{Microdollars: r.cost.Int64}
			sa.authoritativeCost = &v
		}
	}
	if err := rows.Err(); err != nil {
		return nil,
			fmt.Errorf("iterating top sessions rows: %w", err)
	}
	result := make([]TopSessionEntry, 0, len(order))
	for _, id := range order {
		sa, ok := accum[id]
		if !ok || sa == nil {
			continue
		}
		result = append(result, TopSessionEntry{
			SessionID:           id,
			DisplayName:         id,
			InputTokens:         sa.inputTokens,
			OutputTokens:        sa.outputTokens,
			CacheCreationTokens: sa.cacheCreateTokens,
			CacheReadTokens:     sa.cacheReadTokens,
			TotalTokens:         sa.totalTokens,
			Cost: func() money.Money {
				if sa.authoritativeCost != nil {
					return *sa.authoritativeCost
				}
				return sa.cost
			}(),
		})
	}

	result = SortAndLimitTopSessions(
		result, limit, f.TopSessionsSort, f.TopSessionsTokenTypes,
	)

	sessionIDs := make([]string, len(result))
	for i := range result {
		sessionIDs[i] = result[i].SessionID
	}
	metadata, err := db.loadTopSessionMetadata(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}
	for i := range result {
		if meta, ok := metadata[result[i].SessionID]; ok {
			result[i].DisplayName = meta.displayName
			result[i].Agent = meta.agent
			result[i].Project = meta.project
			result[i].StartedAt = meta.startedAt
		}
	}

	return result, nil
}

// SessionUsage is the per-session token + cost summary returned by
// the `session usage` command. Cost is an estimate from the
// model_pricing catalog unless an agent reported cost directly
// (usage_events.cost_microdollars). Cost is non-zero only when HasCost is
// true; a partial total (some models unpriced) is never emitted.
type SessionUsage struct {
	SessionID         string      `json:"session_id"`
	Agent             string      `json:"agent"`
	Project           string      `json:"project"`
	TotalOutputTokens int         `json:"total_output_tokens"`
	PeakContextTokens int         `json:"peak_context_tokens"`
	HasTokenData      bool        `json:"has_token_data"`
	Cost              money.Money `json:"cost"`
	HasCost           bool        `json:"has_cost"`
	// CostUSD is a deprecated compatibility alias for
	// Cost.Microdollars/1e6, kept during a deprecation window for
	// consumers (such as roborev) still reading the pre-v0.39
	// cost_usd field. It is present exactly when HasCost is true and
	// always equals Cost expressed in dollars; it will be removed in
	// a future release. New consumers should read cost.microdollars.
	CostUSD        *float64          `json:"cost_usd,omitempty"`
	CostSource     export.CostSource `json:"cost_source,omitempty"`
	AICredits      float64           `json:"ai_credits,omitzero"`
	Models         []string          `json:"models"`
	UnpricedModels []string          `json:"unpriced_models,omitempty"`
	BreakdownCount int               `json:"breakdown_count"`
	// SubagentCount is how many subagent descendant sessions were folded
	// into this result. It is zero (and omitted) for own-session results,
	// which is what GetSessionUsage always returns; only the
	// presentation-time combining in internal/service sets it.
	SubagentCount int                          `json:"subagent_count,omitzero"`
	Breakdown     []SessionUsageBreakdownEntry `json:"breakdown"`
}

type SessionUsageBreakdownEntry struct {
	Ordinal        int    `json:"ordinal"`
	MessageOrdinal *int   `json:"message_ordinal,omitempty"`
	Source         string `json:"source"`
	Label          string `json:"label"`
	Timestamp      string `json:"timestamp"`
	Model          string `json:"model"`
	// SubagentSessionID names the subagent descendant session this row
	// came from. Empty (and omitted) for the queried session's own rows,
	// so `source` keeps its existing meaning.
	SubagentSessionID        string `json:"subagent_session_id,omitempty"`
	InputTokens              int    `json:"input_tokens"`
	OutputTokens             int    `json:"output_tokens"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens"`
	// WebSearchRequests is how many Anthropic server-side web searches
	// this row was billed for, at export.WebSearchRequestMicrodollars
	// each. Omitted when the row performed none, which is every row for
	// every provider that does not report server tool use.
	WebSearchRequests int         `json:"web_search_requests,omitzero"`
	Cost              money.Money `json:"cost"`
	HasCost           bool        `json:"has_cost"`
}

// SessionUsageBreakdownLabel renders a breakdown row's human label. It is
// shared so every backend (and the presentation-time subagent combiner)
// produces the same label for the same row.
func SessionUsageBreakdownLabel(messageOrdinal *int, source string) string {
	if messageOrdinal != nil {
		if source == "message" {
			return fmt.Sprintf("Prompt %d", *messageOrdinal+1)
		}
		return fmt.Sprintf("Step %d", *messageOrdinal+1)
	}
	if source != "" {
		return source
	}
	return "usage"
}

// sessionRowCost computes one usage row's cost and reports whether
// it was priced and whether it contributes to the estimate. A row
// contributes when it carries an explicit cost, any tokens, or any billed
// web search requests. It does an explicit map lookup so callers can
// distinguish "unpriced" from "$0".
//
// The web search fee is added on top of the token cost, and only when the
// row's cost is computed: an explicitly reported cost is authoritative for
// the whole row and is assumed to already settle its server tool use. An
// unpriced model still gets the fee, because the fee is a known amount of
// real spend regardless of what the model's token rates are; priced stays
// false so the row is still reported as an incomplete estimate.
func sessionRowCost(
	r usageScanRow, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	return sessionRowCostWithWebSearchRequests(
		r, usageRowWebSearchRequests(r.usageSource, r.tokenJSON), pricing)
}

func sessionRowCostWithWebSearchRequests(
	r usageScanRow, webSearches int, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	var inTok, outTok, crTok, rdTok int
	reasoningTok := r.reasoningTokens
	if r.usageSource == "message" {
		inTok, outTok, crTok, rdTok, reasoningTok =
			clampedUsageTokenCountersWithReasoning(r.tokenJSON)
	} else {
		inTok, outTok, crTok, rdTok = usageEventRowTokens(
			r.usageSource,
			r.inputTokens, r.outputTokens,
			r.cacheCreationInputTokens, r.cacheReadInputTokens)
	}
	pricedModel, lookup := pricing.Resolve(
		r.model, usageLookupModel(r.model, r.ts))
	if r.cost.Valid {
		pricing.RecordResolvedReported(r.model, pricedModel, lookup)
		return money.Money{Microdollars: r.cost.Int64}, true, true, nil
	}
	if !activity.UsageDataContributes(
		false, inTok, outTok, reasoningTok, crTok, rdTok, webSearches,
	) {
		return money.Money{}, true, false, nil
	}
	if !lookup.OK {
		pricing.RecordResolvedComputed(r.model, pricedModel, lookup)
		fee, feeErr := export.WebSearchFee(webSearches)
		if feeErr != nil {
			return money.Money{}, false, false, feeErr
		}
		return fee, false, true, nil
	}
	requestScoped := usageRowIsRequestScoped(r.usageSource, r.messageOrdinal)
	cost, err = lookup.Rates.CostForTokensScoped(
		requestScoped,
		inTok, outTok, reasoningTok, crTok, rdTok)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing session usage for model %q: %w", r.model, err)
	}
	cost, err = export.AddWebSearchFee(cost, webSearches)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing session usage for model %q: %w", r.model, err)
	}
	recordComputedUsagePricing(
		pricing,
		r.model,
		pricedModel,
		lookup,
		requestScoped,
		inTok,
		crTok,
		rdTok,
	)
	return cost, true, true, nil
}

func sessionUsageBreakdownEntryWithWebSearchRequests(
	r usageScanRow,
	ordinal int,
	cost money.Money,
	priced bool,
	webSearches int,
) SessionUsageBreakdownEntry {
	var inTok, outTok, crTok, rdTok int
	if r.usageSource == "message" {
		inTok, outTok, crTok, rdTok =
			clampedUsageTokenCounters(r.tokenJSON)
	} else {
		inTok, outTok, crTok, rdTok = usageEventRowTokens(
			r.usageSource,
			r.inputTokens, r.outputTokens,
			r.cacheCreationInputTokens, r.cacheReadInputTokens)
	}
	entry := SessionUsageBreakdownEntry{
		Ordinal:                  ordinal,
		Source:                   r.usageSource,
		Label:                    sessionUsageBreakdownLabel(r),
		Timestamp:                r.ts,
		Model:                    r.model,
		InputTokens:              inTok,
		OutputTokens:             outTok,
		CacheCreationInputTokens: crTok,
		CacheReadInputTokens:     rdTok,
		WebSearchRequests:        webSearches,
		Cost:                     cost,
		HasCost:                  priced,
	}
	if r.messageOrdinal.Valid {
		messageOrdinal := int(r.messageOrdinal.Int64)
		entry.MessageOrdinal = &messageOrdinal
	}
	return entry
}

func sessionUsageBreakdownLabel(r usageScanRow) string {
	return SessionUsageBreakdownLabel(
		nullInt64Pointer(r.messageOrdinal), r.usageSource)
}

// getSessionUsageLegacy is the wide-row test oracle. It starts from GetSession
// (so metadata and session-level
// token aggregates are reported even when there are no per-message
// usage rows), then aggregates cost over the session's own usage
// rows. Dedup is intra-session only; this reports the session's own
// usage, which can diverge from the dashboard's cross-session
// credited total for fork/subagent sessions. Returns (nil, nil) when
// the session does not exist. BreakdownCount is always populated;
// per-row Breakdown entries are built only when includeBreakdown is
// true so callers that need just the totals avoid the row payload.
func (db *DB) getSessionUsageLegacy(
	ctx context.Context, sessionID string, includeBreakdown bool,
) (*SessionUsage, error) {
	sess, err := db.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil
	}

	pricing, err := db.loadPricingMap(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)

	query := usageRowSelect() + ` AND u.session_id = ?
		ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC,
		u.usage_source ASC,
		COALESCE(u.usage_dedup_key, '') ASC`
	rows, err := db.getReader().QueryContext(ctx, query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("querying session usage: %w", err)
	}
	defer rows.Close()

	var cost money.Money
	var authoritativeCost *money.Money
	var hasComputedCost, hasReportedCost bool
	contributing := false
	allPriced := true
	modelsSet := make(map[string]struct{})
	unpricedSet := make(map[string]struct{})
	breakdown := make([]SessionUsageBreakdownEntry, 0)
	breakdownCount := 0

	var usageRows []usageScanRow
	for rows.Next() {
		r, scanErr := scanUsageRow(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scanning session usage row: %w", scanErr)
		}
		usageRows = append(usageRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating session usage rows: %w", err)
	}
	snapshotRows := make([]activity.UsageRow, len(usageRows))
	for i, r := range usageRows {
		var outputTokens int
		if r.usageSource == "message" {
			_, outputTokens, _, _ = clampedUsageTokenCounters(r.tokenJSON)
		} else {
			_, outputTokens, _, _ = usageEventRowTokens(
				r.usageSource,
				r.inputTokens, r.outputTokens,
				r.cacheCreationInputTokens, r.cacheReadInputTokens)
		}
		snapshotRows[i] = activity.UsageRow{
			SessionID:      r.sessionID,
			Timestamp:      r.ts,
			MessageOrdinal: usageRowMessageOrdinal(r.messageOrdinal),
			OutputTokens:   outputTokens,
			WebSearchRequests: usageRowWebSearchRequests(
				r.usageSource, r.tokenJSON),
			ClaudeMessageID: r.claudeMessageID,
			ClaudeRequestID: r.claudeRequestID,
		}
	}
	snapshotMask, _, snapshotWebSearchRequests :=
		activity.ClaudeSnapshotSurvivorSelection(snapshotRows)
	deduplicatedOutputTokens := 0
	seen := make(map[usageDedupToken]struct{})
	for i, r := range usageRows {
		if !snapshotMask[i] {
			deduplicatedOutputTokens += snapshotRows[i].OutputTokens
			continue
		}
		if key, ok := usageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		costRow := r
		authoritative := r.costSource == CopilotReportedCostSource && r.cost.Valid
		if authoritative {
			v := money.Money{Microdollars: r.cost.Int64}
			authoritativeCost = &v
			costRow.cost = sql.NullInt64{}
		}
		c, priced, contributes, priceErr := sessionRowCostWithWebSearchRequests(
			costRow, snapshotWebSearchRequests[i], rateResolver)
		if priceErr != nil {
			return nil, priceErr
		}
		if !contributes {
			continue
		}
		contributing = true
		modelsSet[r.model] = struct{}{}
		if !authoritative {
			if r.cost.Valid {
				hasReportedCost = true
			} else {
				hasComputedCost = true
			}
		}
		if priced {
			cost, priceErr = money.Add(cost, c)
			if priceErr != nil {
				return nil, fmt.Errorf("summing session usage cost: %w", priceErr)
			}
		} else {
			allPriced = false
			unpricedSet[r.model] = struct{}{}
		}
		breakdownCount++
		if includeBreakdown {
			breakdown = append(breakdown,
				sessionUsageBreakdownEntryWithWebSearchRequests(
					r, breakdownCount, c, priced,
					snapshotWebSearchRequests[i]))
		}
	}
	if authoritativeCost != nil && len(breakdown) > 0 {
		weights := make([]money.Money, len(breakdown))
		for i := range breakdown {
			weights[i] = breakdown[i].Cost
		}
		costs := export.AllocateCostByWeight(*authoritativeCost, weights)
		for i := range breakdown {
			breakdown[i].Cost = costs[i]
			breakdown[i].HasCost = true
		}
	}

	out := &SessionUsage{
		SessionID:         sess.ID,
		Agent:             sess.Agent,
		Project:           sess.Project,
		TotalOutputTokens: max(sess.TotalOutputTokens-deduplicatedOutputTokens, 0),
		PeakContextTokens: sess.PeakContextTokens,
		HasTokenData:      sess.HasTotalOutputTokens || sess.HasPeakContextTokens,
		Models:            sortedSetKeys(modelsSet),
		HasCost:           authoritativeCost != nil || (contributing && allPriced),
		BreakdownCount:    breakdownCount,
		Breakdown:         breakdown,
	}
	if out.HasCost {
		if authoritativeCost != nil {
			out.Cost = *authoritativeCost
			out.CostSource = export.CostSourceReported
		} else {
			out.Cost = cost
			out.CostSource = export.CombinedCostSource(hasComputedCost, hasReportedCost)
		}
		out.AICredits = AICreditsFromCost(sess.Agent, out.Cost)
	}
	out.CostUSD = CostUSDFromCost(out.HasCost, out.Cost)
	if len(unpricedSet) > 0 {
		out.UnpricedModels = sortedSetKeys(unpricedSet)
	}
	return out, nil
}

// sortedSetKeys returns the map keys sorted; never nil so JSON
// renders "[]" rather than "null".
func sortedSetKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// UsageSessionCounts holds distinct session counts grouped by
// project and agent over a filter range.
type UsageSessionCounts struct {
	Total     int            `json:"total"`
	ByProject map[string]int `json:"byProject"`
	ByAgent   map[string]int `json:"byAgent"`
}

type UsageSessionInfo struct {
	Project string
	Agent   string
}

func NewUsageSessionCounts(
	seen map[string]UsageSessionInfo,
) UsageSessionCounts {
	out := UsageSessionCounts{
		Total:     len(seen),
		ByProject: make(map[string]int),
		ByAgent:   make(map[string]int),
	}
	for _, info := range seen {
		out.ByProject[info.Project]++
		out.ByAgent[info.Agent]++
	}
	return out
}

// getUsageSessionCountsLegacy is the wide-row test oracle. Sessions spanning
// multiple days count
// once. Soft-deleted sessions are excluded via
// usageMessageEligibility.
//
// Like GetDailyUsage and GetTopSessionsByCost, this query pads
// the UTC bounds by +/-14h and applies a post-query localDate
// filter so timezone-boundary messages are counted correctly.
func (db *DB) getUsageSessionCountsLegacy(
	ctx context.Context, f UsageFilter,
) (UsageSessionCounts, error) {
	query, args := topSessionsUsageRowQuery(f)
	// Deterministic ordering so the Claude dedup winner — the
	// session that "owns" a shared message — is stable across
	// runs. Matches GetDailyUsage / GetTopSessionsByCost so all
	// three queries agree on which session gets credit.
	query += ` ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return UsageSessionCounts{},
			fmt.Errorf("querying session counts: %w", err)
	}
	defer rows.Close()

	loc := f.location()

	// Track which sessions pass the localDate filter via a
	// set of seen session IDs. Each session is counted once
	// regardless of how many qualifying messages it has.
	type sessInfo struct {
		project string
		agent   string
	}
	seen := make(map[string]sessInfo)

	// Usage dedup mirrors GetDailyUsage: if a session only
	// qualifies because of rows that duplicate an earlier
	// session's usage (fork/subagent replays), that session
	// should NOT be counted. Otherwise sessionCounts would
	// disagree with the deduped token totals.
	dedup := make(map[usageDedupToken]struct{})

	for rows.Next() {
		r, err := scanDailyUsageRow(rows)
		if err != nil {
			return UsageSessionCounts{},
				fmt.Errorf("scanning session counts: %w", err)
		}

		// Post-query date filter (same as GetDailyUsage).
		date := localDate(r.ts, loc)
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}

		// Dedup AFTER the date filter, matching the other two
		// queries so ±14h padding rows don't claim keys.
		if key, ok := usageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := dedup[key]; dup {
				continue
			}
			dedup[key] = struct{}{}
		}

		if _, ok := seen[r.sessionID]; !ok {
			seen[r.sessionID] = sessInfo{
				project: r.project,
				agent:   r.agent,
			}
		}
	}
	if err := rows.Err(); err != nil {
		return UsageSessionCounts{},
			fmt.Errorf("iterating session counts: %w", err)
	}

	out := UsageSessionCounts{
		Total:     len(seen),
		ByProject: make(map[string]int),
		ByAgent:   make(map[string]int),
	}
	for _, info := range seen {
		out.ByProject[info.project]++
		out.ByAgent[info.agent]++
	}

	return out, nil
}

// getUsageMatchingSessionCountLegacy is the wide-row test oracle for sessions
// that match the usage filter
// even when they have no token-bearing usage rows. Bounded ranges are
// resolved against the timestamps of the sessions' messages/usage_events
// rows (falling back to s.started_at for rows with no timestamp of their
// own), the same shape usageRowsSQLForBounds uses, so a session whose
// started_at/ended_at fall outside the window but whose message activity
// falls inside it is still counted.
func (db *DB) getUsageMatchingSessionCountLegacy(
	ctx context.Context, f UsageFilter,
) (int, error) {
	bounds := usageBoundsForFilter(f)

	if !bounds.bounded() {
		where, args := f.appendUsageSessionFilterClauses(usageSessionEligibility, nil)
		where, args = f.appendUsageMatchingActivityClauses(where, args)

		var count int
		err := db.getReader().QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM sessions s WHERE `+where, args...).Scan(&count)
		if err != nil {
			return 0, fmt.Errorf("querying matching usage sessions: %w", err)
		}
		return count, nil
	}

	rowsSQL, args := usageMatchingSessionRowsSQLForBounds(f, bounds)
	rows, err := db.getReader().QueryContext(
		ctx, dailyUsageRowSelectFromRows(rowsSQL), args...)
	if err != nil {
		return 0, fmt.Errorf("querying matching usage sessions: %w", err)
	}
	defer rows.Close()

	loc := f.location()
	seen := make(map[string]struct{})
	for rows.Next() {
		r, err := scanDailyUsageRow(rows)
		if err != nil {
			return 0, fmt.Errorf("scanning matching usage session: %w", err)
		}
		date := localDate(r.ts, loc)
		if date == "" {
			continue
		}
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}
		seen[r.sessionID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating matching usage sessions: %w", err)
	}
	return len(seen), nil
}
