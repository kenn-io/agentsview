package duckdb

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/readbase"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// activityUsageModels selects the (model, provider_id) pairs usage rows can
// carry. A pair it misses costs one more read of the rows.
const activityUsageModels = `SELECT DISTINCT model, provider_id FROM messages WHERE model != ''
	UNION SELECT DISTINCT model, provider_id FROM usage_events WHERE model != ''`

// sumActivityUsage selects, prices, and sums the usage survivors of an
// Activity report in SQL, and records the pricing it used on pricing.
//
// The usage rows come from duckActivityUsageWith over the bounds, for the
// candidate sessions candidateWhere selects. Its activity_usage_rows carry
// session_id, ord (-1 without a message ordinal), has_ordinal (0 or 1),
// ts_us, pricing_us (activityUsageNullPricingUS without a pricing
// timestamp), source, model, provider_id, agent, claude_message_id,
// claude_request_id, source_uuid, usage_dedup_key, input, output, reasoning,
// cache_create, cache_create_1h, cache_read, web, cost, cost_source, and
// recorded_cost. Timestamps are microseconds since the epoch, counters are
// 64-bit integers, and cost and recorded_cost are nullable microdollars:
// cost is a reported or Copilot session cost, and recorded_cost is a
// harness-recorded cost (usagefacts.CostStatusRecorded) the catalog may
// override. Rows outside the report range may be included; the query drops
// them.
//
// The query applies the report's rules in order: rows must carry a
// timestamp inside the report range and before its effective end; each
// Claude response keeps its fullest snapshot, credited to the earliest
// transcript holding it and billed for the most web searches any snapshot
// reported; survivors credited to sessions outside the candidates drop out;
// then the first row in report order keeps each source UUID or usage key.
// Each survivor is priced exactly as activity pricing prices one row, and
// survivors are summed per session, model, and bucket window. A session
// carrying a Copilot session cost returns its survivors one by one, in
// report order, for activity.AllocateUsageCosts to apportion.
func (s *Store) sumActivityUsage(
	ctx context.Context, candidateWhere string, candidateArgs []any, lowerBound, upperBound string,
	q activity.Query, pricing *export.PricingResolver,
) ([]activity.UsageRow, error) {
	startUS, endUS := readbase.ActivityUsageSpan(q)
	if startUS >= endUS {
		return []activity.UsageRow{}, nil
	}
	with := duckActivityUsageWith(candidateWhere)
	withArgs := append([]any{lowerBound, upperBound}, candidateArgs...)
	withArgs = append(withArgs, int64(readbase.ActivityUsageNullPricingUS))
	keys, err := s.activityUsageRateKeys(ctx)
	if err != nil {
		return nil, err
	}
	schedule, groups, err := readbase.PriceActivityUsageGroups(pricing, keys, startUS, endUS,
		func(schedule readbase.UsageRateSchedule) ([]readbase.ActivityUsageGroup, error) {
			return s.readActivityUsageGroups(ctx, with, withArgs, schedule, q, startUS, endUS)
		})
	if err != nil {
		return nil, err
	}
	return readbase.AssembleActivityUsage(pricing, schedule, groups)
}

// readActivityUsageGroups runs the usage query under schedule.
func (s *Store) readActivityUsageGroups(
	ctx context.Context, with string, withArgs []any, schedule readbase.UsageRateSchedule,
	q activity.Query, startUS, endUS int64,
) ([]readbase.ActivityUsageGroup, error) {
	sqlText, args := activityUsageSQL(with, withArgs, schedule, readbase.ActivityUsageBucketStarts(q), startUS, endUS)
	rows, err := s.queryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("querying activity usage: %w", err)
	}
	defer rows.Close()
	var groups []readbase.ActivityUsageGroup
	for rows.Next() {
		var g readbase.ActivityUsageGroup
		var raw, overflow int64
		if err := rows.Scan(
			&g.Session, &g.Model, &g.ProviderID, &g.Bucket, &g.Rate, &g.Kind, &g.Band,
			&raw, &g.RawTS, &g.RawSession, &g.RawOrd, &g.RawSource, &g.RawKey,
			&g.Count, &g.Input, &g.Output, &g.Cost, &overflow, &g.MinTS, &g.SessionCost,
		); err != nil {
			return nil, fmt.Errorf("scanning activity usage: %w", err)
		}
		g.Raw, g.Overflow = raw == 1, overflow != 0
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating activity usage: %w", err)
	}
	return groups, nil
}

// activityUsageRateKeys reads the pairs activityUsageModels selects.
func (s *Store) activityUsageRateKeys(ctx context.Context) ([]readbase.UsageRateKey, error) {
	rows, err := s.queryContext(ctx, activityUsageModels)
	if err != nil {
		return nil, fmt.Errorf("querying activity usage models: %w", err)
	}
	defer rows.Close()
	var keys []readbase.UsageRateKey
	for rows.Next() {
		var key readbase.UsageRateKey
		if err := rows.Scan(&key.Model, &key.ProviderID); err != nil {
			return nil, fmt.Errorf("scanning activity usage models: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating activity usage models: %w", err)
	}
	slices.SortFunc(keys, readbase.CompareUsageRateKeys)
	return slices.Compact(keys), nil
}

// activityUsageReportOrder orders rows as the report reads them.
const activityUsageReportOrder = `ts_us, session_id, ord, source, model, input, output,
	cache_create, cache_read, reasoning, cost NULLS FIRST, cost_source,
	claude_message_id, claude_request_id, source_uuid, usage_dedup_key`

// activityUsageSnapshotPartition groups the snapshots of one Claude response.
// A row without a response ID forms its own group.
const activityUsageSnapshotPartition = `PARTITION BY claude_message_id, claude_request_id,
	CASE WHEN claude_message_id = '' THEN session_id ELSE '' END,
	CASE WHEN claude_message_id = '' THEN ord ELSE -2 END`

// maxInt64SQL is the largest cost a row or sum may carry.
const maxInt64SQL = "9223372036854775807"

// Columns of the relations activityUsageSQL binds, as "name TYPE".
var (
	usageSegmentColumns = []string{
		"rate_id BIGINT", "rate_model VARCHAR", "rate_provider VARCHAR", "from_us BIGINT",
		"billed_ok BIGINT", "catalog_ok BIGINT", "billed_err BIGINT", "rate_in BIGINT",
		"rate_out BIGINT", "rate_cw BIGINT", "rate_cw1h BIGINT", "rate_cr BIGINT",
	}
	usageBandColumns = []string{
		"band_rate BIGINT", "band_floor BIGINT", "band_above BIGINT", "band_in BIGINT",
		"band_out BIGINT", "band_cw BIGINT", "band_cw1h BIGINT", "band_cr BIGINT",
	}
	usageBucketColumns = []string{"bucket_start BIGINT", "bucket BIGINT"}
)

// duckValuesRelation renders a relation of rows of placeholders, bound in
// row-major order, with columns given as "name TYPE".
func duckValuesRelation(columns []string, rows int) string {
	names := make([]string, len(columns))
	casts := make([]string, len(columns))
	for i, column := range columns {
		name, typ, _ := strings.Cut(column, " ")
		names[i] = name
		casts[i] = "CAST(? AS " + typ + ")"
	}
	row := "(" + strings.Join(casts, ", ") + ")"
	return "(SELECT * FROM (VALUES " + strings.TrimSuffix(strings.Repeat(row+", ", rows), ", ") +
		") AS v(" + strings.Join(names, ", ") + "))"
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func activityUsageSQL(
	with string, withArgs []any, schedule readbase.UsageRateSchedule,
	bucketStarts []int64, startUS, endUS int64,
) (string, []any) {
	args := slices.Clone(withArgs)
	args = append(args, startUS, endUS)

	var segmentArgs, bandArgs, bucketArgs []any
	for _, seg := range schedule.Segments() {
		segmentArgs = append(segmentArgs, seg.ID, seg.Model, seg.ProviderID, seg.FromUS,
			boolInt(seg.BilledOK), boolInt(seg.CatalogOK), boolInt(seg.BilledErr),
			seg.Input, seg.Output, seg.CacheWrite, seg.CacheWrite1h, seg.CacheRead)
	}
	bands := schedule.Bands()
	for _, band := range bands {
		bandArgs = append(bandArgs, band.RateID, band.Floor, band.Above,
			band.Input, band.Output, band.CacheWrite, band.CacheWrite1h, band.CacheRead)
	}
	// A relation needs a row. These match no usage row: a NULL model equals
	// nothing, and no rate has ID -1.
	segments := len(schedule.Rates)
	if segments == 0 {
		segmentArgs = []any{
			int64(-1), nil, nil, int64(0), int64(0), int64(0), int64(0),
			int64(0), int64(0), int64(0), int64(0), int64(0),
		}
		segments = 1
	}
	bandRows := len(bands)
	if bandRows == 0 {
		bandArgs = []any{int64(-1), int64(math.MaxInt64), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)}
		bandRows = 1
	}
	for i, start := range bucketStarts {
		bucketArgs = append(bucketArgs, start, int64(i))
	}

	requestSources := make([]string, len(usagefacts.RequestScopedSources))
	for i, source := range usagefacts.RequestScopedSources {
		requestSources[i] = "'" + source + "'"
	}
	requestScoped := "source IN (" + strings.Join(requestSources, ", ") + ") OR source LIKE '" +
		usagefacts.RequestScopedSourcePrefix + "%' OR has_ordinal = 1"
	// HUGEINT keeps exact money arithmetic from overflowing before the
	// final sums are checked against the int64 range.
	wide := func(expr string) string { return "CAST(" + expr + " AS HUGEINT)" }
	bout := "CASE WHEN output = 0 THEN reasoning ELSE output END"
	cw1h := "CASE WHEN cache_create_1h < cache_create THEN cache_create_1h ELSE cache_create END"
	pick := func(band, base string) string {
		return "CASE WHEN band_rate IS NOT NULL THEN " + band + " ELSE " + base + " END"
	}
	tokenCost := "(" + wide("input") + " * " + pick("band_in", "rate_in") + " + " +
		wide(bout) + " * " + pick("band_out", "rate_out") + " + " +
		wide("cache_create - "+cw1h) + " * " + pick("band_cw", "rate_cw") + " + " +
		wide(cw1h) + " * " + pick("band_cw1h", "rate_cw1h") + " + " +
		wide("cache_read") + " * " + pick("band_cr", "rate_cr") + " + 500000) // 1000000"
	webFee := wide("web_billed") + " * " + strconv.FormatInt(export.WebSearchRequestMicrodollars, 10)
	costOverflows := "max(row_cost) > " + maxInt64SQL + " OR sum(row_cost) > " + maxInt64SQL

	query := "WITH " + with + `,
		activity_usage_eligible AS (
			SELECT * FROM activity_usage_rows WHERE ts_us >= ? AND ts_us < ?
		),
		activity_usage_snapshots AS (
			SELECT *,
				row_number() OVER (` + activityUsageSnapshotPartition + `
					ORDER BY output DESC, ts_us DESC, session_id DESC, ord DESC) AS snapshot_rank,
				first_value(session_id) OVER (` + activityUsageSnapshotPartition + `
					ORDER BY ts_us, session_id, ord) AS snapshot_session,
				max(web) OVER (` + activityUsageSnapshotPartition + `) AS snapshot_web
			FROM activity_usage_eligible
		),
		activity_usage_kept AS (
			SELECT *,
				CASE WHEN claude_message_id = '' THEN session_id ELSE snapshot_session END AS account_session,
				CASE WHEN claude_message_id = '' THEN web ELSE snapshot_web END AS web_billed,
				CASE
					WHEN claude_message_id != '' THEN 1
					WHEN agent != '' AND source_uuid != '' THEN 2
					WHEN usage_dedup_key != '' THEN 3
					ELSE 0
				END AS token_kind
			FROM activity_usage_snapshots
			WHERE claude_message_id = '' OR snapshot_rank = 1
		),
		activity_usage_allowed AS (
			SELECT *,
				CASE token_kind WHEN 1 THEN claude_message_id WHEN 2 THEN agent
					WHEN 3 THEN usage_dedup_key ELSE session_id END AS token_a,
				CASE token_kind WHEN 1 THEN claude_request_id WHEN 2 THEN source_uuid
					ELSE '' END AS token_b,
				CASE token_kind WHEN 0 THEN ord ELSE 0 END AS token_c
			FROM activity_usage_kept
			WHERE account_session IN (SELECT id FROM activity_usage_candidates)
		),
		activity_usage_deduped AS (
			SELECT *,
				row_number() OVER (PARTITION BY token_kind, token_a, token_b, token_c
					ORDER BY ` + activityUsageReportOrder + `) AS token_rank
			FROM activity_usage_allowed
		),
		activity_usage_survivors AS (
			SELECT * FROM activity_usage_deduped WHERE token_kind = 0 OR token_rank = 1
		),
		activity_usage_rated AS (
			SELECT s.*, r.rate_id, r.billed_ok, r.catalog_ok, r.billed_err,
				r.rate_in, r.rate_out, r.rate_cw, r.rate_cw1h, r.rate_cr
			FROM activity_usage_survivors s
			ASOF LEFT JOIN ` + duckValuesRelation(usageSegmentColumns, segments) + ` r
				ON s.model = r.rate_model AND s.provider_id = r.rate_provider AND s.pricing_us >= r.from_us
		),
		activity_usage_classified AS (
			SELECT *,
				CASE WHEN cost IS NOT NULL AND cost_source = '` + db.CopilotReportedCostSource + `' THEN cost END AS session_cost,
				CASE
					WHEN cost IS NOT NULL AND cost_source != '` + db.CopilotReportedCostSource + `' THEN cost
					WHEN cost IS NULL AND recorded_cost > 0 AND COALESCE(catalog_ok, 0) = 0 THEN recorded_cost
				END AS reported_cost
			FROM activity_usage_rated
		),
		activity_usage_kinds AS (
			SELECT *,
				CASE
					WHEN rate_id IS NULL THEN '` + readbase.UsageKindMissingRate + `'
					WHEN reported_cost IS NOT NULL THEN '` + readbase.UsageKindReported + `'
					WHEN input = 0 AND output = 0 AND reasoning = 0 AND cache_create = 0
						AND cache_read = 0 AND web_billed = 0 THEN '` + readbase.UsageKindZero + `'
					WHEN billed_err = 1 THEN '` + readbase.UsageKindBillingErr + `'
					WHEN billed_ok = 0 THEN '` + readbase.UsageKindUnresolved + `'
					WHEN ` + requestScoped + ` THEN '` + readbase.UsageKindRequest + `'
					ELSE '` + readbase.UsageKindAggregate + `'
				END AS price_kind
			FROM activity_usage_classified
		),
		activity_usage_banded AS (
			SELECT k.*, b.band_rate, b.band_above, b.band_in, b.band_out, b.band_cw, b.band_cw1h, b.band_cr
			FROM (
				SELECT *, CASE WHEN price_kind = '` + readbase.UsageKindRequest + `'
					THEN input + cache_create + cache_read ELSE -1 END AS band_probe
				FROM activity_usage_kinds
			) k
			ASOF LEFT JOIN ` + duckValuesRelation(usageBandColumns, bandRows) + ` b
				ON k.rate_id = b.band_rate AND k.band_probe >= b.band_floor
		),
		activity_usage_priced AS (
			SELECT *,
				CASE price_kind
					WHEN '` + readbase.UsageKindReported + `' THEN ` + wide("reported_cost") + `
					WHEN '` + readbase.UsageKindUnresolved + `' THEN ` + webFee + `
					WHEN '` + readbase.UsageKindRequest + `' THEN ` + tokenCost + ` + ` + webFee + `
					WHEN '` + readbase.UsageKindAggregate + `' THEN ` + tokenCost + ` + ` + webFee + `
					ELSE ` + wide("0") + `
				END AS row_cost,
				CASE WHEN band_rate IS NOT NULL THEN band_above ELSE -1 END AS price_band
			FROM activity_usage_banded
		),
		activity_usage_bucketed AS (
			SELECT p.*, w.bucket,
				max(CASE WHEN session_cost IS NULL THEN 0 ELSE 1 END)
					OVER (PARTITION BY account_session) AS session_has_cost
			FROM activity_usage_priced p
			ASOF LEFT JOIN ` + duckValuesRelation(usageBucketColumns, len(bucketStarts)) + ` w
				ON p.ts_us >= w.bucket_start
		),
		activity_usage_keyed AS (
			SELECT *,
				CASE WHEN session_has_cost = 1 THEN ts_us ELSE 0 END AS raw_ts,
				CASE WHEN session_has_cost = 1 THEN session_id ELSE '' END AS raw_session,
				CASE WHEN session_has_cost = 1 THEN ord ELSE 0 END AS raw_ord,
				CASE WHEN session_has_cost = 1 THEN source ELSE '' END AS raw_source,
				CASE WHEN session_has_cost = 1 THEN usage_dedup_key ELSE '' END AS raw_key
			FROM activity_usage_bucketed
		)
		SELECT account_session, model, provider_id, CAST(COALESCE(bucket, -1) AS BIGINT),
			CAST(COALESCE(rate_id, -1) AS BIGINT), price_kind, CAST(price_band AS BIGINT),
			CAST(max(session_has_cost) AS BIGINT), CAST(raw_ts AS BIGINT), raw_session,
			CAST(raw_ord AS BIGINT), raw_source, raw_key,
			CAST(count(*) AS BIGINT), CAST(sum(input) AS BIGINT), CAST(sum(output) AS BIGINT),
			CAST(CASE WHEN ` + costOverflows + ` THEN 0 ELSE sum(row_cost) END AS BIGINT),
			CAST(CASE WHEN ` + costOverflows + ` THEN 1 ELSE 0 END AS BIGINT),
			CAST(min(ts_us) AS BIGINT), max(session_cost)
		FROM activity_usage_keyed
		GROUP BY account_session, model, provider_id, bucket, rate_id, price_kind, price_band,
			raw_ts, raw_session, raw_ord, raw_source, raw_key`
	args = append(args, segmentArgs...)
	args = append(args, bandArgs...)
	args = append(args, bucketArgs...)
	return query, args
}
