package duckdb

import (
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

const duckUsageMessageEligibility = `
			m.token_usage != ''
			AND m.model != ''
			AND m.model != '<synthetic>'
			AND s.deleted_at IS NULL`

const duckUsageEventEligibility = `
			ue.model != ''
			AND s.deleted_at IS NULL`

func duckUsageLocalDateSQL(f db.UsageFilter) (string, any) {
	if f.Timezone != "" {
		return "COALESCE(strftime(timezone(?, timezone('UTC', ts)), '%Y-%m-%d'), '')", f.Timezone
	}
	ref := time.Now().UTC()
	if f.From != "" {
		if t, err := time.Parse(time.RFC3339, f.From+"T12:00:00Z"); err == nil {
			ref = t
		}
	}
	_, offset := ref.In(time.Local).Zone()
	return "COALESCE(strftime(ts + (? * INTERVAL 1 SECOND), '%Y-%m-%d'), '')", offset
}

func duckPriceModelCaseSQL() string {
	aliases := pricingpkg.DateAliasedModels()
	quoted := make([]string, len(aliases))
	for i, alias := range aliases {
		quoted[i] = "'" + alias + "'"
	}
	cutoff := pricingpkg.KimiModelEraCutoff.UTC().Format("2006-01-02 15:04:05")
	return fmt.Sprintf(`CASE
		WHEN regexp_replace(model, '^.*/', '') IN (%[1]s)
			AND (ts IS NULL OR ts >= TIMESTAMP '%[2]s')
			THEN '%[3]s'
		WHEN regexp_replace(model, '^.*/', '') IN (%[1]s)
			THEN '%[4]s'
		ELSE model
	END`,
		strings.Join(quoted, ", "), cutoff,
		pricingpkg.KimiK3Canonical, pricingpkg.KimiK26Canonical,
	)
}

func duckUsageCTEFromRaw(
	f db.UsageFilter, rawSQL string, args []any,
) (string, []any) {
	localDateSQL, localDateArg := duckUsageLocalDateSQL(f)
	priceModelSQL := duckPriceModelCaseSQL()
	// Apply the local-date window BEFORE deduping so an out-of-range
	// duplicate (pulled in by the padded UTC bounds) cannot win
	// dedup_rank = 1 and suppress the in-range row. Mirrors the
	// dedup-after-date-filter order in internal/db/usage.go.
	datePred := "TRUE"
	var dateArgs []any
	if f.From != "" {
		datePred += " AND local_date >= ?"
		dateArgs = append(dateArgs, f.From)
	}
	if f.To != "" {
		datePred += " AND local_date <= ?"
		dateArgs = append(dateArgs, f.To)
	}
	query := fmt.Sprintf(`
		WITH usage_raw AS (
			%[1]s
		),
		usage_normalized AS (
			SELECT *,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.input_tokens') AS BIGINT), 0), 0), %[4]d)
					WHEN source = 'session' THEN GREATEST(input_tokens, 0)
					ELSE LEAST(GREATEST(input_tokens, 0), %[4]d)
				END AS input_tokens_norm,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.output_tokens') AS BIGINT), 0), 0), %[4]d)
					WHEN source = 'session' THEN GREATEST(output_tokens, 0)
					ELSE LEAST(GREATEST(output_tokens, 0), %[4]d)
				END AS output_tokens_norm,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_creation_input_tokens') AS BIGINT), 0), 0), %[4]d)
					WHEN source = 'session' THEN GREATEST(cache_create, 0)
					ELSE LEAST(GREATEST(cache_create, 0), %[4]d)
				END AS cache_create_norm,
					CASE
						WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_read_input_tokens') AS BIGINT), 0), 0), %[4]d)
						WHEN source = 'session' THEN GREATEST(cache_read, 0)
						ELSE LEAST(GREATEST(cache_read, 0), %[4]d)
					END AS cache_read_norm,
					CASE
						WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.reasoning_tokens') AS BIGINT), 0), 0), %[4]d)
						WHEN source = 'session' THEN GREATEST(reasoning_tokens, 0)
						ELSE LEAST(GREATEST(reasoning_tokens, 0), %[4]d)
					END AS reasoning_tokens_norm,
				CASE
					WHEN claude_message_id != '' AND claude_request_id != ''
						THEN 'claude:' || claude_message_id || ':' || claude_request_id
					WHEN source = 'message' AND agent != '' AND source_uuid != ''
						THEN 'source:' || agent || ':' || source_uuid
					WHEN usage_dedup_key != ''
						THEN 'usage:' || usage_dedup_key
					ELSE 'row:' || session_id || ':' || source || ':' ||
						COALESCE(CAST(message_ordinal AS VARCHAR), '') || ':' ||
						COALESCE(CAST(ts AS VARCHAR), '') || ':' || model
				END AS dedup_group,
				%[2]s AS local_date,
				%[5]s AS price_model
			FROM usage_raw
		),
		usage_windowed AS (
			SELECT *
			FROM usage_normalized
			WHERE %[3]s
		),
		usage_ranked AS (
			SELECT *,
				ROW_NUMBER() OVER (
					PARTITION BY dedup_group
					ORDER BY ts ASC, session_id ASC, COALESCE(message_ordinal, -1) ASC
				) AS dedup_rank
			FROM usage_windowed
		),
		usage_localized AS (
			SELECT *
			FROM usage_ranked
			WHERE dedup_rank = 1
		)`, rawSQL, localDateSQL, datePred, db.MaxPlausibleTokens, priceModelSQL)
	args = append(args, localDateArg)
	args = append(args, dateArgs...)
	return query, args
}

func duckUsageLookupModel(model, ts string) string {
	timestamp, _ := parseDuckTime(ts)
	if canonical := pricingpkg.CanonicalModelForDate(model, timestamp); canonical != "" {
		return canonical
	}
	return model
}
func duckUsageAggregateCost(
	model string,
	inputTok, outputTok, cacheCr, cacheRd int,
	billableInput, billableOutput, billableReasoning, billableCacheCr, billableCacheRd int,
	explicitCost int64,
	hasReportedCost bool,
	requestScoped bool,
	pricing *export.PricingResolver,
) (money.Money, money.Money, bool, bool, error) {
	return duckUsageAggregateResolvedCost(
		model, model,
		inputTok, outputTok, cacheCr, cacheRd,
		billableInput, billableOutput, billableReasoning,
		billableCacheCr, billableCacheRd,
		explicitCost, hasReportedCost, requestScoped, pricing)
}

func duckUsageAggregateResolvedCost(
	reportedModel, canonicalModel string,
	inputTok, outputTok, cacheCr, cacheRd int,
	billableInput, billableOutput, billableReasoning, billableCacheCr, billableCacheRd int,
	explicitCost int64,
	hasReportedCost bool,
	requestScoped bool,
	pricing *export.PricingResolver,
) (money.Money, money.Money, bool, bool, error) {
	pricedModel, lookup := pricing.Resolve(reportedModel, canonicalModel)
	hasBillableTokens := billableInput != 0 || billableOutput != 0 ||
		billableReasoning != 0 || billableCacheCr != 0 || billableCacheRd != 0
	if !hasReportedCost &&
		explicitCost == 0 &&
		inputTok == 0 && outputTok == 0 && cacheCr == 0 && cacheRd == 0 &&
		!hasBillableTokens {
		pricing.RecordResolvedComputed(reportedModel, pricedModel, lookup)
		return money.Money{}, money.Money{}, true, false, nil
	}
	rates := lookup.Rates
	computed, err := rates.CostForTokensScoped(
		requestScoped,
		billableInput, billableOutput, billableReasoning,
		billableCacheCr, billableCacheRd)
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("pricing duckdb usage for model %q: %w", reportedModel, err)
	}
	cost, err := money.Add(
		money.Money{Microdollars: explicitCost}, computed)
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("summing duckdb usage for model %q: %w", reportedModel, err)
	}
	if hasReportedCost {
		pricing.RecordResolvedReported(reportedModel, pricedModel, lookup)
	}
	if hasBillableTokens {
		duckRecordComputedUsagePricing(
			pricing, reportedModel, pricedModel, lookup, requestScoped,
			billableInput, billableCacheCr, billableCacheRd,
		)
	}
	selectedRates := rates
	if requestScoped {
		selectedRates = rates.RatesForTokens(inputTok, cacheCr, cacheRd)
	}
	readRate, err := money.Sub(
		selectedRates.InputPerMTok, selectedRates.CacheReadPerMTok)
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("deriving duckdb cache read rate for model %q: %w", reportedModel, err)
	}
	creationRate, err := money.Sub(
		selectedRates.InputPerMTok, selectedRates.CacheWritePerMTok)
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("deriving duckdb cache creation rate for model %q: %w", reportedModel, err)
	}
	savings, err := money.SignedCostPerMillion([]money.RatedTokens{
		{Tokens: int64(cacheRd), Rate: readRate},
		{Tokens: int64(cacheCr), Rate: creationRate},
	})
	if err != nil {
		return money.Money{}, money.Money{}, false, false,
			fmt.Errorf("pricing duckdb cache savings for model %q: %w", reportedModel, err)
	}
	priced := lookup.OK
	if !hasBillableTokens && hasReportedCost {
		priced = true
	}
	return cost, savings, priced, true, nil
}

func duckRecordComputedUsagePricing(
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
