package postgres

import (
	"database/sql"
	"fmt"
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

const pgUsageEventEligibility = `
	ue.model != ''
	AND s.deleted_at IS NULL`

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

type pgUsageScanRow struct {
	sessionID                string
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
