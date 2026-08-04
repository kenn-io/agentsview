package postgres

import (
	"database/sql"
	"fmt"
	"slices"
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

func usageLocation(f db.UsageFilter) *time.Location {
	if f.Timezone == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(f.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}

func paddedUTCBound(ts string, hours int) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Add(time.Duration(hours) * time.Hour).Format(time.RFC3339)
}

func appendPGUsageBranchFilterClauses(
	where string, pb *paramBuilder, f db.UsageFilter, modelCol string,
) string {
	where = appendPGUsageSourceFilterClauses(where, pb, f, modelCol)
	return appendPGUsageSessionFilterClauses(where, pb, f)
}

func appendPGUsageSourceFilterClauses(
	where string, pb *paramBuilder, f db.UsageFilter, modelCol string,
) string {
	appendCSV := func(q, col, csv string, include bool) string {
		if csv == "" {
			return q
		}
		vals := strings.Split(csv, ",")
		op := "IN"
		if !include {
			op = "NOT IN"
		}
		if len(vals) == 1 {
			if include {
				return q + "\n\tAND " + col + " = " + pb.add(vals[0])
			}
			return q + "\n\tAND " + col + " != " + pb.add(vals[0])
		}
		placeholders := make([]string, len(vals))
		for i, v := range vals {
			placeholders[i] = pb.add(v)
		}
		return q + "\n\tAND " + col + " " + op + " (" +
			strings.Join(placeholders, ",") + ")"
	}

	where = appendCSV(where, modelCol, f.Model, true)
	where = appendCSV(where, modelCol, f.ExcludeModel, false)

	return where
}

func appendPGUsageSessionFilterClauses(
	where string, pb *paramBuilder, f db.UsageFilter,
) string {
	appendValues := func(q, col string, vals []string, include bool) string {
		if len(vals) == 0 {
			return q
		}
		op := "IN"
		if !include {
			op = "NOT IN"
		}
		if len(vals) == 1 {
			if include {
				return q + "\n\tAND " + col + " = " + pb.add(vals[0])
			}
			return q + "\n\tAND " + col + " != " + pb.add(vals[0])
		}
		placeholders := make([]string, len(vals))
		for i, v := range vals {
			placeholders[i] = pb.add(v)
		}
		return q + "\n\tAND " + col + " " + op + " (" +
			strings.Join(placeholders, ",") + ")"
	}
	appendCSV := func(q, col, csv string, include bool) string {
		if csv == "" {
			return q
		}
		return appendValues(q, col, strings.Split(csv, ","), include)
	}

	where = appendCSV(where, "s.agent", f.Agent, true)
	where = appendValues(where, "s.project", f.ProjectFilterLabels(), true)
	where = appendCSV(where, "s.machine", f.Machine, true)
	if f.GitBranch != "" {
		where += "\n\tAND " + db.BranchPairPredicate(
			"s.project", "s.git_branch", f.GitBranch,
			func(s string) string { return pb.add(s) })
	}
	where = appendValues(
		where, "s.project", f.ExcludedProjectFilterLabels(), false,
	)
	where = appendCSV(where, "s.agent", f.ExcludeAgent, false)

	if f.MinUserMessages > 0 {
		where += "\n\tAND s.user_message_count >= " +
			pb.add(f.MinUserMessages)
	}
	scope := normalizePGAutomatedScope(
		f.AutomatedScope, f.ExcludeAutomated)
	if f.ExcludeOneShot {
		if scope == "human" {
			where += "\n\tAND s.user_message_count > 1"
		} else {
			where += "\n\tAND (s.user_message_count > 1 OR COALESCE(s.is_automated, false) = TRUE)"
		}
	}
	if pred := pgAutomatedScopePredicate(
		scope,
		"COALESCE(s.is_automated, false)",
	); pred != "" {
		where += "\n\tAND " + pred
	}
	if f.ActiveSince != "" {
		where += "\n\tAND COALESCE(s.ended_at, s.started_at, s.created_at) >= " +
			pb.add(f.ActiveSince) + "::timestamptz"
	}
	if pred := pgUsageTerminationPred(f.Termination, pb); pred != "" {
		where += "\n\tAND " + pred
	}

	return where
}

func pgUsageTerminationPred(status string, pb *paramBuilder) string {
	if status == "" || status == "all" {
		return ""
	}
	now := time.Now().UTC()
	activeCutoff := now.Add(-pgActiveWindow)
	staleCutoff := now.Add(-pgStaleWindow)
	const activityExpr = "COALESCE(s.ended_at, s.started_at, s.created_at)"
	const flagged = "s.termination_status IN ('tool_call_pending', 'truncated')"

	parts := strings.Split(status, ",")
	preds := make([]string, 0, len(parts))
	for _, p := range parts {
		switch strings.TrimSpace(p) {
		case "active":
			preds = append(preds,
				activityExpr+" > "+pb.add(activeCutoff))
		case "stale":
			preds = append(preds, "("+
				activityExpr+" > "+pb.add(staleCutoff)+
				" AND "+activityExpr+" <= "+pb.add(activeCutoff)+
				" AND "+flagged+")")
		case "unclean":
			preds = append(preds, "("+
				activityExpr+" <= "+pb.add(staleCutoff)+
				" AND "+flagged+")")
		case "clean":
			preds = append(preds, "s.termination_status = 'clean'")
		case "awaiting_user":
			preds = append(preds,
				"s.termination_status = 'awaiting_user'")
		}
	}
	if len(preds) == 0 {
		return ""
	}
	if len(preds) == 1 {
		return preds[0]
	}
	return "(" + strings.Join(preds, " OR ") + ")"
}

const pgUsageRowsSQLTemplate = `
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
	m.model,
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

const pgDailyUsageMessageRowsSQLTemplate = `
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
	isAutomated              bool
	sessionActivityAt        sql.NullTime
	displayName              string
	startedAt                sql.NullTime
}

type pgDailyUsageScanRow struct {
	messageOrdinal           sql.NullInt64
	usageSource              string
	ts                       sql.NullTime
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
}

func pgUsageRowSelectFromRows(rowsSQL string) string {
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
		rowsSQL, includeMachine, "u.session_id", false)
}

func pgDailyUsageRowSelectFromSnapshotRowsWithMachine(
	rowsSQL string, includeMachine bool,
) string {
	return pgDailyUsageRowSelectFromRowsWithSession(
		rowsSQL, includeMachine, "u.snapshot_attribution_session_id", true)
}

func pgDailyUsageRowSelectFromRowsWithSession(
	rowsSQL string, includeMachine bool, sessionColumn string,
	reloadSessionMetadata bool,
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
	return `
SELECT
	` + sessionColumn + `,
	u.message_ordinal,
	u.usage_source,
	u.ts,
	u.model,
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
	` + agentColumn + ` AS agent` + machineColumn + `
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
		padded := paddedUTCBound(f.From+"T00:00:00Z", -14)
		b.from = pb.add(padded)
	}
	if f.To != "" {
		padded := paddedUTCBound(f.To+"T23:59:59Z", 14)
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

func pgCursorUsageRowsSQLForBounds(
	pb *paramBuilder, f db.UsageFilter, b pgUsageBounds,
) (string, bool) {
	hasTermFilter := f.Termination != "" && f.Termination != "all"
	// Cursor usage rows carry no project or git branch and bypass the session
	// filter, so any filter they cannot satisfy (project, machine, branch)
	// must exclude them entirely rather than let them leak into totals.
	if len(f.ProjectFilterLabels()) > 0 ||
		len(f.ExcludedProjectFilterLabels()) > 0 ||
		f.Machine != "" || f.GitBranch != "" || f.MinUserMessages > 0 ||
		f.ExcludeOneShot || hasTermFilter || f.ActiveSince != "" {
		return "", false
	}
	if f.Agent != "" {
		vals := strings.Split(f.Agent, ",")
		for i := range vals {
			vals[i] = strings.TrimSpace(vals[i])
		}
		if !slices.Contains(vals, "cursor") {
			return "", false
		}
	}
	if f.ExcludeAgent != "" {
		vals := strings.Split(f.ExcludeAgent, ",")
		for i := range vals {
			vals[i] = strings.TrimSpace(vals[i])
		}
		if slices.Contains(vals, "cursor") {
			return "", false
		}
	}

	where := "cu.model != ''"
	scope := normalizePGAutomatedScope(f.AutomatedScope, f.ExcludeAutomated)
	if pred := pgAutomatedScopePredicate(scope, "cu.is_headless"); pred != "" {
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

func pgTopSessionsUsageRowQuery(pb *paramBuilder, f db.UsageFilter) string {
	bounds := pgUsageBoundsForFilter(pb, f)
	rowsSQL := pgDailyUsageRowsSQLForBounds(
		pb, pgUsageSnapshotInputFilter(f), bounds)
	rowsSQL = pgSnapshotRankedDailyUsageRowsSQL(pb, rowsSQL, f)
	return pgDailyUsageRowSelectFromSnapshotRowsWithMachine(rowsSQL, false)
}

func pgUsageSnapshotInputFilter(f db.UsageFilter) db.UsageFilter {
	return db.UsageFilter{From: f.From, To: f.To, Timezone: f.Timezone}
}

func pgExactUsageUTCWindow(f db.UsageFilter) (from, to time.Time) {
	loc := usageLocation(f)
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
		&r.displayName,
		&r.startedAt,
	)
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

func pgClampedUsageRowTokens(
	inputTokens, outputTokens, cacheCreationInputTokens,
	cacheReadInputTokens int,
) (inputTok, outputTok, cacheCrTok, cacheRdTok int) {
	return db.ClampPlausibleTokens(int64(inputTokens)),
		db.ClampPlausibleTokens(int64(outputTokens)),
		db.ClampPlausibleTokens(int64(cacheCreationInputTokens)),
		db.ClampPlausibleTokens(int64(cacheReadInputTokens))
}

func pgUsageEventRowTokens(
	source string,
	inputTokens, outputTokens, cacheCreationInputTokens,
	cacheReadInputTokens int,
) (inputTok, outputTok, cacheCrTok, cacheRdTok int) {
	if source == "session" {
		return pgFloorNegativeTokens(inputTokens),
			pgFloorNegativeTokens(outputTokens),
			pgFloorNegativeTokens(cacheCreationInputTokens),
			pgFloorNegativeTokens(cacheReadInputTokens)
	}
	return pgClampedUsageRowTokens(
		inputTokens, outputTokens,
		cacheCreationInputTokens, cacheReadInputTokens)
}

func pgFloorNegativeTokens(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// pgUsageLookupModel mirrors internal/db usage pricing: Kimi runtime aliases
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

func pgDailyUsageAmounts(
	r pgDailyUsageScanRow, pricing *export.PricingResolver,
) (
	inputTok, outputTok, cacheCrTok, cacheRdTok int,
	cost, savings money.Money,
	err error,
) {
	inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok :=
		pgDailyUsageRowTokens(r)

	pricedModel, lookup := pricing.Resolve(
		r.model, pgUsageLookupModel(r.model, r.ts))
	rates := lookup.Rates
	requestScoped := pgUsageRowIsRequestScoped(r.usageSource, r.messageOrdinal)
	if r.cost.Valid && r.costSource != db.CopilotReportedCostSource {
		cost = money.Money{Microdollars: r.cost.Int64}
		pricing.RecordResolvedReported(r.model, pricedModel, lookup)
	} else {
		cost, err = rates.CostForTokensScoped(
			requestScoped,
			inputTok, outputTok, reasoningTok, cacheCrTok, cacheRdTok)
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
		pgRecordComputedUsagePricing(
			pricing, r.model, pricedModel, lookup, requestScoped,
			inputTok, cacheCrTok, cacheRdTok,
		)
	}
	selectedRates := rates
	if requestScoped {
		selectedRates = rates.RatesForTokens(inputTok, cacheCrTok, cacheRdTok)
	}
	readRate, err := money.Sub(
		selectedRates.InputPerMTok, selectedRates.CacheReadPerMTok)
	if err != nil {
		return 0, 0, 0, 0, money.Money{}, money.Money{},
			fmt.Errorf("deriving pg cache read rate for model %q: %w", r.model, err)
	}
	creationRate, err := money.Sub(
		selectedRates.InputPerMTok, selectedRates.CacheWritePerMTok)
	if err != nil {
		return 0, 0, 0, 0, money.Money{}, money.Money{},
			fmt.Errorf("deriving pg cache creation rate for model %q: %w", r.model, err)
	}
	savings, err = money.SignedCostPerMillion([]money.RatedTokens{
		{Tokens: int64(cacheRdTok), Rate: readRate},
		{Tokens: int64(cacheCrTok), Rate: creationRate},
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
		inputTok, outputTok, cacheCrTok, cacheRdTok =
			pgUsageEventRowTokens(
				r.usageSource,
				r.inputTokens, r.outputTokens,
				r.cacheCreationInputTokens, r.cacheReadInputTokens)
	}
	return
}

func pgUsageRowIsRequestScoped(
	usageSource string, messageOrdinal sql.NullInt64,
) bool {
	return db.UsageSourceIsRequestScoped(usageSource) || messageOrdinal.Valid
}

func pgRecordComputedUsagePricing(
	pricing *export.PricingResolver,
	reportedModel, pricedModel string,
	lookup export.PricingLookup,
	requestScoped bool,
	inputTokens, cacheWriteTokens, cacheReadTokens int,
) {
	if requestScoped {
		pricing.RecordResolvedComputedRequest(
			reportedModel, pricedModel, lookup,
			inputTokens, cacheWriteTokens, cacheReadTokens)
		return
	}
	pricing.RecordResolvedComputedAggregate(reportedModel, pricedModel, lookup)
}

type pgUsageDedupToken struct {
	kind  string
	value string
}

func pgUsageDedupTokenForRow(
	usageSource, agent, claudeMessageID, claudeRequestID, sourceUUID, usageDedupKey string,
) (pgUsageDedupToken, bool) {
	if claudeMessageID != "" && claudeRequestID != "" {
		return pgUsageDedupToken{
			kind:  "claude",
			value: claudeMessageID + ":" + claudeRequestID,
		}, true
	}
	if usageSource == "message" && agent != "" && sourceUUID != "" {
		return pgUsageDedupToken{
			kind:  "source",
			value: agent + ":" + sourceUUID,
		}, true
	}
	if usageDedupKey != "" {
		return pgUsageDedupToken{
			kind:  "usage",
			value: usageDedupKey,
		}, true
	}
	return pgUsageDedupToken{}, false
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
		inTok, outTok, crTok, rdTok = pgUsageEventRowTokens(
			r.usageSource,
			r.inputTokens, r.outputTokens,
			r.cacheCreationInputTokens, r.cacheReadInputTokens)
	}
	pricedModel, lookup := pricing.Resolve(
		r.model, pgUsageLookupModel(r.model, r.ts))
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
	requestScoped := pgUsageRowIsRequestScoped(r.usageSource, r.messageOrdinal)
	cost, err = lookup.Rates.CostForTokensScoped(
		requestScoped,
		inTok, outTok, reasoningTok, crTok, rdTok)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing pg session usage for model %q: %w", r.model, err)
	}
	cost, err = export.AddWebSearchFee(cost, webSearches)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing pg session usage for model %q: %w", r.model, err)
	}
	pgRecordComputedUsagePricing(
		pricing, r.model, pricedModel, lookup,
		requestScoped, inTok, crTok, rdTok)
	return cost, true, true, nil
}

func startedAtString(ts sql.NullTime) string {
	if !ts.Valid {
		return ""
	}
	return FormatISO8601(ts.Time)
}
