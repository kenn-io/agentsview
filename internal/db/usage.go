package db

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

func lookupModelRates(
	pricing map[string]modelRates, model string,
) (modelRates, bool) {
	return pricingpkg.Resolve(pricing, model)
}

// UsageFilter controls the date range, agent, and timezone
// for daily usage aggregation queries.
type UsageFilter struct {
	From             string // YYYY-MM-DD, inclusive
	To               string // YYYY-MM-DD, inclusive
	Agent            string // "" for all; supports comma-separated
	Project          string // "" for all; supports comma-separated
	Machine          string // "" for all; supports comma-separated
	Model            string // "" for all; supports comma-separated
	ExcludeProject   string // comma-separated projects to exclude
	ExcludeAgent     string // comma-separated agents to exclude
	ExcludeModel     string // comma-separated models to exclude
	Timezone         string // IANA timezone, "" for UTC
	MinUserMessages  int    // user_message_count >= N
	ExcludeOneShot   bool   // user_message_count > 1
	ExcludeAutomated bool   // is_automated = false
	ActiveSince      string // RFC3339 session recency cutoff
	Breakdowns       bool   // populate Project/AgentBreakdowns per day
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

	where, args = appendCSV(where, args, "s.agent", f.Agent, true)
	where, args = appendCSV(where, args, "s.project", f.Project, true)
	where, args = appendCSV(where, args, "s.machine", f.Machine, true)
	where, args = appendCSV(where, args, "s.project", f.ExcludeProject, false)
	where, args = appendCSV(where, args, "s.agent", f.ExcludeAgent, false)

	if f.MinUserMessages > 0 {
		where += "\n\tAND s.user_message_count >= ?"
		args = append(args, f.MinUserMessages)
	}
	if f.ExcludeOneShot {
		where += "\n\tAND s.user_message_count > 1"
	}
	if f.ExcludeAutomated {
		where += "\n\tAND COALESCE(s.is_automated, 0) = 0"
	}
	if f.ActiveSince != "" {
		where += "\n\tAND COALESCE(s.ended_at, s.started_at, s.created_at) >= ?"
		args = append(args, f.ActiveSince)
	}

	return where, args
}

// location loads the timezone or returns the system local timezone.
func (f UsageFilter) location() *time.Location {
	if f.Timezone == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(f.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
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
// per-query claude_message_id + claude_request_id dedup in
// GetDailyUsage, which is more precise than a blanket exclusion:
// a fork session can legitimately contribute unique-keyed messages
// that should still be counted (see
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
	COALESCE(m.timestamp, s.started_at) AS ts,
	m.model,
	m.token_usage,
	0 AS input_tokens,
	0 AS output_tokens,
	0 AS cache_creation_input_tokens,
	0 AS cache_read_input_tokens,
	0 AS reasoning_tokens,
	NULL AS cost_usd,
	'' AS cost_status,
	'' AS cost_source,
	m.claude_message_id,
	m.claude_request_id,
	'' AS usage_dedup_key,
	s.project,
	s.agent,
	s.machine,
	s.user_message_count,
	COALESCE(s.is_automated, 0) AS is_automated,
	COALESCE(s.ended_at, s.started_at, s.created_at) AS session_activity_at,
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
	COALESCE(ue.occurred_at, s.started_at) AS ts,
	ue.model,
	'' AS token_usage,
	ue.input_tokens,
	ue.output_tokens,
	ue.cache_creation_input_tokens,
	ue.cache_read_input_tokens,
	ue.reasoning_tokens,
	ue.cost_usd,
	ue.cost_status,
	ue.cost_source,
	'' AS claude_message_id,
	'' AS claude_request_id,
	CASE
		WHEN ue.dedup_key != '' THEN ue.session_id || ':' || ue.source || ':' || ue.dedup_key
		ELSE ue.session_id || ':' || ue.source || ':id:' || ue.id
	END AS usage_dedup_key,
	s.project,
	s.agent,
	s.machine,
	s.user_message_count,
	COALESCE(s.is_automated, 0) AS is_automated,
	COALESCE(s.ended_at, s.started_at, s.created_at) AS session_activity_at,
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
	COALESCE(m.timestamp, s.started_at) AS ts,
	m.model,
	m.token_usage,
	0 AS input_tokens,
	0 AS output_tokens,
	0 AS cache_creation_input_tokens,
	0 AS cache_read_input_tokens,
	NULL AS cost_usd,
	m.claude_message_id,
	m.claude_request_id,
	'' AS usage_dedup_key,
	s.project,
	s.agent
FROM messages m
JOIN sessions s ON m.session_id = s.id
WHERE %s

UNION ALL

SELECT
	ue.session_id,
	ue.message_ordinal,
	ue.source AS usage_source,
	COALESCE(ue.occurred_at, s.started_at) AS ts,
	ue.model,
	'' AS token_usage,
	ue.input_tokens,
	ue.output_tokens,
	ue.cache_creation_input_tokens,
	ue.cache_read_input_tokens,
	ue.cost_usd,
	'' AS claude_message_id,
	'' AS claude_request_id,
	CASE
		WHEN ue.dedup_key != '' THEN ue.session_id || ':' || ue.source || ':' || ue.dedup_key
		ELSE ue.session_id || ':' || ue.source || ':id:' || ue.id
	END AS usage_dedup_key,
	s.project,
	s.agent
FROM usage_events ue
JOIN sessions s ON s.id = ue.session_id
WHERE %s`

const dailyUsageMessageRowsSQLTemplate = `
SELECT
	m.session_id,
	m.ordinal AS message_ordinal,
	'message' AS usage_source,
	COALESCE(m.timestamp, s.started_at) AS ts,
	m.model,
	m.token_usage,
	0 AS input_tokens,
	0 AS output_tokens,
	0 AS cache_creation_input_tokens,
	0 AS cache_read_input_tokens,
	NULL AS cost_usd,
	m.claude_message_id,
	m.claude_request_id,
	'' AS usage_dedup_key,
	s.project,
	s.agent
FROM %s m
JOIN sessions s ON m.session_id = s.id
WHERE %s`

const dailyUsageEventRowsSQLTemplate = `
SELECT
	ue.session_id,
	ue.message_ordinal,
	ue.source AS usage_source,
	COALESCE(ue.occurred_at, s.started_at) AS ts,
	ue.model,
	'' AS token_usage,
	ue.input_tokens,
	ue.output_tokens,
	ue.cache_creation_input_tokens,
	ue.cache_read_input_tokens,
	ue.cost_usd,
	'' AS claude_message_id,
	'' AS claude_request_id,
	CASE
		WHEN ue.dedup_key != '' THEN ue.session_id || ':' || ue.source || ':' || ue.dedup_key
		ELSE ue.session_id || ':' || ue.source || ':id:' || ue.id
	END AS usage_dedup_key,
	s.project,
	s.agent
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
		m.timestamp,
		m.model,
		m.token_usage,
		m.claude_message_id,
		m.claude_request_id
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
		ue.cost_usd,
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
	costUSD                  sql.NullFloat64
	costStatus               string
	costSource               string
	claudeMessageID          string
	claudeRequestID          string
	usageDedupKey            string
	project                  string
	agent                    string
	machine                  string
	userMessageCount         int
	isAutomated              int
	sessionActivityAt        string
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
	inputTokens              int
	outputTokens             int
	cacheCreationInputTokens int
	cacheReadInputTokens     int
	costUSD                  sql.NullFloat64
	claudeMessageID          string
	claudeRequestID          string
	usageDedupKey            string
	project                  string
	agent                    string
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
	u.cost_usd,
	u.cost_status,
	u.cost_source,
	u.claude_message_id,
	u.claude_request_id,
	u.usage_dedup_key,
	u.project,
	u.agent,
	u.machine,
	u.user_message_count,
	u.is_automated,
	u.session_activity_at,
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
	u.cost_usd,
	u.claude_message_id,
	u.claude_request_id,
	u.usage_dedup_key,
	u.project,
	u.agent
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

func dailyUsageRowsSQLForBounds(
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

	messageTimestampSourceWhere := usageMessageSourceEligibility +
		"\n\tAND m.timestamp IS NOT NULL"
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

	messageFallbackWhere := usageMessageEligibility +
		"\n\tAND m.timestamp IS NULL"
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

func usageRowQuery(f UsageFilter) (string, []any) {
	rowsSQL, args := dailyUsageRowsSQLForBounds(f, usageBoundsForFilter(f))
	query := dailyUsageRowSelectFromRows(rowsSQL)
	return query, args
}

func topSessionsUsageRowQuery(f UsageFilter) (string, []any) {
	return usageRowQuery(f)
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
		&r.costUSD,
		&r.costStatus,
		&r.costSource,
		&r.claudeMessageID,
		&r.claudeRequestID,
		&r.usageDedupKey,
		&r.project,
		&r.agent,
		&r.machine,
		&r.userMessageCount,
		&r.isAutomated,
		&r.sessionActivityAt,
		&r.displayName,
		&r.startedAt,
	)
	return r, err
}

func scanDailyUsageRow(rows *sql.Rows) (dailyUsageScanRow, error) {
	var r dailyUsageScanRow
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
		&r.costUSD,
		&r.claudeMessageID,
		&r.claudeRequestID,
		&r.usageDedupKey,
		&r.project,
		&r.agent,
	)
	return r, err
}

func dailyUsageAmounts(
	r dailyUsageScanRow, pricing map[string]modelRates,
) (inputTok, outputTok, cacheCrTok, cacheRdTok int, cost, savings float64) {
	if r.usageSource == "message" {
		usage := gjson.Parse(r.tokenJSON)
		inputTok = int(usage.Get("input_tokens").Int())
		outputTok = int(usage.Get("output_tokens").Int())
		cacheCrTok = int(
			usage.Get("cache_creation_input_tokens").Int())
		cacheRdTok = int(
			usage.Get("cache_read_input_tokens").Int())
	} else {
		inputTok = r.inputTokens
		outputTok = r.outputTokens
		cacheCrTok = r.cacheCreationInputTokens
		cacheRdTok = r.cacheReadInputTokens
	}

	rates, _ := lookupModelRates(pricing, r.model)
	if r.costUSD.Valid {
		cost = r.costUSD.Float64
	} else {
		cost = (float64(inputTok)*rates.input +
			float64(outputTok)*rates.output +
			float64(cacheCrTok)*rates.cacheCreation +
			float64(cacheRdTok)*rates.cacheRead) / 1_000_000
	}

	readDelta := float64(cacheRdTok) *
		(rates.input - rates.cacheRead) / 1_000_000
	crDelta := float64(cacheCrTok) *
		(rates.input - rates.cacheCreation) / 1_000_000
	savings = readDelta + crDelta
	return
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
	TotalCost           float64            `json:"totalCost"`
	ModelsUsed          []string           `json:"modelsUsed"`
	ModelBreakdowns     []ModelBreakdown   `json:"modelBreakdowns,omitempty"`
	ProjectBreakdowns   []ProjectBreakdown `json:"projectBreakdowns,omitempty"`
	AgentBreakdowns     []AgentBreakdown   `json:"agentBreakdowns,omitempty"`
}

// ModelBreakdown holds per-model token and cost breakdown.
type ModelBreakdown struct {
	ModelName           string  `json:"modelName"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	Cost                float64 `json:"cost"`
}

// ProjectBreakdown is the per-project slice of a day's usage.
type ProjectBreakdown struct {
	Project             string  `json:"project"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	Cost                float64 `json:"cost"`
}

// AgentBreakdown is the per-agent slice of a day's usage.
type AgentBreakdown struct {
	Agent               string  `json:"agent"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	Cost                float64 `json:"cost"`
}

// UsageTotals holds aggregate token and cost totals.
type UsageTotals struct {
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheReadTokens     int     `json:"cacheReadTokens"`
	TotalCost           float64 `json:"totalCost"`
	// CacheSavings is the net dollar delta vs an uncached run:
	// cache reads save (input_rate - cache_read_rate) per token,
	// cache creations cost (input_rate - cache_creation_rate)
	// per token (usually negative because creation is billed
	// above the input rate). Computed from per-model rates so
	// mixed-model workloads get the right number, not a fixed
	// Sonnet proxy.
	CacheSavings float64 `json:"cacheSavings"`
}

// DailyUsageResult wraps the daily entries and totals.
type DailyUsageResult struct {
	Daily         []DailyUsageEntry  `json:"daily"`
	Totals        UsageTotals        `json:"totals"`
	SessionCounts UsageSessionCounts `json:"sessionCounts,omitempty"`
}

// modelRates holds per-model pricing in rate-per-token form.
type modelRates struct {
	input         float64
	output        float64
	cacheCreation float64
	cacheRead     float64
}

// loadPricingMap reads the model_pricing table into a map for
// in-memory joins. This is much faster than a SQL LEFT JOIN
// on every row of the daily usage scan, since the pricing
// table is tiny (a few dozen rows) and lookups are O(1).
func (db *DB) loadPricingMap(
	ctx context.Context,
) (map[string]modelRates, error) {
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT model_pattern,
			input_per_mtok, output_per_mtok,
			cache_creation_per_mtok, cache_read_per_mtok
		 FROM model_pricing
		 WHERE model_pattern NOT LIKE '\_%' ESCAPE '\'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]modelRates)
	for rows.Next() {
		var (
			pattern string
			rates   modelRates
		)
		if err := rows.Scan(
			&pattern,
			&rates.input, &rates.output,
			&rates.cacheCreation, &rates.cacheRead,
		); err != nil {
			return nil, err
		}
		out[pattern] = rates
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for model, cp := range db.customPricing {
		out[model] = modelRates{
			input:         cp.Input,
			output:        cp.Output,
			cacheCreation: cp.CacheCreation,
			cacheRead:     cp.CacheRead,
		}
	}

	return out, nil
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

// GetDailyUsage returns token usage and cost aggregated by day.
// It scans messages with non-empty token_usage JSON blobs,
// parses them in Go (faster than SQLite's json_extract per row),
// joins against an in-memory pricing map, and buckets by
// local date.
func (db *DB) GetDailyUsage(
	ctx context.Context, f UsageFilter,
) (DailyUsageResult, error) {
	loc := f.location()

	pricing, err := db.loadPricingMap(ctx)
	if err != nil {
		return DailyUsageResult{},
			fmt.Errorf("loading pricing: %w", err)
	}

	// Filter on usage timestamp (not only session started_at) so
	// long-lived sessions that span date boundaries are included.
	// Pad by +/-14h to cover all timezone offsets; the actual
	// date filtering happens post-query via localDate.
	query, args := usageRowQuery(f)
	query += ` ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC`

	rows, err := db.getReader().QueryContext(ctx, query, args...)
	if err != nil {
		return DailyUsageResult{},
			fmt.Errorf("querying daily usage: %w", err)
	}
	defer rows.Close()

	// 4-tuple key for per-(date, project, agent, model) accumulation.
	type accumKey struct {
		date    string
		project string
		agent   string
		model   string
	}
	type bucket struct {
		inputTok  int
		outputTok int
		cacheCr   int
		cacheRd   int
		cost      float64
	}

	accum := make(map[accumKey]*bucket)

	type dedupKey struct {
		msgID, reqID string
	}
	seen := make(map[dedupKey]struct{})
	seenSessions := make(map[string]UsageSessionInfo)

	// totalSavings is the running sum of per-message cache
	// savings using each row's actual per-model rates. We sum
	// at the message level instead of deriving from totals
	// later because the rate mix varies per workload and a
	// single fallback rate would misreport mixed-model periods.
	var totalSavings float64

	for rows.Next() {
		r, scanErr := scanDailyUsageRow(rows)
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
		if r.claudeMessageID != "" && r.claudeRequestID != "" {
			key := dedupKey{
				msgID: r.claudeMessageID,
				reqID: r.claudeRequestID,
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		} else if r.usageDedupKey != "" {
			key := dedupKey{msgID: "usage", reqID: r.usageDedupKey}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		if _, ok := seenSessions[r.sessionID]; !ok {
			seenSessions[r.sessionID] = UsageSessionInfo{
				Project: r.project,
				Agent:   r.agent,
			}
		}

		inputTok, outputTok, cacheCrTok, cacheRdTok, cost, savings :=
			dailyUsageAmounts(r, pricing)
		totalSavings += savings

		key := accumKey{
			date: date, project: r.project,
			agent: r.agent, model: r.model,
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
		b.cost += cost
	}
	if err := rows.Err(); err != nil {
		return DailyUsageResult{},
			fmt.Errorf("iterating daily usage rows: %w", err)
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
			cost      float64
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
			ma.cost += b.cost
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
				if ci != cj {
					return ci > cj
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
				entry.TotalCost += ma.cost
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
			totals.TotalCost += entry.TotalCost
		}

		if daily == nil {
			daily = []DailyUsageEntry{}
		}
		totals.CacheSavings = totalSavings
		sessionCounts := NewUsageSessionCounts(seenSessions)
		return DailyUsageResult{
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
	}
	days := make(map[string]*dayMaps, 64)
	for key, b := range accum {
		dm, ok := days[key.date]
		if !ok {
			dm = &dayMaps{
				models:   make(map[string]bucket, 4),
				projects: make(map[string]bucket, 8),
				agents:   make(map[string]bucket, 4),
			}
			days[key.date] = dm
		}
		cur := dm.models[key.model]
		cur.inputTok += b.inputTok
		cur.outputTok += b.outputTok
		cur.cacheCr += b.cacheCr
		cur.cacheRd += b.cacheRd
		cur.cost += b.cost
		dm.models[key.model] = cur

		cur = dm.projects[key.project]
		cur.inputTok += b.inputTok
		cur.outputTok += b.outputTok
		cur.cacheCr += b.cacheCr
		cur.cacheRd += b.cacheRd
		cur.cost += b.cost
		dm.projects[key.project] = cur

		cur = dm.agents[key.agent]
		cur.inputTok += b.inputTok
		cur.outputTok += b.outputTok
		cur.cacheCr += b.cacheCr
		cur.cacheRd += b.cacheRd
		cur.cost += b.cost
		dm.agents[key.agent] = cur
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
			if ci != cj {
				return ci > cj
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
			entry.TotalCost += b.cost
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
			if pbd[i].Cost != pbd[j].Cost {
				return pbd[i].Cost > pbd[j].Cost
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
			if abd[i].Cost != abd[j].Cost {
				return abd[i].Cost > abd[j].Cost
			}
			return abd[i].Agent < abd[j].Agent
		})
		entry.AgentBreakdowns = abd

		daily = append(daily, entry)

		totals.InputTokens += entry.InputTokens
		totals.OutputTokens += entry.OutputTokens
		totals.CacheCreationTokens += entry.CacheCreationTokens
		totals.CacheReadTokens += entry.CacheReadTokens
		totals.TotalCost += entry.TotalCost
	}

	if daily == nil {
		daily = []DailyUsageEntry{}
	}

	totals.CacheSavings = totalSavings
	sessionCounts := NewUsageSessionCounts(seenSessions)
	return DailyUsageResult{
		Daily:         daily,
		Totals:        totals,
		SessionCounts: sessionCounts,
	}, nil
}

// TopSessionEntry is one row in the "top sessions by cost" result.
type TopSessionEntry struct {
	SessionID   string  `json:"sessionId"`
	DisplayName string  `json:"displayName"`
	Agent       string  `json:"agent"`
	Project     string  `json:"project"`
	StartedAt   string  `json:"startedAt"`
	TotalTokens int     `json:"totalTokens"`
	Cost        float64 `json:"cost"`
}

// GetTopSessionsByCost returns sessions ranked by total cost
// over the filter range. Default limit 20, max 100.
func (db *DB) GetTopSessionsByCost(
	ctx context.Context, f UsageFilter, limit int,
) ([]TopSessionEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	pricing, err := db.loadPricingMap(ctx)
	if err != nil {
		return nil,
			fmt.Errorf("loading pricing: %w", err)
	}

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
		totalTokens int
		cost        float64
	}

	accum := make(map[string]*sessAccum)
	// Track insertion order for stable iteration.
	var order []string

	// Dedup duplicate Claude messages across fork/subagent
	// boundaries so per-session totals match the aggregate
	// totals from GetDailyUsage. Same key and ordering rules.
	type dedupKey struct {
		msgID, reqID string
	}
	seen := make(map[dedupKey]struct{})

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
		if r.claudeMessageID != "" && r.claudeRequestID != "" {
			key := dedupKey{
				msgID: r.claudeMessageID,
				reqID: r.claudeRequestID,
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		} else if r.usageDedupKey != "" {
			key := dedupKey{msgID: "usage", reqID: r.usageDedupKey}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		inputTok, outputTok, cacheCrTok, cacheRdTok, cost, _ :=
			dailyUsageAmounts(r, pricing)

		sa, ok := accum[r.sessionID]
		if !ok {
			sa = &sessAccum{}
			accum[r.sessionID] = sa
			order = append(order, r.sessionID)
		}
		sa.totalTokens += inputTok + outputTok +
			cacheCrTok + cacheRdTok
		sa.cost += cost
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
			SessionID:   id,
			DisplayName: id,
			TotalTokens: sa.totalTokens,
			Cost:        sa.cost,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].Cost != result[j].Cost {
			return result[i].Cost > result[j].Cost
		}
		return result[i].SessionID < result[j].SessionID
	})

	if len(result) > limit {
		result = result[:limit]
	}

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
// (usage_events.cost_usd). CostUSD is non-zero only when HasCost is
// true; a partial total (some models unpriced) is never emitted.
type SessionUsage struct {
	SessionID         string   `json:"session_id"`
	Agent             string   `json:"agent"`
	Project           string   `json:"project"`
	TotalOutputTokens int      `json:"total_output_tokens"`
	PeakContextTokens int      `json:"peak_context_tokens"`
	HasTokenData      bool     `json:"has_token_data"`
	CostUSD           float64  `json:"cost_usd"`
	HasCost           bool     `json:"has_cost"`
	Models            []string `json:"models"`
	UnpricedModels    []string `json:"unpriced_models,omitempty"`
}

// sessionRowCost computes one usage row's cost and reports whether
// it was priced and whether it contributes to the estimate. A row
// contributes when it carries an explicit cost or any tokens. It does
// an explicit map lookup so callers can distinguish "unpriced" from
// "$0".
func sessionRowCost(
	r usageScanRow, pricing map[string]modelRates,
) (cost float64, priced, contributes bool) {
	var inTok, outTok, crTok, rdTok int
	if r.usageSource == "message" {
		usage := gjson.Parse(r.tokenJSON)
		inTok = int(usage.Get("input_tokens").Int())
		outTok = int(usage.Get("output_tokens").Int())
		crTok = int(usage.Get("cache_creation_input_tokens").Int())
		rdTok = int(usage.Get("cache_read_input_tokens").Int())
	} else {
		inTok = r.inputTokens
		outTok = r.outputTokens
		crTok = r.cacheCreationInputTokens
		rdTok = r.cacheReadInputTokens
	}

	if r.costUSD.Valid {
		return r.costUSD.Float64, true, true
	}
	if inTok == 0 && outTok == 0 && crTok == 0 && rdTok == 0 {
		return 0, true, false
	}
	rates, ok := lookupModelRates(pricing, r.model)
	if !ok {
		return 0, false, true
	}
	cost = (float64(inTok)*rates.input +
		float64(outTok)*rates.output +
		float64(crTok)*rates.cacheCreation +
		float64(rdTok)*rates.cacheRead) / 1_000_000
	return cost, true, true
}

// GetSessionUsage returns one session's token totals and cost
// estimate. It starts from GetSession (so metadata and session-level
// token aggregates are reported even when there are no per-message
// usage rows), then aggregates cost over the session's own usage
// rows. Dedup is intra-session only; this reports the session's own
// usage, which can diverge from the dashboard's cross-session
// credited total for fork/subagent sessions. Returns (nil, nil) when
// the session does not exist.
func (db *DB) GetSessionUsage(
	ctx context.Context, sessionID string,
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

	query := usageRowSelect() + ` AND u.session_id = ?
		ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC`
	rows, err := db.getReader().QueryContext(ctx, query, sessionID)
	if err != nil {
		return nil, fmt.Errorf("querying session usage: %w", err)
	}
	defer rows.Close()

	var cost float64
	contributing := false
	allPriced := true
	modelsSet := make(map[string]struct{})
	unpricedSet := make(map[string]struct{})

	type dedupKey struct{ msgID, reqID string }
	seen := make(map[dedupKey]struct{})

	for rows.Next() {
		r, scanErr := scanUsageRow(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scanning session usage row: %w", scanErr)
		}
		if r.claudeMessageID != "" && r.claudeRequestID != "" {
			key := dedupKey{r.claudeMessageID, r.claudeRequestID}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		} else if r.usageDedupKey != "" {
			key := dedupKey{"usage", r.usageDedupKey}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		c, priced, contributes := sessionRowCost(r, pricing)
		if !contributes {
			continue
		}
		contributing = true
		modelsSet[r.model] = struct{}{}
		if priced {
			cost += c
		} else {
			allPriced = false
			unpricedSet[r.model] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating session usage rows: %w", err)
	}

	out := &SessionUsage{
		SessionID:         sess.ID,
		Agent:             sess.Agent,
		Project:           sess.Project,
		TotalOutputTokens: sess.TotalOutputTokens,
		PeakContextTokens: sess.PeakContextTokens,
		HasTokenData:      sess.HasTotalOutputTokens || sess.HasPeakContextTokens,
		Models:            sortedSetKeys(modelsSet),
		HasCost:           contributing && allPriced,
	}
	if out.HasCost {
		out.CostUSD = cost
	}
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

// GetUsageSessionCounts returns distinct session counts grouped
// by project and agent. Sessions spanning multiple days count
// once. Soft-deleted sessions are excluded via
// usageMessageEligibility.
//
// Like GetDailyUsage and GetTopSessionsByCost, this query pads
// the UTC bounds by +/-14h and applies a post-query localDate
// filter so timezone-boundary messages are counted correctly.
func (db *DB) GetUsageSessionCounts(
	ctx context.Context, f UsageFilter,
) (UsageSessionCounts, error) {
	query, args := usageRowQuery(f)
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

	// Claude message dedup mirrors GetDailyUsage: if a session
	// only qualifies because of messages that are duplicates of
	// an earlier session's rows (fork/subagent replays), that
	// session should NOT be counted. Otherwise sessionCounts
	// would disagree with the deduped token totals — a fork
	// with zero unique messages would inflate the count even
	// though it contributes zero cost.
	type dedupKey struct {
		msgID, reqID string
	}
	dedup := make(map[dedupKey]struct{})

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
		if r.claudeMessageID != "" && r.claudeRequestID != "" {
			key := dedupKey{
				msgID: r.claudeMessageID,
				reqID: r.claudeRequestID,
			}
			if _, dup := dedup[key]; dup {
				continue
			}
			dedup[key] = struct{}{}
		} else if r.usageDedupKey != "" {
			key := dedupKey{msgID: "usage", reqID: r.usageDedupKey}
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
