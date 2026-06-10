package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/db"
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

	where = appendCSV(where, "s.agent", f.Agent, true)
	where = appendCSV(where, "s.project", f.Project, true)
	where = appendCSV(where, "s.machine", f.Machine, true)
	where = appendCSV(where, "s.project", f.ExcludeProject, false)
	where = appendCSV(where, "s.agent", f.ExcludeAgent, false)

	if f.MinUserMessages > 0 {
		where += "\n\tAND s.user_message_count >= " +
			pb.add(f.MinUserMessages)
	}
	if f.ExcludeOneShot {
		where += "\n\tAND s.user_message_count > 1"
	}
	if f.ExcludeAutomated {
		where += "\n\tAND COALESCE(s.is_automated, false) = false"
	}
	if f.ActiveSince != "" {
		where += "\n\tAND COALESCE(s.ended_at, s.started_at, s.created_at) >= " +
			pb.add(f.ActiveSince) + "::timestamptz"
	}

	return where
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
	NULL::double precision AS cost_usd,
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
	NULL::double precision AS cost_usd,
	m.claude_message_id,
	m.claude_request_id,
	'' AS usage_dedup_key,
	s.project,
	s.agent
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
	costUSD                  sql.NullFloat64
	claudeMessageID          string
	claudeRequestID          string
	usageDedupKey            string
	project                  string
	agent                    string
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

	messageTimestampSourceWhere := pgUsageMessageSourceEligibility +
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

	messageFallbackWhere := pgUsageMessageEligibility +
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

func pgUsageRowQuery(pb *paramBuilder, f db.UsageFilter) string {
	bounds := pgUsageBoundsForFilter(pb, f)
	return pgDailyUsageRowSelectFromRows(pgDailyUsageRowsSQLForBounds(
		pb, f, bounds,
	))
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
	var r pgDailyUsageScanRow
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

func pgDailyUsageAmounts(
	r pgDailyUsageScanRow, pricing map[string]modelRates,
) (inputTok, outputTok, cacheCrTok, cacheRdTok int, cost, savings float64) {
	if r.usageSource == "message" {
		usage := gjson.Parse(r.tokenJSON)
		inputTok = int(usage.Get("input_tokens").Int())
		outputTok = int(usage.Get("output_tokens").Int())
		cacheCrTok = int(usage.Get("cache_creation_input_tokens").Int())
		cacheRdTok = int(usage.Get("cache_read_input_tokens").Int())
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
	createDelta := float64(cacheCrTok) *
		(rates.input - rates.cacheCreation) / 1_000_000
	savings = readDelta + createDelta
	return
}

func pgSessionRowCost(
	r pgUsageScanRow, pricing map[string]modelRates,
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
// estimate from the PostgreSQL session store.
func (s *Store) GetSessionUsage(
	ctx context.Context, sessionID string,
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

	pb := &paramBuilder{}
	query := pgUsageRowSelect() + " AND u.session_id = " +
		pb.add(sessionID) + ` ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC`
	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("querying pg session usage: %w", err)
	}
	defer rows.Close()

	var cost float64
	contributing := false
	allPriced := true
	modelsSet := make(map[string]struct{})
	unpricedSet := make(map[string]struct{})

	type dedupKey struct {
		msgID string
		reqID string
	}
	seen := make(map[dedupKey]struct{})

	for rows.Next() {
		r, scanErr := scanPGUsageRow(rows)
		if scanErr != nil {
			return nil,
				fmt.Errorf("scanning pg session usage row: %w", scanErr)
		}
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
			key := dedupKey{
				msgID: "usage",
				reqID: r.usageDedupKey,
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}

		c, priced, contributes := pgSessionRowCost(r, pricing)
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
		return nil, fmt.Errorf("iterating pg session usage rows: %w", err)
	}

	out := &db.SessionUsage{
		SessionID:         sess.ID,
		Agent:             sess.Agent,
		Project:           sess.Project,
		TotalOutputTokens: sess.TotalOutputTokens,
		PeakContextTokens: sess.PeakContextTokens,
		HasTokenData: sess.HasTotalOutputTokens ||
			sess.HasPeakContextTokens,
		Models:  sortedStringSetKeys(modelsSet),
		HasCost: contributing && allPriced,
	}
	if out.HasCost {
		out.CostUSD = cost
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

	pb := &paramBuilder{}
	query := pgUsageRowQuery(pb, f)
	query += ` ORDER BY u.ts ASC, u.session_id ASC,
		COALESCE(u.message_ordinal, -1) ASC`

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return db.DailyUsageResult{},
			fmt.Errorf("querying daily usage: %w", err)
	}
	defer rows.Close()

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
	type dedupKey struct {
		msgID string
		reqID string
	}

	accum := make(map[accumKey]*bucket)
	seen := make(map[dedupKey]struct{})
	seenSessions := make(map[string]db.UsageSessionInfo)
	var totalSavings float64

	for rows.Next() {
		r, scanErr := scanPGDailyUsageRow(rows)
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

		if r.claudeMessageID != "" && r.claudeRequestID != "" {
			key := dedupKey{msgID: r.claudeMessageID, reqID: r.claudeRequestID}
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
			seenSessions[r.sessionID] = db.UsageSessionInfo{
				Project: r.project,
				Agent:   r.agent,
			}
		}

		inputTok, outputTok, cacheCrTok, cacheRdTok, cost, savings :=
			pgDailyUsageAmounts(r, pricing)
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
		return db.DailyUsageResult{},
			fmt.Errorf("iterating daily usage rows: %w", err)
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
				entry.TotalCost += ma.cost
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
		sessionCounts := db.NewUsageSessionCounts(seenSessions)
		return db.DailyUsageResult{
			Daily:         daily,
			Totals:        totals,
			SessionCounts: sessionCounts,
		}, nil
	}

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
			if left.cost != right.cost {
				return left.cost > right.cost
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
			entry.TotalCost += b.cost
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
		daily = []db.DailyUsageEntry{}
	}
	totals.CacheSavings = totalSavings
	sessionCounts := db.NewUsageSessionCounts(seenSessions)
	return db.DailyUsageResult{
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
		totalTokens int
		cost        float64
	}
	type dedupKey struct {
		msgID string
		reqID string
	}

	accum := make(map[string]*sessAccum)
	var order []string
	seen := make(map[dedupKey]struct{})

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

		if r.claudeMessageID != "" && r.claudeRequestID != "" {
			key := dedupKey{msgID: r.claudeMessageID, reqID: r.claudeRequestID}
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
			pgDailyUsageAmounts(r, pricing)

		sa, ok := accum[r.sessionID]
		if !ok {
			sa = &sessAccum{}
			accum[r.sessionID] = sa
			order = append(order, r.sessionID)
		}
		sa.totalTokens += inputTok + outputTok + cacheCrTok + cacheRdTok
		sa.cost += cost
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
	type dedupKey struct {
		msgID string
		reqID string
	}

	seen := make(map[string]sessInfo)
	dedup := make(map[dedupKey]struct{})

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

		if r.claudeMessageID != "" && r.claudeRequestID != "" {
			key := dedupKey{msgID: r.claudeMessageID, reqID: r.claudeRequestID}
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
