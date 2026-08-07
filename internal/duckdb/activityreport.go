package duckdb

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// GetActivityReport assembles a concurrency- and usage-oriented report
// for the resolved range `q`, reading from the DuckDB store. It mirrors
// the SQLite (*DB).GetActivityReport and PostgreSQL
// (*Store).GetActivityReport: three fetches scoped to the SAME candidate
// session-ID set so the concurrency timeline, sessions table, and usage
// totals stay mutually consistent (no orphan usage rows), then the
// in-memory streams are handed to activity.Aggregate.
//
// The filter `f` is honored as-is: callers that want one-shot or
// automated sessions included must pass them through with the
// corresponding exclusions disabled. Subagent and fork sessions are
// always counted so the cost totals match GetDailyUsage, which never
// filters by relationship_type. Fork sessions hold only their own
// rewound-branch messages (the parsers partition entries across
// branches), so counting them adds no duplicate activity; any usage
// rows that do recur across sessions collapse in the aggregator's
// dedup, the same guarantee GetDailyUsage relies on.

// GetSessionUsageRows returns the backend-priced usage rows for the supplied
// sessions, with the same cross-session deduplication as activity reports.
type duckSessionUsageOrderedRow struct {
	scan    duckActivityReportUsageRow
	ts      time.Time
	validTS bool
	ordinal int64
}

func (s *Store) GetSessionUsageRows(
	ctx context.Context, ids []string,
) ([]activity.UsageRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	pricing, err := s.LoadPricingMap(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading duckdb pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)
	sessionOrder := make(map[string]int, len(ids))
	for i, id := range ids {
		sessionOrder[id] = i
	}
	args, placeholders := stringInArgs(ids)
	inClause := strings.Join(placeholders, ",")
	rawSQL := fmt.Sprintf(`
		SELECT m.session_id AS session_id, m.ordinal AS message_ordinal,
			'message' AS source, COALESCE(m.timestamp, s.started_at) AS ts,
			m.model AS model, m.token_usage AS token_json,
			m.claude_message_id AS claude_message_id,
			m.claude_request_id AS claude_request_id,
			m.source_uuid AS source_uuid,
			'' AS usage_dedup_key,
			0 AS input_tokens, 0 AS output_tokens,
			0 AS cache_create, 0 AS cache_read,
			COALESCE(TRY_CAST(json_extract_string(m.token_usage, '$.reasoning_tokens') AS BIGINT), 0) AS reasoning_tokens,
			NULL AS cost_microdollars, '' AS cost_source,
			s.project AS project, s.agent AS agent, s.machine AS machine,
			s.user_message_count AS user_message_count, s.is_automated AS is_automated,
			COALESCE(s.display_name, s.session_name, s.first_message, s.project, s.id) AS display_name,
			s.started_at AS started_at,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS activity_at
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE %s
			AND s.id IN (%s)
		UNION ALL
		SELECT ue.session_id AS session_id, ue.message_ordinal AS message_ordinal,
			ue.source AS source, COALESCE(ue.occurred_at, s.started_at) AS ts,
			ue.model AS model, '' AS token_json,
			'' AS claude_message_id, '' AS claude_request_id,
			'' AS source_uuid,
			CASE
				WHEN ue.dedup_key != '' THEN ue.session_id || ':' || ue.source || ':' || ue.dedup_key
				ELSE ue.session_id || ':' || ue.source || ':id:' || CAST(ue.id AS VARCHAR)
			END AS usage_dedup_key,
			ue.input_tokens AS input_tokens, ue.output_tokens AS output_tokens,
			ue.cache_creation_input_tokens AS cache_create,
			ue.cache_read_input_tokens AS cache_read,
			ue.reasoning_tokens AS reasoning_tokens,
			ue.cost_microdollars AS cost_microdollars,
			ue.cost_source AS cost_source,
			s.project AS project, s.agent AS agent, s.machine AS machine,
			s.user_message_count AS user_message_count, s.is_automated AS is_automated,
			COALESCE(s.display_name, s.session_name, s.first_message, s.project, s.id) AS display_name,
			s.started_at AS started_at,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS activity_at
		FROM usage_events ue
		JOIN sessions s ON s.id = ue.session_id
		WHERE %s
			AND s.id IN (%s)`,
		duckUsageMessageEligibility, inClause,
		duckUsageEventEligibility, inClause,
	)
	queryArgs := make([]any, 0, len(args)*2)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, args...)
	cte, queryArgs := duckUsageCTEFromRaw(db.UsageFilter{}, rawSQL, queryArgs)
	query := cte + `
		SELECT session_id, message_ordinal, ts, source, model,
			agent, claude_message_id, claude_request_id, source_uuid,
			usage_dedup_key, input_tokens_norm, output_tokens_norm,
			cache_create_norm, cache_read_norm, reasoning_tokens_norm,
			cost_microdollars, cost_source
		FROM usage_normalized`
	rows, err := s.queryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb session usage rows: %w", err)
	}
	defer rows.Close()
	var rowsAcc []duckSessionUsageOrderedRow
	for rows.Next() {
		var r duckActivityReportUsageRow
		var ts any
		if err := rows.Scan(
			&r.sessionID, &r.messageOrdinal, &ts, &r.source, &r.model,
			&r.agent, &r.claudeMessageID, &r.claudeRequestID, &r.sourceUUID,
			&r.usageDedupKey,
			&r.inputTok, &r.outputTok, &r.cacheCr, &r.cacheRd,
			&r.reasoningTok, &r.cost, &r.costSource,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb session usage rows: %w", err)
		}
		r.ts = formatDBTime(ts)
		ordinal := int64(-1)
		if o, ok := duckUsageOrdinal(r.messageOrdinal); ok {
			ordinal = o
		}
		parsedTS, ok := parseTimestamp(r.ts)
		rowsAcc = append(rowsAcc, duckSessionUsageOrderedRow{
			scan:    r,
			ts:      parsedTS,
			validTS: ok,
			ordinal: ordinal,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb session usage rows: %w", err)
	}
	sort.SliceStable(rowsAcc, func(i, j int) bool {
		return duckSessionUsageRowLess(rowsAcc[i], rowsAcc[j], sessionOrder)
	})
	seen := make(map[string]struct{})
	out := make([]activity.UsageRow, 0, len(rowsAcc))
	for _, o := range rowsAcc {
		r := o.scan
		if key, ok := duckSessionUsageDedupKey(r); ok {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}
		cost, costSource, priced, contributes, sessionCost, priceErr :=
			duckActivityUsageCost(r, rateResolver)
		if priceErr != nil {
			return nil, priceErr
		}
		out = append(out, activity.UsageRow{
			SessionID:       r.sessionID,
			Model:           r.model,
			Timestamp:       r.ts,
			OutputTokens:    r.outputTok,
			Cost:            cost,
			CostSource:      costSource,
			SessionCost:     sessionCost,
			Priced:          priced,
			Contributes:     contributes,
			Agent:           r.agent,
			ClaudeMessageID: r.claudeMessageID,
			ClaudeRequestID: r.claudeRequestID,
			SourceUUID:      r.sourceUUID,
			UsageDedupKey:   r.usageDedupKey,
		})
	}
	return out, nil
}

func duckSessionUsageDedupKey(r duckActivityReportUsageRow) (string, bool) {
	if r.claudeMessageID != "" && r.claudeRequestID != "" {
		return "claude:" + r.claudeMessageID + ":" + r.claudeRequestID, true
	}
	if r.source == "message" && r.agent != "" && r.sourceUUID != "" {
		return "source:" + r.agent + ":" + r.sourceUUID, true
	}
	if r.usageDedupKey != "" {
		return "usage:" + r.usageDedupKey, true
	}
	return "", false
}

func duckSessionUsageRowLess(
	a, b duckSessionUsageOrderedRow,
	sessionOrder map[string]int,
) bool {
	if a.validTS && b.validTS {
		if !a.ts.Equal(b.ts) {
			return a.ts.Before(b.ts)
		}
	} else if a.validTS != b.validTS {
		return a.validTS
	}
	if a.scan.ts != b.scan.ts {
		return a.scan.ts < b.scan.ts
	}
	if ai, ok := sessionOrder[a.scan.sessionID]; ok {
		if bi, ok := sessionOrder[b.scan.sessionID]; ok && ai != bi {
			return ai < bi
		}
	}
	if a.scan.sessionID != b.scan.sessionID {
		return a.scan.sessionID < b.scan.sessionID
	}
	if a.ordinal != b.ordinal {
		return a.ordinal < b.ordinal
	}
	if a.scan.source != b.scan.source {
		return a.scan.source < b.scan.source
	}
	return a.scan.usageDedupKey < b.scan.usageDedupKey
}

// duckActivityReportUsageRow is one scanned usage-union row before mapping
// into an activity.UsageRow, carrying the normalized token amounts and
// dedup keys the aggregator and per-row cost need.
type duckActivityReportUsageRow struct {
	sessionID       string
	source          string
	model           string
	ts              string
	messageOrdinal  any
	agent           string
	claudeMessageID string
	claudeRequestID string
	sourceUUID      string
	usageDedupKey   string
	inputTok        int
	outputTok       int
	cacheCr         int
	cacheRd         int
	reasoningTok    int
	cost            *int64
	costSource      string
}

// duckActivityReportRowStatus computes one usage row's cost and pricing state the same way
// GetDailyUsage does: an explicit cost_microdollars wins, otherwise the per-model
// rates price the normalized token amounts. Billable amounts equal the
// normalized amounts when there is no explicit cost (mirroring the
// billable_* SQL in dailyUsageRowsForAggregation). It returns the cache
// savings delta and the cost.
func duckActivityReportRowStatus(
	r duckActivityReportUsageRow, pricing *export.PricingResolver,
) (savings, cost money.Money, priced, contributes bool, err error) {
	canonicalModel := duckUsageLookupModel(r.model, r.ts)
	var explicitCost int64
	var billableInput, billableOutput, billableReasoning, billableCacheCr, billableCacheRd int
	if r.cost != nil {
		explicitCost = *r.cost
		priced = true
		contributes = true
	} else if r.inputTok != 0 || r.outputTok != 0 || r.reasoningTok != 0 ||
		r.cacheCr != 0 || r.cacheRd != 0 {
		contributes = true
		_, lookup := pricing.Resolve(r.model, canonicalModel)
		priced = lookup.OK
		billableInput = r.inputTok
		billableOutput = r.outputTok
		billableReasoning = r.reasoningTok
		billableCacheCr = r.cacheCr
		billableCacheRd = r.cacheRd
	} else {
		priced = true
		billableInput = r.inputTok
		billableOutput = r.outputTok
		billableReasoning = r.reasoningTok
		billableCacheCr = r.cacheCr
		billableCacheRd = r.cacheRd
	}
	cost, savings, _, _, err = duckUsageAggregateResolvedCost(
		r.model, canonicalModel,
		r.inputTok, r.outputTok, r.cacheCr, r.cacheRd,
		billableInput, billableOutput, billableReasoning,
		billableCacheCr, billableCacheRd,
		explicitCost,
		r.cost != nil,
		db.UsageSourceIsRequestScoped(r.source) ||
			duckActivityUsageHasOrdinal(r.messageOrdinal),
		pricing,
	)
	return savings, cost, priced, contributes, err
}

func duckActivityUsageHasOrdinal(v any) bool {
	_, ok := duckUsageOrdinal(v)
	return ok
}

func duckActivityUsageCost(
	r duckActivityReportUsageRow, pricing *export.PricingResolver,
) (cost money.Money, costSource export.CostSource, priced, contributes bool,
	sessionCost *money.Money, err error) {
	costRow := r
	if r.costSource == db.CopilotReportedCostSource && r.cost != nil {
		v := money.Money{Microdollars: *r.cost}
		sessionCost = &v
		costRow.cost = nil
		pricing.RecordUnattributedReported()
	}
	_, cost, priced, contributes, err =
		duckActivityReportRowStatus(costRow, pricing)
	costSource = export.CostSourceComputed
	if costRow.cost != nil {
		costSource = export.CostSourceReported
	}
	return
}

// duckUsageOrdinal extracts a non-negative message ordinal from a
// scanned value (DuckDB returns NULL message_ordinal for some usage
// events). ok is false when the value is NULL or not an integer.
func duckUsageOrdinal(v any) (int64, bool) {
	switch n := v.(type) {
	case nil:
		return 0, false
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}
