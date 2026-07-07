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
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
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
	if f.ExcludeGitBranch != "" {
		where += "\n\tAND " + db.BranchPairExcludePredicate(
			"s.project", "s.git_branch", f.ExcludeGitBranch,
			func(s string) string { return pb.add(s) })
	}

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
	NULL::double precision AS cost_usd,
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
	ue.cost_usd,
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
	NULL::double precision AS cost_usd,
	'' AS cost_source,
	m.claude_message_id,
	m.claude_request_id,
	m.source_uuid,
	'' AS usage_dedup_key,
	s.project,
	s.agent,
	s.machine,
	s.git_branch
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
	s.git_branch
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
	NULL::double precision AS cost_usd,
	'' AS cost_source,
	m.claude_message_id,
	m.claude_request_id,
	m.source_uuid,
	'' AS usage_dedup_key,
	s.project,
	s.agent,
	s.machine,
	s.git_branch
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
			ue.cost_usd,
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
	s.git_branch
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
		ue.cost_usd,
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
	costUSD                  sql.NullFloat64
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
	model                    string
	tokenJSON                string
	inputTokens              int
	outputTokens             int
	cacheCreationInputTokens int
	cacheReadInputTokens     int
	reasoningTokens          int
	costUSD                  sql.NullFloat64
	costSource               string
	claudeMessageID          string
	claudeRequestID          string
	sourceUUID               string
	usageDedupKey            string
	project                  string
	agent                    string
	machine                  string
	gitBranch                string
}

type pgTopSessionMetadata struct {
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
	machineColumn := ""
	if includeMachine {
		machineColumn = ",\n\tu.machine"
	}
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
		u.cost_source,
		u.claude_message_id,
	u.claude_request_id,
	u.source_uuid,
	u.usage_dedup_key,
	u.project,
	u.agent` + machineColumn + `,
	u.git_branch
FROM (` + rowsSQL + `) u
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
	cu.charged_cents / 100.0 AS cost_usd,
	'cursor-reported' AS cost_source,
	'' AS claude_message_id,
	'' AS claude_request_id,
	'' AS source_uuid,
	cu.dedup_key AS usage_dedup_key,
	'' AS project,
	'cursor' AS agent,
	'' AS machine,
	'' AS git_branch
FROM cursor_usage_events cu
WHERE %s`

func pgCursorUsageRowsSQLForBounds(
	pb *paramBuilder, f db.UsageFilter, b pgUsageBounds,
) (string, bool) {
	// Any explicit termination filter other than "all" drops cursor rows,
	// even values the session-row path would ignore as unrecognized.
	hasTermFilter := f.Termination != "" && f.Termination != "all"
	if f.RequiresSessionScope() || hasTermFilter {
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
	rowsSQL := pgDailyUsageRowsSQLForBounds(pb, f, bounds)
	if hasCursorTable {
		cursorRowsSQL, ok := pgCursorUsageRowsSQLForBounds(pb, f, bounds)
		if ok {
			rowsSQL += "\n\nUNION ALL\n\n" + cursorRowsSQL
		}
	}
	return pgDailyUsageRowSelectFromRowsWithMachine(rowsSQL, f.Breakdowns)
}

func pgTopSessionsUsageRowQuery(pb *paramBuilder, f db.UsageFilter) string {
	return pgUsageRowQuery(pb, f)
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
		&r.costUSD,
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
	rows *sql.Rows, includeMachine bool,
) (pgDailyUsageScanRow, error) {
	var r pgDailyUsageScanRow
	dest := []any{
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
	dest = append(dest, &r.gitBranch)
	err := rows.Scan(dest...)
	return r, err
}

func pgTokenJSONCount(usage gjson.Result, key string) int {
	return db.ClampPlausibleTokens(usage.Get(key).Int())
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

func pgDailyUsageAmounts(
	r pgDailyUsageScanRow, pricing *export.PricingResolver,
) (inputTok, outputTok, cacheCrTok, cacheRdTok int, cost, savings float64) {
	reasoningTok := r.reasoningTokens
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

	lookup := pricing.Lookup(r.model)
	rates := lookup.Rates
	if r.costUSD.Valid && r.costSource != db.CopilotReportedCostSource {
		cost = r.costUSD.Float64
		pricing.RecordReported(r.model, lookup)
	} else {
		cost = rates.CostForTokens(
			inputTok, outputTok, reasoningTok, cacheCrTok, cacheRdTok)
		pricing.RecordComputed(r.model, lookup)
	}
	readDelta := float64(cacheRdTok) *
		(rates.InputPerMTok - rates.CacheReadPerMTok) / 1_000_000
	createDelta := float64(cacheCrTok) *
		(rates.InputPerMTok - rates.CacheWritePerMTok) / 1_000_000
	savings = readDelta + createDelta
	return
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
) (cost float64, priced, contributes bool) {
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

	if r.costUSD.Valid {
		pricing.RecordReported(r.model, pricing.Lookup(r.model))
		return r.costUSD.Float64, true, true
	}
	if inTok == 0 && outTok == 0 && reasoningTok == 0 &&
		crTok == 0 && rdTok == 0 {
		return 0, true, false
	}
	lookup := pricing.Lookup(r.model)
	if !lookup.OK {
		pricing.RecordComputed(r.model, lookup)
		return 0, false, true
	}
	cost = lookup.Rates.CostForTokens(
		inTok, outTok, reasoningTok, crTok, rdTok)
	pricing.RecordComputed(r.model, lookup)
	return cost, true, true
}

func pgSessionUsageBreakdownEntry(
	r pgUsageScanRow,
	ordinal int,
	cost float64,
	priced bool,
) db.SessionUsageBreakdownEntry {
	var inTok, outTok, crTok, rdTok int
	if r.usageSource == "message" {
		usage := gjson.Parse(r.tokenJSON)
		inTok = pgTokenJSONCount(usage, "input_tokens")
		outTok = pgTokenJSONCount(usage, "output_tokens")
		crTok = pgTokenJSONCount(usage, "cache_creation_input_tokens")
		rdTok = pgTokenJSONCount(usage, "cache_read_input_tokens")
	} else {
		inTok, outTok, crTok, rdTok = pgUsageEventRowTokens(
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
		CostUSD:                  cost,
		HasCost:                  priced,
	}
	if r.messageOrdinal.Valid {
		messageOrdinal := int(r.messageOrdinal.Int64)
		entry.MessageOrdinal = &messageOrdinal
	}
	return entry
}

func pgSessionUsageBreakdownLabel(r pgUsageScanRow) string {
	if r.messageOrdinal.Valid {
		if r.usageSource == "message" {
			return fmt.Sprintf("Prompt %d", r.messageOrdinal.Int64+1)
		}
		return fmt.Sprintf("Step %d", r.messageOrdinal.Int64+1)
	}
	if r.usageSource != "" {
		return r.usageSource
	}
	return "usage"
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
	started_at
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

	var cost float64
	var authoritativeCost *float64
	var hasComputedCost, hasReportedCost bool
	contributing := false
	allPriced := true
	modelsSet := make(map[string]struct{})
	unpricedSet := make(map[string]struct{})
	breakdown := make([]db.SessionUsageBreakdownEntry, 0)
	breakdownCount := 0

	seen := make(map[pgUsageDedupToken]struct{})

	for rows.Next() {
		r, scanErr := scanPGUsageRow(rows)
		if scanErr != nil {
			return nil,
				fmt.Errorf("scanning pg session usage row: %w", scanErr)
		}
		if key, ok := pgUsageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		costRow := r
		authoritative := r.costSource == db.CopilotReportedCostSource && r.costUSD.Valid
		if authoritative {
			v := r.costUSD.Float64
			authoritativeCost = &v
			costRow.costUSD = sql.NullFloat64{}
		}
		c, priced, contributes := pgSessionRowCost(costRow, rateResolver)
		if !contributes {
			continue
		}
		contributing = true
		modelsSet[r.model] = struct{}{}
		if !authoritative {
			if r.costUSD.Valid {
				hasReportedCost = true
			} else {
				hasComputedCost = true
			}
		}
		if priced {
			cost += c
		} else {
			allPriced = false
			unpricedSet[r.model] = struct{}{}
		}
		breakdownCount++
		if includeBreakdown {
			breakdown = append(breakdown, pgSessionUsageBreakdownEntry(
				r, breakdownCount, c, priced))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pg session usage rows: %w", err)
	}
	if authoritativeCost != nil && len(breakdown) > 0 {
		weights := make([]float64, len(breakdown))
		for i := range breakdown {
			weights[i] = breakdown[i].CostUSD
		}
		costs := export.AllocateCostByWeight(*authoritativeCost, weights)
		for i := range breakdown {
			breakdown[i].CostUSD = costs[i]
			breakdown[i].HasCost = true
		}
	}

	out := &db.SessionUsage{
		SessionID:         sess.ID,
		Agent:             sess.Agent,
		Project:           sess.Project,
		TotalOutputTokens: sess.TotalOutputTokens,
		PeakContextTokens: sess.PeakContextTokens,
		HasTokenData: sess.HasTotalOutputTokens ||
			sess.HasPeakContextTokens,
		Models:         sortedStringSetKeys(modelsSet),
		HasCost:        authoritativeCost != nil || (contributing && allPriced),
		BreakdownCount: breakdownCount,
		Breakdown:      breakdown,
	}
	if authoritativeCost != nil {
		out.CostUSD = *authoritativeCost
		out.CostSource = export.CostSourceReported
	} else if out.HasCost {
		out.CostUSD = cost
		out.CostSource = export.CombinedCostSource(
			hasComputedCost, hasReportedCost)
	}
	if out.HasCost {
		out.AICredits = db.AICreditsFromCost(sess.Agent, out.CostUSD)
	}
	if len(unpricedSet) > 0 {
		out.UnpricedModels = sortedStringSetKeys(unpricedSet)
	}
	return out, nil
}

func sortedStringSetKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// GetDailyUsage returns token usage and cost aggregated by day.
func (s *Store) GetDailyUsage(
	ctx context.Context, f db.UsageFilter,
) (db.DailyUsageResult, error) {
	loc := usageLocation(f)

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
		date      string
		project   string
		agent     string
		machine   string
		model     string
		gitBranch string
	}
	type sessionCost struct {
		estimated     map[accumKey]float64
		authoritative *float64
	}
	accum := make(map[accumKey]*db.UsageBucket)
	sessionCosts := make(map[string]sessionCost)
	useAuthoritativeCost := f.Model == "" && f.ExcludeModel == ""
	seen := make(map[pgUsageDedupToken]struct{})
	var seenSessions map[string]db.UsageSessionInfo
	if !f.SkipSessionCounts {
		seenSessions = make(map[string]db.UsageSessionInfo)
	}
	projectLabels := make(map[string]struct{})
	var totalSavings float64

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

		if key, ok := pgUsageDedupTokenForRow(
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

		inputTok, outputTok, cacheCrTok, cacheRdTok, cost, savings :=
			pgDailyUsageAmounts(r, rateResolver)
		totalSavings += savings

		// Leave gitBranch out of the accumulator key unless breakdowns are
		// requested, so a plain totals query still sums one row per
		// (date, project, agent, model) instead of splitting by branch too.
		gitBranch := ""
		if f.Breakdowns {
			gitBranch = r.gitBranch
		}
		key := accumKey{
			date: date, project: r.project,
			agent: r.agent, machine: r.machine, model: r.model,
			gitBranch: gitBranch,
		}
		b, ok := accum[key]
		if !ok {
			b = &db.UsageBucket{}
			accum[key] = b
		}
		b.InputTok += inputTok
		b.OutputTok += outputTok
		b.CacheCr += cacheCrTok
		b.CacheRd += cacheRdTok

		sc := sessionCosts[r.sessionID]
		if sc.estimated == nil {
			sc.estimated = make(map[accumKey]float64)
		}
		sc.estimated[key] += cost
		if useAuthoritativeCost &&
			r.costSource == db.CopilotReportedCostSource &&
			r.costUSD.Valid {
			v := r.costUSD.Float64
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
				if a.model != b.model {
					return a.model < b.model
				}
				return a.gitBranch < b.gitBranch
			})
			weights := make([]float64, len(keys))
			for i, key := range keys {
				weights[i] = sc.estimated[key]
			}
			costs := export.AllocateCostByWeight(*sc.authoritative, weights)
			for i, key := range keys {
				b := accum[key]
				if b == nil {
					b = &db.UsageBucket{}
					accum[key] = b
				}
				b.Cost += costs[i]
				b.AggregateCost += costs[i]
			}
		} else {
			for key, cost := range sc.estimated {
				b := accum[key]
				if b == nil {
					b = &db.UsageBucket{}
					accum[key] = b
				}
				b.Cost += cost
				b.AggregateCost += cost
			}
		}
	}

	if !f.Breakdowns {
		type dateModelKey struct {
			date  string
			model string
		}
		type modelAccum struct {
			inputTok      int
			outputTok     int
			cacheCr       int
			cacheRd       int
			cost          float64
			aggregateCost float64
		}
		dm := make(map[dateModelKey]*modelAccum)
		for key, b := range accum {
			dmk := dateModelKey{date: key.date, model: key.model}
			ma, ok := dm[dmk]
			if !ok {
				ma = &modelAccum{}
				dm[dmk] = ma
			}
			ma.inputTok += b.InputTok
			ma.outputTok += b.OutputTok
			ma.cacheCr += b.CacheCr
			ma.cacheRd += b.CacheRd
			ma.cost += b.Cost
			ma.aggregateCost += b.AggregateCost
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
				if left.cost != right.cost {
					return left.cost > right.cost
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
				entry.TotalCost += ma.aggregateCost
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
			totals.TotalCost += entry.TotalCost
		}
		if daily == nil {
			daily = []db.DailyUsageEntry{}
		}
		totals.CacheSavings = totalSavings

		var aiCredits float64
		for key, b := range accum {
			aiCredits += db.AICreditsFromCost(key.agent, b.AggregateCost)
		}
		if aiCredits > 0 {
			totals.CopilotAICredits = aiCredits
		}

		var sessionCounts db.UsageSessionCounts
		if seenSessions != nil {
			sessionCounts = db.NewUsageSessionCounts(seenSessions)
		}
		projects, err := s.BuildProjectIdentityMap(ctx,
			sortedStringSetKeys(projectLabels))
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

	type branchMapKey struct {
		project string
		branch  string
	}
	type dayMaps struct {
		models   map[string]db.UsageBucket
		projects map[string]db.UsageBucket
		agents   map[string]db.UsageBucket
		machines map[string]db.UsageBucket
		branches map[branchMapKey]db.UsageBucket
	}
	days := make(map[string]*dayMaps, 64)
	for key, b := range accum {
		dm, ok := days[key.date]
		if !ok {
			dm = &dayMaps{
				models:   make(map[string]db.UsageBucket, 4),
				projects: make(map[string]db.UsageBucket, 8),
				agents:   make(map[string]db.UsageBucket, 4),
				machines: make(map[string]db.UsageBucket, 4),
				branches: make(map[branchMapKey]db.UsageBucket, 8),
			}
			days[key.date] = dm
		}
		db.AddUsageBucket(dm.models, key.model, *b)
		aggregateBucket := *b
		aggregateBucket.Cost = b.AggregateCost
		db.AddUsageBucket(dm.projects, key.project, aggregateBucket)
		db.AddUsageBucket(dm.agents, key.agent, aggregateBucket)
		db.AddUsageBucket(dm.machines, key.machine, aggregateBucket)
		db.AddUsageBucket(dm.branches, branchMapKey{
			project: key.project,
			branch:  key.gitBranch,
		}, aggregateBucket)
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
			if left.Cost != right.Cost {
				return left.Cost > right.Cost
			}
			return modelNames[i] < modelNames[j]
		})
		entry.ModelsUsed = modelNames
		mbd := make([]db.ModelBreakdown, 0, len(modelNames))
		for _, m := range modelNames {
			b := dm.models[m]
			entry.InputTokens += b.InputTok
			entry.OutputTokens += b.OutputTok
			entry.CacheCreationTokens += b.CacheCr
			entry.CacheReadTokens += b.CacheRd
			entry.TotalCost += b.AggregateCost
			mbd = append(mbd, db.ModelBreakdown{
				ModelName:           m,
				InputTokens:         b.InputTok,
				OutputTokens:        b.OutputTok,
				CacheCreationTokens: b.CacheCr,
				CacheReadTokens:     b.CacheRd,
				Cost:                b.Cost,
			})
		}
		entry.ModelBreakdowns = mbd

		pbd := make([]db.ProjectBreakdown, 0, len(dm.projects))
		for p, b := range dm.projects {
			pbd = append(pbd, db.ProjectBreakdown{
				Project:             p,
				InputTokens:         b.InputTok,
				OutputTokens:        b.OutputTok,
				CacheCreationTokens: b.CacheCr,
				CacheReadTokens:     b.CacheRd,
				Cost:                b.Cost,
			})
		}
		sort.Slice(pbd, func(i, j int) bool {
			if pbd[i].Cost != pbd[j].Cost {
				return pbd[i].Cost > pbd[j].Cost
			}
			return pbd[i].Project < pbd[j].Project
		})
		entry.ProjectBreakdowns = pbd

		abd := make([]db.AgentBreakdown, 0, len(dm.agents))
		for a, b := range dm.agents {
			abd = append(abd, db.AgentBreakdown{
				Agent:               a,
				InputTokens:         b.InputTok,
				OutputTokens:        b.OutputTok,
				CacheCreationTokens: b.CacheCr,
				CacheReadTokens:     b.CacheRd,
				Cost:                b.Cost,
			})
		}
		sort.Slice(abd, func(i, j int) bool {
			if abd[i].Cost != abd[j].Cost {
				return abd[i].Cost > abd[j].Cost
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
				InputTokens:         b.InputTok,
				OutputTokens:        b.OutputTok,
				CacheCreationTokens: b.CacheCr,
				CacheReadTokens:     b.CacheRd,
				Cost:                b.Cost,
			})
		}
		sort.Slice(machineBreakdowns, func(i, j int) bool {
			if machineBreakdowns[i].Cost != machineBreakdowns[j].Cost {
				return machineBreakdowns[i].Cost > machineBreakdowns[j].Cost
			}
			return machineBreakdowns[i].MachineName < machineBreakdowns[j].MachineName
		})
		entry.MachineBreakdowns = machineBreakdowns

		bbd := make([]db.BranchBreakdown, 0, len(dm.branches))
		for bk, b := range dm.branches {
			bbd = append(bbd, db.BranchBreakdown{
				Project:             bk.project,
				Branch:              bk.branch,
				InputTokens:         b.InputTok,
				OutputTokens:        b.OutputTok,
				CacheCreationTokens: b.CacheCr,
				CacheReadTokens:     b.CacheRd,
				Cost:                b.Cost,
			})
		}
		db.SortBranchBreakdowns(bbd)
		entry.BranchBreakdowns = bbd

		daily = append(daily, entry)
		totals.InputTokens += entry.InputTokens
		totals.OutputTokens += entry.OutputTokens
		totals.CacheCreationTokens += entry.CacheCreationTokens
		totals.CacheReadTokens += entry.CacheReadTokens
		totals.TotalCost += entry.TotalCost
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
		sortedStringSetKeys(projectLabels))
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
	query := pgTopSessionsUsageRowQuery(pb, f)
	query += ` ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC`

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("querying top sessions: %w", err)
	}
	defer rows.Close()

	loc := usageLocation(f)
	type sessAccum struct {
		totalTokens       int
		cost              float64
		authoritativeCost *float64
	}

	accum := make(map[string]*sessAccum)
	var order []string
	seen := make(map[pgUsageDedupToken]struct{})

	for rows.Next() {
		r, err := scanPGDailyUsageRow(rows)
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

		if key, ok := pgUsageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		inputTok, outputTok, cacheCrTok, cacheRdTok, cost, _ :=
			pgDailyUsageAmounts(r, rateResolver)

		sa, ok := accum[r.sessionID]
		if !ok {
			sa = &sessAccum{}
			accum[r.sessionID] = sa
			order = append(order, r.sessionID)
		}
		sa.totalTokens += inputTok + outputTok + cacheCrTok + cacheRdTok
		sa.cost += cost
		if f.Model == "" && f.ExcludeModel == "" &&
			r.costSource == db.CopilotReportedCostSource &&
			r.costUSD.Valid {
			v := r.costUSD.Float64
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
			SessionID:   id,
			DisplayName: id,
			TotalTokens: sa.totalTokens,
			Cost: func() float64 {
				if sa.authoritativeCost != nil {
					return *sa.authoritativeCost
				}
				return sa.cost
			}(),
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
		}
	}
	return result, nil
}

// GetUsageSessionCounts returns distinct session counts grouped by project and agent.
func (s *Store) GetUsageSessionCounts(
	ctx context.Context, f db.UsageFilter,
) (db.UsageSessionCounts, error) {
	pb := &paramBuilder{}
	query := pgUsageRowQuery(pb, f)
	query += ` ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC`

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return db.UsageSessionCounts{},
			fmt.Errorf("querying session counts: %w", err)
	}
	defer rows.Close()

	loc := usageLocation(f)
	type sessInfo struct {
		project string
		agent   string
	}

	seen := make(map[string]sessInfo)
	dedup := make(map[pgUsageDedupToken]struct{})

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

		if key, ok := pgUsageDedupTokenForRow(
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

	loc := usageLocation(f)
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
