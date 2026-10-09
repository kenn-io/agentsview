package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

const pgUsageMessageEligibility = `
	m.token_usage != ''
	AND m.model != ''
	AND m.model != '<synthetic>'
	AND s.deleted_at IS NULL`

const pgUsageMessageSourceEligibility = `
	m.token_usage != ''
	AND m.model != ''
	AND m.model != '<synthetic>'`

const pgUsageMatchingMessageEligibility = `
	m.role = 'assistant'
	AND m.model != '<synthetic>'
	AND s.deleted_at IS NULL`

const pgUsageMatchingMessageSourceEligibility = `
	m.role = 'assistant'
	AND m.model != '<synthetic>'`

const pgUsageEventEligibility = `
	ue.model != ''
	AND s.deleted_at IS NULL`

const pgUsageEventSourceEligibility = `
	ue.model != ''`

const pgUsageSessionEligibility = `s.deleted_at IS NULL`

func appendPGUsageBranchFilterClauses(
	where string, pb *paramBuilder, f db.UsageFilter, modelCol string,
) string {
	where = appendPGUsageSourceFilterClauses(where, pb, f, modelCol)
	return appendPGUsageSessionFilterClauses(where, pb, f)
}

func appendPGUsageSourceFilterClauses(
	where string, pb *paramBuilder, f db.UsageFilter, modelCol string,
) string {
	b := db.NewQueryBuilder(db.PostgresQueryDialect(), pb.n)
	preds := db.BuildUsageSourceFilter(f, b, modelCol)
	where = db.AppendUsagePredicates(where, preds, "\t")
	pb.args = append(pb.args, b.Args()...)
	pb.n += len(b.Args())
	return where
}

func appendPGUsageSessionFilterClauses(
	where string, pb *paramBuilder, f db.UsageFilter,
) string {
	b := db.NewQueryBuilder(db.PostgresQueryDialect(), pb.n)
	preds := db.BuildUsageSessionFilter(f, b, "")
	where = db.AppendUsagePredicates(where, preds, "\t")
	pb.args = append(pb.args, b.Args()...)
	pb.n += len(b.Args())
	return where
}

const pgUsageRowsSQLTemplate = `
SELECT
	m.session_id,
	m.ordinal AS message_ordinal,
	'message' AS usage_source,
	COALESCE(m.timestamp, s.started_at) AS ts,
	m.timestamp AS pricing_ts,
	m.model,
	m.provider_id,
	m.token_usage,
	0 AS input_tokens,
	0 AS output_tokens,
	0 AS cache_creation_input_tokens,
	0 AS cache_read_input_tokens,
	0 AS reasoning_tokens,
	NULL::bigint AS cost_microdollars,
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
	COALESCE(s.is_automated, false) AS is_automated,
	COALESCE(s.ended_at, s.started_at, s.created_at) AS session_activity_at,
	COALESCE(NULLIF(COALESCE(s.display_name, s.session_name), ''), NULLIF(s.first_message, ''), NULLIF(s.project, ''), s.id) AS display_name,
	s.started_at
FROM messages m
JOIN sessions s ON m.session_id = s.id
WHERE %s

UNION ALL

SELECT
	ue.session_id,
	ue.message_ordinal,
	ue.source AS usage_source,
	COALESCE(ue.occurred_at, s.started_at) AS ts,
	ue.occurred_at AS pricing_ts,
	ue.model,
	ue.provider_id,
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
	COALESCE(s.is_automated, false) AS is_automated,
	COALESCE(s.ended_at, s.started_at, s.created_at) AS session_activity_at,
	COALESCE(NULLIF(COALESCE(s.display_name, s.session_name), ''), NULLIF(s.first_message, ''), NULLIF(s.project, ''), s.id) AS display_name,
	s.started_at
FROM usage_events ue
JOIN sessions s ON s.id = ue.session_id
WHERE %s`

func pgUsageRowsSQLWithWhere(
	messageWhere, usageEventWhere string,
) string {
	return fmt.Sprintf(
		pgUsageRowsSQLTemplate,
		messageWhere,
		usageEventWhere,
	)
}

const pgDailyUsageRowsSQLTemplate = `
SELECT
	m.session_id,
	m.ordinal AS message_ordinal,
	'message' AS usage_source,
	COALESCE(m.timestamp, s.started_at) AS ts,
	m.timestamp AS pricing_ts,
	m.model,
	m.provider_id,
	m.token_usage,
	0 AS input_tokens,
	0 AS output_tokens,
	0 AS cache_creation_input_tokens,
	0 AS cache_read_input_tokens,
	0 AS reasoning_tokens,
	NULL::bigint AS cost_microdollars,
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
	COALESCE(ue.occurred_at, s.started_at) AS ts,
	ue.occurred_at AS pricing_ts,
	ue.model,
	ue.provider_id,
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

const pgDailyUsageMessageRowsSQLTemplate = `
SELECT
	m.session_id,
	m.ordinal AS message_ordinal,
	'message' AS usage_source,
	COALESCE(m.timestamp, s.started_at) AS ts,
	m.timestamp AS pricing_ts,
	m.model,
	m.provider_id,
	m.token_usage,
	0 AS input_tokens,
	0 AS output_tokens,
	0 AS cache_creation_input_tokens,
	0 AS cache_read_input_tokens,
	0 AS reasoning_tokens,
	NULL::bigint AS cost_microdollars,
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

const pgDailyUsageEventRowsSQLTemplate = `
SELECT
	ue.session_id,
	ue.message_ordinal,
	ue.source AS usage_source,
	COALESCE(ue.occurred_at, s.started_at) AS ts,
	ue.occurred_at AS pricing_ts,
	ue.model,
	ue.provider_id,
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

func pgDailyUsageRowsSQLWithWhere(
	messageWhere, usageEventWhere string,
) string {
	return fmt.Sprintf(
		pgDailyUsageRowsSQLTemplate,
		messageWhere,
		usageEventWhere,
	)
}

func pgDailyUsageRowsSQLWithTimestampCTEs(
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
		m.provider_id,
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
		ue.provider_id,
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
		pgDailyUsageMessageRowsSQLTemplate,
		"message_timestamp_rows",
		messageTimestampJoinWhere,
	) + `

UNION ALL

` + fmt.Sprintf(
		pgDailyUsageEventRowsSQLTemplate,
		"usage_event_timestamp_rows",
		eventTimestampJoinWhere,
	) + `

UNION ALL

` + fmt.Sprintf(
		pgDailyUsageMessageRowsSQLTemplate,
		"messages",
		messageFallbackWhere,
	) + `

UNION ALL

` + fmt.Sprintf(
		pgDailyUsageEventRowsSQLTemplate,
		"usage_events",
		eventFallbackWhere,
	)
}

type pgUsageScanRow struct {
	sessionID                string
	messageOrdinal           sql.NullInt64
	usageSource              string
	ts                       sql.NullTime
	pricingTS                sql.NullTime
	model                    string
	providerID               string
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
	isAutomated              bool
	sessionActivityAt        sql.NullTime
	displayName              string
	startedAt                sql.NullTime
}

type pgDailyUsageScanRow struct {
	sessionID                string
	messageOrdinal           sql.NullInt64
	usageSource              string
	ts                       sql.NullTime
	pricingTS                sql.NullTime
	model                    string
	providerID               string
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

type pgTopSessionMetadata struct {
	machine     string
	groupKey    string
	sessionName string
	displayName string
	agent       string
	project     string
	startedAt   string
}

func pgUsageRowSelectFromRows(rowsSQL string) string {
	return `
SELECT
	u.session_id,
	u.message_ordinal,
	u.usage_source,
	u.ts,
	u.pricing_ts,
	u.model,
	u.provider_id,
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
	u.display_name,
	u.started_at
FROM (` + rowsSQL + `) u
WHERE 1=1`
}

func pgUsageRowSelect() string {
	return pgUsageRowSelectFromRows(pgUsageRowsSQLWithWhere(
		pgUsageMessageEligibility,
		pgUsageEventEligibility,
	))
}

func pgDailyUsageRowSelectFromRows(rowsSQL string) string {
	return pgDailyUsageRowSelectFromRowsWithMachine(rowsSQL, false)
}

func pgDailyUsageRowSelectFromRowsWithMachine(
	rowsSQL string, includeMachine bool,
) string {
	return pgDailyUsageRowSelectFromRowsWithSession(
		rowsSQL, includeMachine, "u.session_id", false, false)
}

func pgDailyUsageRowSelectFromSnapshotRowsWithMachine(
	rowsSQL string, includeMachine bool,
) string {
	return pgDailyUsageRowSelectFromRowsWithSession(
		rowsSQL, includeMachine, "u.snapshot_attribution_session_id", true, false)
}

func pgDailyUsageRowSelectFromRowsWithSession(
	rowsSQL string, includeMachine bool, sessionColumn string,
	reloadSessionMetadata, includeSessionGroups bool,
) string {
	projectColumn := "u.project"
	agentColumn := "u.agent"
	machineColumnExpr := "u.machine"
	webSearchColumn := `CASE
		WHEN u.usage_source = 'message' THEN GREATEST(COALESCE(
			CAST(agentsview_json_integer(
				u.token_usage,
				ARRAY['server_tool_use', 'web_search_requests'],
				'web_search_requests') AS BIGINT),
			0), 0)
		ELSE 0
	END`
	metadataJoin := ""
	if reloadSessionMetadata {
		metadataJoin = `
LEFT JOIN sessions attributed
	ON attributed.id = u.snapshot_attribution_session_id`
		projectColumn = "CASE WHEN attributed.id IS NULL THEN u.project ELSE attributed.project END"
		agentColumn = "CASE WHEN attributed.id IS NULL THEN u.agent ELSE attributed.agent END"
		machineColumnExpr = "CASE WHEN attributed.id IS NULL THEN u.machine ELSE attributed.machine END"
		webSearchColumn = "u.snapshot_web_search_requests"
	}
	machineColumn := ""
	if includeMachine {
		machineColumn = ",\n\t" + machineColumnExpr
	}
	sessionMetadataColumns := ""
	if includeSessionGroups {
		sessionMetadataColumns = `,
	COALESCE(attributed.machine, '') AS group_machine,
	COALESCE(attributed.group_key, '') AS group_key,
	COALESCE(attributed.session_name, '') AS session_name,
	attributed.started_at AS group_started_at`
	}
	return `
SELECT
	` + sessionColumn + ` AS session_id,
	u.message_ordinal,
	u.usage_source,
	u.ts,
	u.pricing_ts,
	u.model,
	u.provider_id,
	u.token_usage,
	` + webSearchColumn + ` AS web_search_requests,
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
	` + projectColumn + ` AS project,
	` + agentColumn + ` AS agent` + machineColumn + sessionMetadataColumns + `
FROM (` + rowsSQL + `) u` + metadataJoin + `
WHERE 1=1`
}

type pgUsageBounds struct {
	from string
	to   string
}

func (b pgUsageBounds) bounded() bool {
	return b.from != "" || b.to != ""
}

func pgUsageBoundsForFilter(
	pb *paramBuilder, f db.UsageFilter,
) pgUsageBounds {
	var b pgUsageBounds
	if f.From != "" {
		padded := db.PaddedUTCBound(f.From+"T00:00:00Z", -14)
		b.from = pb.add(padded)
	}
	if f.To != "" {
		padded := db.PaddedUTCBound(f.To+"T23:59:59Z", 14)
		b.to = pb.add(padded)
	}
	return b
}

func appendPGUsageColumnBounds(
	where, col string, b pgUsageBounds,
) string {
	if b.from != "" {
		where += "\n\tAND " + col + " >= " + b.from + "::timestamptz"
	}
	if b.to != "" {
		where += "\n\tAND " + col + " <= " + b.to + "::timestamptz"
	}
	return where
}

func pgDailyUsageRowsSQLForBounds(
	pb *paramBuilder, f db.UsageFilter, b pgUsageBounds,
) string {
	if !b.bounded() {
		messageWhere := appendPGUsageBranchFilterClauses(
			pgUsageMessageEligibility, pb, f, "m.model")
		eventWhere := appendPGUsageBranchFilterClauses(
			pgUsageEventEligibility, pb, f, "ue.model")
		return pgDailyUsageRowsSQLWithWhere(messageWhere, eventWhere)
	}

	return pgBoundedDailyUsageRowsSQL(
		pb, f, b, pgUsageMessageSourceEligibility, pgUsageMessageEligibility)
}

// pgBoundedDailyUsageRowsSQL builds the bounded-branch CTE row source
// shared by pgDailyUsageRowsSQLForBounds (token-eligible rows) and
// pgMatchingUsageRowsSQLForBounds (relaxed matching rows). The two
// callers differ only in the message eligibility predicates.
func pgBoundedDailyUsageRowsSQL(
	pb *paramBuilder, f db.UsageFilter, b pgUsageBounds,
	messageSourceEligibility, messageEligibility string,
) string {
	messageTimestampSourceWhere := messageSourceEligibility +
		"\n\tAND m.timestamp IS NOT NULL"
	messageTimestampSourceWhere = appendPGUsageSourceFilterClauses(
		messageTimestampSourceWhere, pb, f, "m.model")
	messageTimestampSourceWhere = appendPGUsageColumnBounds(
		messageTimestampSourceWhere, "m.timestamp", b)

	eventTimestampSourceWhere := pgUsageEventSourceEligibility +
		"\n\tAND ue.occurred_at IS NOT NULL"
	eventTimestampSourceWhere = appendPGUsageSourceFilterClauses(
		eventTimestampSourceWhere, pb, f, "ue.model")
	eventTimestampSourceWhere = appendPGUsageColumnBounds(
		eventTimestampSourceWhere, "ue.occurred_at", b)

	messageTimestampJoinWhere := appendPGUsageSessionFilterClauses(
		pgUsageSessionEligibility, pb, f)
	eventTimestampJoinWhere := appendPGUsageSessionFilterClauses(
		pgUsageSessionEligibility, pb, f)

	messageFallbackWhere := messageEligibility +
		"\n\tAND m.timestamp IS NULL"
	messageFallbackWhere = appendPGUsageBranchFilterClauses(
		messageFallbackWhere, pb, f, "m.model")
	messageFallbackWhere = appendPGUsageColumnBounds(
		messageFallbackWhere, "s.started_at", b)
	eventFallbackWhere := pgUsageEventEligibility +
		"\n\tAND ue.occurred_at IS NULL"
	eventFallbackWhere = appendPGUsageBranchFilterClauses(
		eventFallbackWhere, pb, f, "ue.model")
	eventFallbackWhere = appendPGUsageColumnBounds(
		eventFallbackWhere, "s.started_at", b)

	return pgDailyUsageRowsSQLWithTimestampCTEs(
		messageTimestampSourceWhere,
		eventTimestampSourceWhere,
		messageTimestampJoinWhere,
		eventTimestampJoinWhere,
		messageFallbackWhere,
		eventFallbackWhere,
	)
}

// pgMatchingUsageRowsSQLForBounds is pgDailyUsageRowsSQLForBounds's
// bounded branch built from the relaxed pgUsageMatchingMessageEligibility
// predicates, so GetUsageMatchingSessionCount only relaxes the token-usage
// and model-presence requirements and keeps the same per-row
// Model/ExcludeModel filtering as the normal bounded path.
func pgMatchingUsageRowsSQLForBounds(
	pb *paramBuilder, f db.UsageFilter, b pgUsageBounds,
) string {
	return pgBoundedDailyUsageRowsSQL(
		pb, f, b,
		pgUsageMatchingMessageSourceEligibility, pgUsageMatchingMessageEligibility)
}

func pgUsageRowQuery(pb *paramBuilder, f db.UsageFilter) string {
	bounds := pgUsageBoundsForFilter(pb, f)
	return pgDailyUsageRowSelectFromRows(pgDailyUsageRowsSQLForBounds(
		pb, f, bounds,
	))
}

const pgDailyCursorUsageRowsSQLTemplate = `
SELECT
	'' AS session_id,
	NULL::INT AS message_ordinal,
	'cursor' AS usage_source,
	cu.occurred_at AS ts,
	cu.occurred_at AS pricing_ts,
	cu.model,
	'' AS provider_id,
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

func pgCursorUsageRowsSQLForBounds(
	pb *paramBuilder, f db.UsageFilter, b pgUsageBounds,
) (string, bool) {
	hasTermFilter := f.Termination != "" && f.Termination != "all"
	// Cursor usage rows carry no project or git branch and bypass the session
	// filter, so any filter they cannot satisfy (project, machine, branch)
	// must exclude them entirely rather than let them leak into totals.
	if len(f.ProjectFilterLabels()) > 0 ||
		len(f.ExcludedProjectFilterLabels()) > 0 ||
		len(db.CSVFilterValues(f.Machine)) > 0 || f.GitBranch != "" || f.MinUserMessages > 0 ||
		f.ExcludeOneShot || hasTermFilter || f.ActiveSince != "" {
		return "", false
	}
	if vals := db.CSVFilterValues(f.Agent); len(vals) > 0 && !slices.Contains(vals, "cursor") {
		return "", false
	}
	if vals := db.CSVFilterValues(f.ExcludeAgent); slices.Contains(vals, "cursor") {
		return "", false
	}

	where := "cu.model != ''"
	scope := db.NormalizeAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	if pred := db.PostgresQueryDialect().AutomatedScopePredicate(scope, "cu.is_headless"); pred != "" {
		where += "\n\tAND " + pred
	}
	where = appendPGUsageSourceFilterClauses(
		where, pb, f, "cu.model",
	)
	where = appendPGUsageColumnBounds(
		where, "cu.occurred_at", b,
	)
	return fmt.Sprintf(pgDailyCursorUsageRowsSQLTemplate, where), true
}

func pgDailyUsageRowQuery(pb *paramBuilder, f db.UsageFilter, hasCursorTable bool) string {
	bounds := pgUsageBoundsForFilter(pb, f)
	rowsSQL := pgDailyUsageRowsSQLForBounds(
		pb, pgUsageSnapshotInputFilter(f), bounds)
	if hasCursorTable {
		cursorRowsSQL, ok := pgCursorUsageRowsSQLForBounds(pb, f, bounds)
		if ok {
			rowsSQL += "\n\nUNION ALL\n\n" + cursorRowsSQL
		}
	}
	rowsSQL = pgSnapshotRankedDailyUsageRowsSQL(pb, rowsSQL, f)
	return pgDailyUsageRowSelectFromSnapshotRowsWithMachine(
		rowsSQL, f.Breakdowns)
}

func pgTopSessionsUsageRowQuery(pb *paramBuilder, f db.UsageFilter, includeSessionGroups bool) string {
	bounds := pgUsageBoundsForFilter(pb, f)
	rowsSQL := pgDailyUsageRowsSQLForBounds(
		pb, pgUsageSnapshotInputFilter(f), bounds)
	rowsSQL = pgSnapshotRankedDailyUsageRowsSQL(pb, rowsSQL, f)
	return pgDailyUsageRowSelectFromRowsWithSession(rowsSQL, false, "u.snapshot_attribution_session_id", true, includeSessionGroups)
}

func pgUsageSnapshotInputFilter(f db.UsageFilter) db.UsageFilter {
	return db.UsageFilter{From: f.From, To: f.To, Timezone: f.Timezone}
}

func pgExactUsageUTCWindow(f db.UsageFilter) (from, to time.Time) {
	loc := f.Location()
	if f.From != "" {
		from, _ = time.ParseInLocation("2006-01-02", f.From, loc)
	}
	if f.To != "" {
		to, _ = time.ParseInLocation("2006-01-02", f.To, loc)
		if !to.IsZero() {
			to = to.AddDate(0, 0, 1)
		}
	}
	return from.UTC(), to.UTC()
}

// pgSnapshotRankedDailyUsageRowsSQL keeps the greatest output snapshot and the
// maximum billed web-search count for each Claude request before rows cross
// into Go. Rows without complete Claude request identity bypass the window.
func pgSnapshotRankedDailyUsageRowsSQL(
	pb *paramBuilder, rowsSQL string, f db.UsageFilter,
) string {
	from, to := pgExactUsageUTCWindow(f)
	where := "TRUE"
	if !from.IsZero() {
		where += "\n\t\t\tAND u.ts >= " + pb.add(from) + "::timestamptz"
	}
	if !to.IsZero() {
		where += "\n\t\t\tAND u.ts < " + pb.add(to) + "::timestamptz"
	}
	filterWhere := appendPGUsageSourceFilterClauses(
		"TRUE", pb, f, "survivor.model")
	filterWhere = appendPGUsageSessionFilterClauses(filterWhere, pb, f)
	outputTokens := fmt.Sprintf(`CASE
				WHEN u.usage_source = 'message' THEN LEAST(GREATEST(COALESCE(
					CAST(agentsview_json_integer(
						u.token_usage, ARRAY['output_tokens'], 'output_tokens') AS NUMERIC),
					0), 0), %[1]d)
				WHEN u.usage_source = 'session'
					THEN GREATEST(u.output_tokens, 0)
				ELSE LEAST(GREATEST(u.output_tokens, 0), %[1]d)
			END`, db.MaxPlausibleTokens)
	webSearchRequests := `CASE
				WHEN u.usage_source = 'message' THEN GREATEST(COALESCE(
					CAST(agentsview_json_integer(
						u.token_usage,
						ARRAY['server_tool_use', 'web_search_requests'],
						'web_search_requests') AS BIGINT),
					0), 0)
				ELSE 0
			END`
	return fmt.Sprintf(`
		WITH usage_snapshot_window AS (
			SELECT u.*, %[1]s AS snapshot_output_tokens,
				%[5]s AS snapshot_row_web_search_requests
			FROM (%[2]s) u
			WHERE %[3]s
		),
		usage_snapshot_ranked AS (
			SELECT usage_snapshot_window.*,
				FIRST_VALUE(session_id) OVER (
					PARTITION BY claude_message_id, claude_request_id
					ORDER BY ts ASC NULLS LAST, session_id ASC,
						COALESCE(message_ordinal, -1) ASC
				) AS snapshot_attribution_session_id,
				ROW_NUMBER() OVER (
					PARTITION BY claude_message_id, claude_request_id
					ORDER BY snapshot_output_tokens DESC, ts DESC NULLS LAST,
						session_id DESC, COALESCE(message_ordinal, -1) DESC
				) AS snapshot_rank,
				MAX(snapshot_row_web_search_requests) OVER (
					PARTITION BY claude_message_id, claude_request_id
				) AS snapshot_web_search_requests
			FROM usage_snapshot_window
			WHERE claude_message_id != '' AND claude_request_id != ''
		),
		usage_snapshot_survivors AS (
			SELECT *
			FROM usage_snapshot_ranked
			WHERE snapshot_rank = 1
			UNION ALL
			SELECT usage_snapshot_window.*,
				session_id AS snapshot_attribution_session_id,
				1 AS snapshot_rank,
				snapshot_row_web_search_requests AS snapshot_web_search_requests
			FROM usage_snapshot_window
			WHERE claude_message_id = '' OR claude_request_id = ''
		)
		SELECT survivor.*
		FROM usage_snapshot_survivors survivor
		LEFT JOIN sessions s
			ON s.id = survivor.snapshot_attribution_session_id
		WHERE survivor.snapshot_attribution_session_id = ''
			OR (%[4]s)`,
		outputTokens, rowsSQL, where, filterWhere, webSearchRequests)
}

func scanPGUsageRow(rows *sql.Rows) (pgUsageScanRow, error) {
	var r pgUsageScanRow
	err := rows.Scan(
		&r.sessionID,
		&r.messageOrdinal,
		&r.usageSource,
		&r.ts,
		&r.pricingTS,
		&r.model,
		&r.providerID,
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
		&r.displayName,
		&r.startedAt,
	)
	return r, err
}

func scanPGDailyUsageRow(rows *sql.Rows) (pgDailyUsageScanRow, error) {
	return scanPGDailyUsageRowWithMachine(rows, false)
}

func scanPGDailyUsageRowWithMachine(
	rows *sql.Rows, includeMachine bool, extra ...any,
) (pgDailyUsageScanRow, error) {
	var r pgDailyUsageScanRow
	dest := []any{
		&r.sessionID,
		&r.messageOrdinal,
		&r.usageSource,
		&r.ts,
		&r.pricingTS,
		&r.model,
		&r.providerID,
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
	err := rows.Scan(append(dest, extra...)...)
	return r, err
}

func pgTokenJSONCount(usage gjson.Result, key string) int {
	return db.ClampPlausibleTokens(usage.Get(key).Int())
}

// pgUsageRowWebSearchRequests returns how many billed Anthropic
// server-side web searches a usage row reports. It mirrors
// db.usageRowWebSearchRequests: only per-message rows carry a usage blob,
// and a negative or absent counter reads as none.
func pgUsageRowWebSearchRequests(usageSource, tokenJSON string) int {
	if usageSource != "message" {
		return 0
	}
	requests := gjson.Get(
		tokenJSON, "server_tool_use.web_search_requests").Int()
	if requests <= 0 {
		return 0
	}
	return int(requests)
}

func pgDailyUsageRowWebSearchRequests(r pgDailyUsageScanRow) int {
	if r.webSearchRequests.Valid {
		return max(int(r.webSearchRequests.Int64), 0)
	}
	return pgUsageRowWebSearchRequests(r.usageSource, r.tokenJSON)
}

// pgUsageLookupModel mirrors internal/db usage pricing: runtime aliases
// resolve to their fixed or timestamp-selected canonical model.
func pgUsageLookupModel(model string, ts sql.NullTime) string {
	var timestamp time.Time
	if ts.Valid {
		timestamp = ts.Time
	}
	if canonical := pricingpkg.CanonicalModelForDate(model, timestamp); canonical != "" {
		return canonical
	}
	return model
}

func pgUsagePricingTimestamp(ts sql.NullTime) time.Time {
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time
}

func pgDailyUsageAmounts(
	r pgDailyUsageScanRow, pricing *export.PricingResolver,
) (
	inputTok, outputTok, cacheCrTok, cacheRdTok int,
	cost, savings money.Money,
	err error,
) {
	inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok := pgDailyUsageRowTokens(r)
	cacheCr1hTok := pgUsageRowCacheCreation1hTokens(
		r.usageSource, r.tokenJSON, cacheCrTok)

	pricedModel, lookup := pricing.ResolveAt(
		r.model, pgUsageLookupModel(r.model, r.pricingTS),
		pgUsagePricingTimestamp(r.pricingTS),
	)
	rates := lookup.Rates
	requestScoped := db.UsageRowIsRequestScoped(r.usageSource, r.messageOrdinal)
	if r.cost.Valid && r.costSource != db.CopilotReportedCostSource {
		cost = money.Money{Microdollars: r.cost.Int64}
		pricing.RecordResolvedReported(r.model, pricedModel, lookup)
	} else {
		_, lookup, err = pricing.ResolveBilledAt(
			r.providerID, r.model, pgUsageLookupModel(r.model, r.pricingTS),
			pgUsagePricingTimestamp(r.pricingTS))
		if err != nil {
			return 0, 0, 0, 0, money.Money{}, money.Money{}, err
		}
		rates = lookup.Rates
		cost, err = rates.CostForTokensScoped(
			requestScoped,
			inputTok, outputTok, reasoningTok, cacheCrTok, cacheCr1hTok,
			cacheRdTok)
		if err != nil {
			return 0, 0, 0, 0, money.Money{}, money.Money{},
				fmt.Errorf("pricing pg usage row for model %q: %w", r.model, err)
		}
		// Anthropic bills server-side web search per request on top of
		// tokens; see db.sessionRowCost for why a reported cost skips it.
		cost, err = export.AddWebSearchFee(
			cost, pgDailyUsageRowWebSearchRequests(r))
		if err != nil {
			return 0, 0, 0, 0, money.Money{}, money.Money{},
				fmt.Errorf("pricing pg usage row for model %q: %w", r.model, err)
		}
		db.RecordComputedUsagePricing(
			pricing, r.model, pricedModel, lookup, requestScoped,
			inputTok, cacheCrTok, cacheRdTok,
		)
	}
	selectedRates := rates
	if requestScoped {
		selectedRates = rates.RatesForTokens(inputTok, cacheCrTok, cacheRdTok)
	}
	savingsRates := selectedRates
	if r.cost.Valid && r.costSource != db.CopilotReportedCostSource &&
		(cacheCrTok != 0 || cacheRdTok != 0) {
		_, savingsLookup, err := pricing.ResolveBilledAt(
			r.providerID, r.model, pgUsageLookupModel(r.model, r.pricingTS),
			pgUsagePricingTimestamp(r.pricingTS))
		if err != nil {
			return 0, 0, 0, 0, money.Money{}, money.Money{},
				fmt.Errorf("pricing pg reported usage cache savings for model %q: %w", r.model, err)
		}
		savingsRates = savingsLookup.Rates
		if requestScoped {
			savingsRates = savingsRates.RatesForTokens(inputTok, cacheCrTok, cacheRdTok)
		}
	}
	readRate, err := money.Sub(
		savingsRates.InputPerMTok, savingsRates.CacheReadPerMTok)
	if err != nil {
		return 0, 0, 0, 0, money.Money{}, money.Money{},
			fmt.Errorf("deriving pg cache read rate for model %q: %w", r.model, err)
	}
	creationRate, err := money.Sub(
		savingsRates.InputPerMTok, savingsRates.CacheWritePerMTok)
	if err != nil {
		return 0, 0, 0, 0, money.Money{}, money.Money{},
			fmt.Errorf("deriving pg cache creation rate for model %q: %w", r.model, err)
	}
	creation1hRate, err := money.Sub(
		savingsRates.InputPerMTok, savingsRates.EffectiveCacheWrite1hPerMTok())
	if err != nil {
		return 0, 0, 0, 0, money.Money{}, money.Money{},
			fmt.Errorf("deriving pg 1h cache creation rate for model %q: %w", r.model, err)
	}
	savings, err = money.SignedCostPerMillion([]money.RatedTokens{
		{Tokens: int64(cacheRdTok), Rate: readRate},
		{Tokens: int64(cacheCrTok - cacheCr1hTok), Rate: creationRate},
		{Tokens: int64(cacheCr1hTok), Rate: creation1hRate},
	})
	if err != nil {
		return 0, 0, 0, 0, money.Money{}, money.Money{},
			fmt.Errorf("pricing pg cache savings for model %q: %w", r.model, err)
	}
	return inputTok, outputTok, cacheCrTok, cacheRdTok, cost, savings, nil
}

func pgDailyUsageRowTokens(
	r pgDailyUsageScanRow,
) (inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok int) {
	reasoningTok = r.reasoningTokens
	if r.usageSource == "message" {
		usage := gjson.Parse(r.tokenJSON)
		inputTok = pgTokenJSONCount(usage, "input_tokens")
		outputTok = pgTokenJSONCount(usage, "output_tokens")
		cacheCrTok = pgTokenJSONCount(
			usage, "cache_creation_input_tokens")
		cacheRdTok = pgTokenJSONCount(usage, "cache_read_input_tokens")
		reasoningTok = pgTokenJSONCount(usage, "reasoning_tokens")
	} else {
		inputTok, outputTok, cacheCrTok, cacheRdTok = db.UsageEventRowTokens(
			r.usageSource,
			r.inputTokens, r.outputTokens,
			r.cacheCreationInputTokens, r.cacheReadInputTokens)
	}
	return
}

// pgUsageRowCacheCreation1hTokens returns the clamped 1h-TTL subset of a
// message row's cache-write tokens from the nested cache_creation
// breakdown. Usage events never carry the breakdown. It mirrors
// db.clampedCacheCreation1hTokens: the flat counter stays authoritative,
// so the subset never exceeds it.
func pgUsageRowCacheCreation1hTokens(
	usageSource, tokenJSON string, cacheCrTok int,
) int {
	if usageSource != "message" {
		return 0
	}
	cr1h := pgTokenJSONCount(
		gjson.Parse(tokenJSON),
		"cache_creation.ephemeral_1h_input_tokens")
	return min(cr1h, cacheCrTok)
}

func pgSessionRowCost(
	r pgUsageScanRow, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	return pgSessionRowCostWithWebSearchRequests(
		r, pgUsageRowWebSearchRequests(r.usageSource, r.tokenJSON), pricing)
}

func pgSessionRowCostWithWebSearchRequests(
	r pgUsageScanRow, webSearches int, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	var inTok, outTok, crTok, rdTok int
	reasoningTok := r.reasoningTokens
	if r.usageSource == "message" {
		usage := gjson.Parse(r.tokenJSON)
		inTok = pgTokenJSONCount(usage, "input_tokens")
		outTok = pgTokenJSONCount(usage, "output_tokens")
		crTok = pgTokenJSONCount(usage, "cache_creation_input_tokens")
		rdTok = pgTokenJSONCount(usage, "cache_read_input_tokens")
		reasoningTok = pgTokenJSONCount(usage, "reasoning_tokens")
	} else {
		inTok, outTok, crTok, rdTok = db.UsageEventRowTokens(
			r.usageSource,
			r.inputTokens, r.outputTokens,
			r.cacheCreationInputTokens, r.cacheReadInputTokens)
	}
	cr1hTok := pgUsageRowCacheCreation1hTokens(
		r.usageSource, r.tokenJSON, crTok)
	pricedModel, lookup := pricing.ResolveAt(
		r.model, pgUsageLookupModel(r.model, r.pricingTS),
		pgUsagePricingTimestamp(r.pricingTS),
	)
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
	pricedModel, lookup, err = pricing.ResolveBilledAt(
		r.providerID, r.model, pgUsageLookupModel(r.model, r.pricingTS),
		pgUsagePricingTimestamp(r.pricingTS))
	if err != nil {
		return money.Money{}, false, false, err
	}
	requestScoped := db.UsageRowIsRequestScoped(r.usageSource, r.messageOrdinal)
	cost, err = lookup.Rates.CostForTokensScoped(
		requestScoped,
		inTok, outTok, reasoningTok, crTok, cr1hTok, rdTok)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing pg session usage for model %q: %w", r.model, err)
	}
	cost, err = export.AddWebSearchFee(cost, webSearches)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing pg session usage for model %q: %w", r.model, err)
	}
	db.RecordComputedUsagePricing(
		pricing, r.model, pricedModel, lookup,
		requestScoped, inTok, crTok, rdTok)
	return cost, true, true, nil
}

func pgSessionUsageBreakdownEntry(
	r pgUsageScanRow,
	ordinal int,
	cost money.Money,
	priced bool,
) db.SessionUsageBreakdownEntry {
	return pgSessionUsageBreakdownEntryWithWebSearchRequests(
		r, ordinal, cost, priced,
		pgUsageRowWebSearchRequests(r.usageSource, r.tokenJSON))
}

func pgSessionUsageBreakdownEntryWithWebSearchRequests(
	r pgUsageScanRow,
	ordinal int,
	cost money.Money,
	priced bool,
	webSearches int,
) db.SessionUsageBreakdownEntry {
	var inTok, outTok, crTok, rdTok int
	if r.usageSource == "message" {
		usage := gjson.Parse(r.tokenJSON)
		inTok = pgTokenJSONCount(usage, "input_tokens")
		outTok = pgTokenJSONCount(usage, "output_tokens")
		crTok = pgTokenJSONCount(usage, "cache_creation_input_tokens")
		rdTok = pgTokenJSONCount(usage, "cache_read_input_tokens")
	} else {
		inTok, outTok, crTok, rdTok = db.UsageEventRowTokens(
			r.usageSource,
			r.inputTokens, r.outputTokens,
			r.cacheCreationInputTokens, r.cacheReadInputTokens)
	}
	entry := db.SessionUsageBreakdownEntry{
		Ordinal:                  ordinal,
		Source:                   r.usageSource,
		Label:                    pgSessionUsageBreakdownLabel(r),
		Timestamp:                startedAtString(r.ts),
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

func pgSessionUsageBreakdownLabel(r pgUsageScanRow) string {
	return db.SessionUsageBreakdownLabel(
		db.NullInt64Pointer(r.messageOrdinal), r.usageSource)
}

func usageDate(ts sql.NullTime, loc *time.Location) string {
	if !ts.Valid {
		return ""
	}
	return ts.Time.In(loc).Format("2006-01-02")
}

func startedAtString(ts sql.NullTime) string {
	if !ts.Valid {
		return ""
	}
	return FormatISO8601(ts.Time)
}

func (s *Store) loadPGTopSessionMetadata(
	ctx context.Context, sessionIDs []string,
) (map[string]pgTopSessionMetadata, error) {
	out := make(map[string]pgTopSessionMetadata, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return out, nil
	}

	pb := &paramBuilder{}
	placeholders := make([]string, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		placeholders = append(placeholders, pb.add(id))
	}
	query := `
SELECT
	id,
	COALESCE(NULLIF(COALESCE(display_name, session_name), ''), NULLIF(first_message, ''), NULLIF(project, ''), id) AS display_name,
	agent,
	project,
	started_at,
	machine,
	group_key,
	COALESCE(session_name, '')
FROM sessions
WHERE id IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("querying pg top session metadata: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var meta pgTopSessionMetadata
		var startedAt sql.NullTime
		if err := rows.Scan(
			&id,
			&meta.displayName,
			&meta.agent,
			&meta.project,
			&startedAt,
			&meta.machine,
			&meta.groupKey,
			&meta.sessionName,
		); err != nil {
			return nil,
				fmt.Errorf("scanning pg top session metadata: %w", err)
		}
		meta.startedAt = startedAtString(startedAt)
		out[id] = meta
	}
	if err := rows.Err(); err != nil {
		return nil,
			fmt.Errorf("iterating pg top session metadata: %w", err)
	}
	return out, nil
}

// GetSessionUsage returns one session's token totals and cost
// estimate from the PostgreSQL session store. BreakdownCount is
// always populated; per-row Breakdown entries are built only when
// includeBreakdown is true.
func (s *Store) GetSessionUsage(
	ctx context.Context, sessionID string, includeBreakdown bool,
) (*db.SessionUsage, error) {
	sess, err := s.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, nil
	}

	pricing, err := s.loadPricingMap(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading pg pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)

	pb := &paramBuilder{}
	query := pgUsageRowSelect() + " AND u.session_id = " +
		pb.add(sessionID) + ` ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC,
		u.usage_source ASC,
		COALESCE(u.usage_dedup_key, '') ASC`
	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("querying pg session usage: %w", err)
	}
	defer rows.Close()

	var cost money.Money
	var authoritativeCost *money.Money
	var hasComputedCost, hasReportedCost bool
	contributing := false
	allPriced := true
	modelsSet := make(map[string]struct{})
	unpricedSet := make(map[string]struct{})
	breakdown := make([]db.SessionUsageBreakdownEntry, 0)
	breakdownCount := 0

	var usageRows []pgUsageScanRow
	for rows.Next() {
		r, scanErr := scanPGUsageRow(rows)
		if scanErr != nil {
			return nil,
				fmt.Errorf("scanning pg session usage row: %w", scanErr)
		}
		usageRows = append(usageRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pg session usage rows: %w", err)
	}
	snapshotRows := make([]activity.UsageRow, len(usageRows))
	for i, r := range usageRows {
		var outputTokens int
		if r.usageSource == "message" {
			outputTokens = pgTokenJSONCount(
				gjson.Parse(r.tokenJSON), "output_tokens")
		} else {
			_, outputTokens, _, _ = db.UsageEventRowTokens(
				r.usageSource,
				r.inputTokens, r.outputTokens,
				r.cacheCreationInputTokens, r.cacheReadInputTokens)
		}
		snapshotRows[i] = activity.UsageRow{
			SessionID:      r.sessionID,
			Timestamp:      startedAtString(r.ts),
			MessageOrdinal: db.UsageRowMessageOrdinal(r.messageOrdinal),
			OutputTokens:   outputTokens,
			WebSearchRequests: pgUsageRowWebSearchRequests(
				r.usageSource, r.tokenJSON),
			ClaudeMessageID: r.claudeMessageID,
			ClaudeRequestID: r.claudeRequestID,
		}
	}
	snapshotMask, _, snapshotWebSearchRequests := activity.ClaudeSnapshotSurvivorSelection(snapshotRows)
	deduplicatedOutputTokens := 0
	seen := make(map[db.UsageDedupToken]struct{})
	for i, r := range usageRows {
		if !snapshotMask[i] {
			deduplicatedOutputTokens += snapshotRows[i].OutputTokens
			continue
		}
		if key, ok := db.UsageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		costRow := r
		authoritative := r.costSource == db.CopilotReportedCostSource && r.cost.Valid
		if authoritative {
			v := money.Money{Microdollars: r.cost.Int64}
			authoritativeCost = &v
			costRow.cost = sql.NullInt64{}
		}
		c, priced, contributes, priceErr := pgSessionRowCostWithWebSearchRequests(
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
				return nil, fmt.Errorf("summing pg session usage cost: %w", priceErr)
			}
		} else {
			allPriced = false
			unpricedSet[r.model] = struct{}{}
		}
		breakdownCount++
		if includeBreakdown {
			breakdown = append(breakdown,
				pgSessionUsageBreakdownEntryWithWebSearchRequests(
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

	out := &db.SessionUsage{
		SessionID:         sess.ID,
		Agent:             sess.Agent,
		Project:           sess.Project,
		TotalOutputTokens: max(sess.TotalOutputTokens-deduplicatedOutputTokens, 0),
		PeakContextTokens: sess.PeakContextTokens,
		HasTokenData: sess.HasTotalOutputTokens ||
			sess.HasPeakContextTokens,
		Models:         db.SortedKeys(modelsSet),
		HasCost:        authoritativeCost != nil || (contributing && allPriced),
		BreakdownCount: breakdownCount,
		Breakdown:      breakdown,
	}
	if out.HasCost {
		if authoritativeCost != nil {
			out.Cost = *authoritativeCost
			out.CostSource = export.CostSourceReported
		} else {
			out.Cost = cost
			out.CostSource = export.CombinedCostSource(hasComputedCost, hasReportedCost)
		}
		out.AICredits = db.AICreditsFromCost(sess.Agent, out.Cost)
	}
	out.CostUSD = db.CostUSDFromCost(out.HasCost, out.Cost)
	if len(unpricedSet) > 0 {
		out.UnpricedModels = db.SortedKeys(unpricedSet)
	}
	return out, nil
}

// GetDailyUsage returns token usage and cost aggregated by day.
func (s *Store) GetDailyUsage(
	ctx context.Context, f db.UsageFilter,
) (db.DailyUsageResult, error) {
	loc := f.Location()

	pricing, err := s.loadPricingMap(ctx)
	if err != nil {
		return db.DailyUsageResult{},
			fmt.Errorf("loading pg pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)

	pb := &paramBuilder{}
	query := pgDailyUsageRowQuery(pb, f, pgHasTable(ctx, s.pg, "cursor_usage_events"))
	query += ` ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC`

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return db.DailyUsageResult{},
			fmt.Errorf("querying daily usage: %w", err)
	}
	defer rows.Close()

	type accumKey struct {
		date       string
		project    string
		agent      string
		machine    string
		model      string
		providerID string
	}
	type bucket struct {
		inputTok  int
		outputTok int
		cacheCr   int
		cacheRd   int
		cost      money.Money
	}
	type sessionCost struct {
		estimated     map[accumKey]money.Money
		authoritative *money.Money
	}
	accum := make(map[accumKey]*bucket)
	sessionCosts := make(map[string]sessionCost)
	useAuthoritativeCost := !f.HasModelFilter()
	seen := make(map[db.UsageDedupToken]struct{})
	var seenSessions map[string]db.UsageSessionInfo
	if !f.SkipSessionCounts {
		seenSessions = make(map[string]db.UsageSessionInfo)
	}
	projectLabels := make(map[string]struct{})
	var totalSavings money.Money

	for rows.Next() {
		r, scanErr := scanPGDailyUsageRowWithMachine(rows, f.Breakdowns)
		if scanErr != nil {
			return db.DailyUsageResult{},
				fmt.Errorf("scanning daily usage row: %w", scanErr)
		}

		date := usageDate(r.ts, loc)
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}
		if key, ok := db.UsageDedupTokenForRow(
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
				seenSessions[r.sessionID] = db.UsageSessionInfo{
					Project: r.project,
					Agent:   r.agent,
				}
			}
		}
		if r.project != "" {
			projectLabels[r.project] = struct{}{}
		}

		inputTok, outputTok, cacheCrTok, cacheRdTok, cost, savings, priceErr := pgDailyUsageAmounts(r, rateResolver)
		if priceErr != nil {
			return db.DailyUsageResult{}, priceErr
		}
		totalSavings, priceErr = money.Add(totalSavings, savings)
		if priceErr != nil {
			return db.DailyUsageResult{}, fmt.Errorf(
				"summing pg daily usage cache savings: %w", priceErr)
		}

		key := accumKey{
			date: date, project: r.project,
			agent: r.agent, machine: r.machine, model: r.model,
			providerID: r.providerID,
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
			sc.estimated = make(map[accumKey]money.Money)
		}
		sc.estimated[key], priceErr = money.Add(sc.estimated[key], cost)
		if priceErr != nil {
			return db.DailyUsageResult{}, fmt.Errorf(
				"summing pg daily usage session cost: %w", priceErr)
		}
		if useAuthoritativeCost &&
			r.costSource == db.CopilotReportedCostSource && r.cost.Valid {
			v := money.Money{Microdollars: r.cost.Int64}
			sc.authoritative = &v
			rateResolver.RecordUnattributedReported()
		}
		sessionCosts[r.sessionID] = sc
	}
	if err := rows.Err(); err != nil {
		return db.DailyUsageResult{},
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
			keys := make([]accumKey, 0, len(sc.estimated))
			for key := range sc.estimated {
				keys = append(keys, key)
			}
			sort.Slice(keys, func(i, j int) bool {
				a, b := keys[i], keys[j]
				if a.date != b.date {
					return a.date < b.date
				}
				if a.project != b.project {
					return a.project < b.project
				}
				if a.agent != b.agent {
					return a.agent < b.agent
				}
				if a.machine != b.machine {
					return a.machine < b.machine
				}
				if a.providerID != b.providerID {
					return a.providerID < b.providerID
				}
				return a.model < b.model
			})
			weights := make([]money.Money, len(keys))
			for i, key := range keys {
				weights[i] = sc.estimated[key]
			}
			costs := export.AllocateCostByWeight(*sc.authoritative, weights)
			for i, key := range keys {
				b := accum[key]
				if b == nil {
					b = &bucket{}
					accum[key] = b
				}
				b.cost, err = money.Add(b.cost, costs[i])
				if err != nil {
					return db.DailyUsageResult{}, fmt.Errorf(
						"summing allocated pg daily usage cost: %w", err)
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
					return db.DailyUsageResult{}, fmt.Errorf(
						"summing estimated pg daily usage cost: %w", err)
				}
			}
		}
	}

	if !f.Breakdowns {
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
				return db.DailyUsageResult{}, fmt.Errorf(
					"summing pg daily model cost: %w", err)
			}
		}

		type dayData struct{ models map[string]*modelAccum }
		days := make(map[string]*dayData)
		for key, ma := range dm {
			dd, ok := days[key.date]
			if !ok {
				dd = &dayData{models: make(map[string]*modelAccum)}
				days[key.date] = dd
			}
			dd.models[key.model] = ma
		}

		dateKeys := make([]string, 0, len(days))
		for d := range days {
			dateKeys = append(dateKeys, d)
		}
		sort.Strings(dateKeys)

		daily := make([]db.DailyUsageEntry, 0, len(dateKeys))
		var totals db.UsageTotals
		for _, date := range dateKeys {
			dd := days[date]
			if dd == nil {
				continue
			}
			var entry db.DailyUsageEntry
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
				if left.cost.Microdollars != right.cost.Microdollars {
					return left.cost.Microdollars > right.cost.Microdollars
				}
				return modelNames[i] < modelNames[j]
			})
			entry.ModelsUsed = modelNames
			mbd := make([]db.ModelBreakdown, 0, len(modelNames))
			for _, m := range modelNames {
				ma := dd.models[m]
				if ma == nil {
					continue
				}
				entry.InputTokens += ma.inputTok
				entry.OutputTokens += ma.outputTok
				entry.CacheCreationTokens += ma.cacheCr
				entry.CacheReadTokens += ma.cacheRd
				entry.TotalCost, err = money.Add(entry.TotalCost, ma.cost)
				if err != nil {
					return db.DailyUsageResult{}, fmt.Errorf(
						"summing pg daily entry cost: %w", err)
				}
				mbd = append(mbd, db.ModelBreakdown{
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
				return db.DailyUsageResult{}, fmt.Errorf(
					"summing pg daily usage total: %w", err)
			}
		}
		if daily == nil {
			daily = []db.DailyUsageEntry{}
		}
		totals.CacheSavings = totalSavings

		var aiCredits float64
		for key, b := range accum {
			aiCredits += db.AICreditsFromCost(key.agent, b.cost)
		}
		if aiCredits > 0 {
			totals.CopilotAICredits = aiCredits
		}

		var sessionCounts db.UsageSessionCounts
		if seenSessions != nil {
			sessionCounts = db.NewUsageSessionCounts(seenSessions)
		}
		projects, err := s.BuildProjectIdentityMap(ctx,
			db.SortedKeys(projectLabels))
		if err != nil {
			return db.DailyUsageResult{}, err
		}
		projectRows := db.DailyUsageResult{Daily: daily, SessionCounts: sessionCounts}
		db.SanitizeDailyUsageProjectLabelsWithCatalog(&projectRows, projects)
		daily = projectRows.Daily
		sessionCounts = projectRows.SessionCounts
		pricingBlock, err := rateResolver.BuildBlock()
		if err != nil {
			return db.DailyUsageResult{}, fmt.Errorf(
				"building pricing block: %w", err)
		}
		return db.DailyUsageResult{
			SchemaVersion: export.UsageDailySchemaVersion,
			Pricing:       &pricingBlock,
			Projects:      export.ProjectMapForWire(projects),
			Daily:         daily,
			Totals:        totals,
			SessionCounts: sessionCounts,
		}, nil
	}

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
			return db.DailyUsageResult{}, fmt.Errorf(
				"summing pg model breakdown cost: %w", err)
		}
		dm.models[key.model] = cur

		cur = dm.projects[key.project]
		cur.inputTok += b.inputTok
		cur.outputTok += b.outputTok
		cur.cacheCr += b.cacheCr
		cur.cacheRd += b.cacheRd
		cur.cost, err = money.Add(cur.cost, b.cost)
		if err != nil {
			return db.DailyUsageResult{}, fmt.Errorf(
				"summing pg project breakdown cost: %w", err)
		}
		dm.projects[key.project] = cur

		cur = dm.agents[key.agent]
		cur.inputTok += b.inputTok
		cur.outputTok += b.outputTok
		cur.cacheCr += b.cacheCr
		cur.cacheRd += b.cacheRd
		cur.cost, err = money.Add(cur.cost, b.cost)
		if err != nil {
			return db.DailyUsageResult{}, fmt.Errorf(
				"summing pg agent breakdown cost: %w", err)
		}
		dm.agents[key.agent] = cur

		cur = dm.machines[key.machine]
		cur.inputTok += b.inputTok
		cur.outputTok += b.outputTok
		cur.cacheCr += b.cacheCr
		cur.cacheRd += b.cacheRd
		cur.cost, err = money.Add(cur.cost, b.cost)
		if err != nil {
			return db.DailyUsageResult{}, fmt.Errorf(
				"summing pg machine breakdown cost: %w", err)
		}
		dm.machines[key.machine] = cur
	}

	dateKeys := make([]string, 0, len(days))
	for d := range days {
		dateKeys = append(dateKeys, d)
	}
	sort.Strings(dateKeys)

	daily := make([]db.DailyUsageEntry, 0, len(dateKeys))
	var totals db.UsageTotals
	for _, date := range dateKeys {
		dm := days[date]
		if dm == nil {
			continue
		}
		var entry db.DailyUsageEntry
		entry.Date = date

		modelNames := make([]string, 0, len(dm.models))
		for m := range dm.models {
			modelNames = append(modelNames, m)
		}
		sort.Slice(modelNames, func(i, j int) bool {
			left := dm.models[modelNames[i]]
			right := dm.models[modelNames[j]]
			if left.cost.Microdollars != right.cost.Microdollars {
				return left.cost.Microdollars > right.cost.Microdollars
			}
			return modelNames[i] < modelNames[j]
		})
		entry.ModelsUsed = modelNames
		mbd := make([]db.ModelBreakdown, 0, len(modelNames))
		for _, m := range modelNames {
			b := dm.models[m]
			entry.InputTokens += b.inputTok
			entry.OutputTokens += b.outputTok
			entry.CacheCreationTokens += b.cacheCr
			entry.CacheReadTokens += b.cacheRd
			entry.TotalCost, err = money.Add(entry.TotalCost, b.cost)
			if err != nil {
				return db.DailyUsageResult{}, fmt.Errorf(
					"summing pg breakdown entry cost: %w", err)
			}
			mbd = append(mbd, db.ModelBreakdown{
				ModelName:           m,
				InputTokens:         b.inputTok,
				OutputTokens:        b.outputTok,
				CacheCreationTokens: b.cacheCr,
				CacheReadTokens:     b.cacheRd,
				Cost:                b.cost,
			})
		}
		entry.ModelBreakdowns = mbd

		pbd := make([]db.ProjectBreakdown, 0, len(dm.projects))
		for p, b := range dm.projects {
			pbd = append(pbd, db.ProjectBreakdown{
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

		abd := make([]db.AgentBreakdown, 0, len(dm.agents))
		for a, b := range dm.agents {
			abd = append(abd, db.AgentBreakdown{
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
			[]db.MachineBreakdown, 0, len(dm.machines),
		)
		for machine, b := range dm.machines {
			machineBreakdowns = append(machineBreakdowns, db.MachineBreakdown{
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
			return db.DailyUsageResult{}, fmt.Errorf(
				"summing pg breakdown total: %w", err)
		}
	}

	if daily == nil {
		daily = []db.DailyUsageEntry{}
	}
	totals.CacheSavings = totalSavings

	var aiCredits float64
	for _, d := range daily {
		for _, ab := range d.AgentBreakdowns {
			aiCredits += db.AICreditsFromCost(ab.Agent, ab.Cost)
		}
	}
	if aiCredits > 0 {
		totals.CopilotAICredits = aiCredits
	}

	var sessionCounts db.UsageSessionCounts
	if seenSessions != nil {
		sessionCounts = db.NewUsageSessionCounts(seenSessions)
	}
	projects, err := s.BuildProjectIdentityMap(ctx,
		db.SortedKeys(projectLabels))
	if err != nil {
		return db.DailyUsageResult{}, err
	}
	projectRows := db.DailyUsageResult{Daily: daily, SessionCounts: sessionCounts}
	db.SanitizeDailyUsageProjectLabelsWithCatalog(&projectRows, projects)
	daily = projectRows.Daily
	sessionCounts = projectRows.SessionCounts
	pricingBlock, err := rateResolver.BuildBlock()
	if err != nil {
		return db.DailyUsageResult{}, fmt.Errorf(
			"building pricing block: %w", err)
	}
	return db.DailyUsageResult{
		SchemaVersion: export.UsageDailySchemaVersion,
		Pricing:       &pricingBlock,
		Projects:      export.ProjectMapForWire(projects),
		Daily:         daily,
		Totals:        totals,
		SessionCounts: sessionCounts,
	}, nil
}

// GetTopSessionsByCost returns sessions ranked by total cost.
func (s *Store) GetTopSessionsByCost(
	ctx context.Context, f db.UsageFilter, limit int,
) ([]db.TopSessionEntry, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	pricing, err := s.loadPricingMap(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading pg pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)

	pb := &paramBuilder{}
	query := pgTopSessionsUsageRowQuery(pb, f, f.TopSessionsByGroup)
	query += ` ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC`

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("querying top sessions: %w", err)
	}
	defer rows.Close()

	loc := f.Location()
	type sessAccum struct {
		inputTokens       int
		outputTokens      int
		cacheCreateTokens int
		cacheReadTokens   int
		totalTokens       int
		cost              money.Money
		authoritativeCost *money.Money
		entry             db.TopSessionEntry
	}

	accum := make(map[string]*sessAccum)
	var order []string
	seen := make(map[db.UsageDedupToken]struct{})

	for rows.Next() {
		var entry db.TopSessionEntry
		var startedAt sql.NullTime
		var groupColumns []any
		if f.TopSessionsByGroup {
			groupColumns = []any{&entry.Machine, &entry.GroupKey, &entry.SessionName, &startedAt}
		}
		r, err := scanPGDailyUsageRowWithMachine(rows, false, groupColumns...)
		entry.Project, entry.Agent = r.project, r.agent
		entry.StartedAt = startedAtString(startedAt)
		if err != nil {
			return nil,
				fmt.Errorf("scanning top sessions row: %w", err)
		}

		date := usageDate(r.ts, loc)
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}
		if key, ok := db.UsageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		inputTok, outputTok, cacheCrTok, cacheRdTok, cost, _, priceErr := pgDailyUsageAmounts(r, rateResolver)
		if priceErr != nil {
			return nil, priceErr
		}

		sa, ok := accum[r.sessionID]
		if !ok {
			sa = &sessAccum{entry: entry}
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
			return nil, fmt.Errorf("summing pg top-session cost: %w", priceErr)
		}
		if !f.HasModelFilter() &&
			r.costSource == db.CopilotReportedCostSource && r.cost.Valid {
			v := money.Money{Microdollars: r.cost.Int64}
			sa.authoritativeCost = &v
		}
	}
	if err := rows.Err(); err != nil {
		return nil,
			fmt.Errorf("iterating top sessions rows: %w", err)
	}
	result := make([]db.TopSessionEntry, 0, len(order))
	for _, id := range order {
		sa := accum[id]
		if sa == nil {
			continue
		}
		result = append(result, db.TopSessionEntry{
			SessionID: id, DisplayName: id,
			Project: sa.entry.Project, Agent: sa.entry.Agent,
			GroupKey: sa.entry.GroupKey, SessionName: sa.entry.SessionName, Machine: sa.entry.Machine,
			StartedAt:           sa.entry.StartedAt,
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

	if f.TopSessionsByGroup {
		result, err = db.GroupTopSessions(result, limit, f.TopSessionsSort, f.TopSessionsTokenTypes)
		if err != nil {
			return nil, err
		}
	} else {
		result = db.SortAndLimitTopSessions(result, limit, f.TopSessionsSort, f.TopSessionsTokenTypes)
	}

	sessionIDs := make([]string, 0, len(result))
	for _, entry := range result {
		if entry.SessionID != "" {
			sessionIDs = append(sessionIDs, entry.SessionID)
		}
	}
	metadata, err := s.loadPGTopSessionMetadata(ctx, sessionIDs)
	if err != nil {
		return nil, err
	}
	for i := range result {
		if meta, ok := metadata[result[i].SessionID]; ok {
			result[i].DisplayName = meta.displayName
			result[i].Agent = meta.agent
			result[i].Project = meta.project
			result[i].StartedAt = meta.startedAt
			result[i].Machine = meta.machine
			result[i].GroupKey = meta.groupKey
			result[i].SessionName = meta.sessionName
		}
	}

	return result, nil
}

// GetUsageSessionCounts returns distinct session counts grouped by project and agent.
func (s *Store) GetUsageSessionCounts(
	ctx context.Context, f db.UsageFilter,
) (db.UsageSessionCounts, error) {
	pb := &paramBuilder{}
	query := pgTopSessionsUsageRowQuery(pb, f, false)
	query += ` ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC`

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return db.UsageSessionCounts{},
			fmt.Errorf("querying session counts: %w", err)
	}
	defer rows.Close()

	loc := f.Location()
	type sessInfo struct {
		project string
		agent   string
	}

	seen := make(map[string]sessInfo)
	dedup := make(map[db.UsageDedupToken]struct{})

	for rows.Next() {
		r, err := scanPGDailyUsageRow(rows)
		if err != nil {
			return db.UsageSessionCounts{},
				fmt.Errorf("scanning session counts: %w", err)
		}

		date := usageDate(r.ts, loc)
		if f.From != "" && date < f.From {
			continue
		}
		if f.To != "" && date > f.To {
			continue
		}

		if key, ok := db.UsageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := dedup[key]; dup {
				continue
			}
			dedup[key] = struct{}{}
		}

		if _, ok := seen[r.sessionID]; !ok {
			seen[r.sessionID] = sessInfo{project: r.project, agent: r.agent}
		}
	}
	if err := rows.Err(); err != nil {
		return db.UsageSessionCounts{},
			fmt.Errorf("iterating session counts: %w", err)
	}

	out := db.UsageSessionCounts{
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

// appendPGUsageMatchingActivityClauses requires the session to have at
// least one row that GetUsageMatchingSessionCount's bounded branch would
// count, mirroring appendUsageMatchingActivityClauses in internal/db so
// bounded and unbounded requests agree on which sessions match.
func appendPGUsageMatchingActivityClauses(
	where string, pb *paramBuilder, f db.UsageFilter,
) string {
	messageWhere := appendPGUsageSourceFilterClauses(
		pgUsageMatchingMessageSourceEligibility, pb, f, "m.model",
	)
	eventWhere := appendPGUsageSourceFilterClauses(
		pgUsageEventSourceEligibility, pb, f, "ue.model",
	)

	return where + `
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
}

// GetUsageMatchingSessionCount counts sessions that match the usage filter
// even when they have no token-bearing usage rows. Bounded ranges are
// resolved against the timestamps of the sessions' messages/usage_events
// rows (falling back to s.started_at for rows with no timestamp of their
// own), the same shape pgDailyUsageRowsSQLForBounds uses, so a session
// whose started_at/ended_at fall outside the window but whose message
// activity falls inside it is still counted.
func (s *Store) GetUsageMatchingSessionCount(
	ctx context.Context, f db.UsageFilter,
) (int, error) {
	pb := &paramBuilder{}

	if f.From == "" && f.To == "" {
		where := appendPGUsageSessionFilterClauses(pgUsageSessionEligibility, pb, f)
		where = appendPGUsageMatchingActivityClauses(where, pb, f)

		var count int
		err := s.pg.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM sessions s
WHERE `+where, pb.args...).Scan(&count)
		if err != nil {
			return 0, fmt.Errorf("querying matching usage sessions: %w", err)
		}
		return count, nil
	}

	bounds := pgUsageBoundsForFilter(pb, f)
	rowsSQL := pgMatchingUsageRowsSQLForBounds(pb, f, bounds)

	rows, err := s.pg.QueryContext(
		ctx, pgDailyUsageRowSelectFromRows(rowsSQL), pb.args...)
	if err != nil {
		return 0, fmt.Errorf("querying matching usage sessions: %w", err)
	}
	defer rows.Close()

	loc := f.Location()
	seen := make(map[string]struct{})
	for rows.Next() {
		r, err := scanPGDailyUsageRow(rows)
		if err != nil {
			return 0, fmt.Errorf("scanning matching usage session: %w", err)
		}
		date := usageDate(r.ts, loc)
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
