package duckdb

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// activityUsageNullPricingUS is the pricing_us of a usage row without a
// pricing timestamp. Such a row prices as usage without a timestamp.
const activityUsageNullPricingUS = math.MinInt64

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
	end := q.RangeEnd
	if q.EffectiveEnd.Before(end) {
		end = q.EffectiveEnd
	}
	startUS, endUS := ceilMicros(q.RangeStart), ceilMicros(end)
	if startUS >= endUS {
		return []activity.UsageRow{}, nil
	}
	with := duckActivityUsageWith(candidateWhere)
	withArgs := append([]any{lowerBound, upperBound}, candidateArgs...)
	withArgs = append(withArgs, int64(activityUsageNullPricingUS))
	keys, err := s.activityUsageRateKeys(ctx)
	if err != nil {
		return nil, err
	}
	// The catalog may lack a model a push has not registered yet; such a
	// row comes back unpriced, and the read runs again with its model.
	var groups []activityUsageGroup
	var schedule usageRateSchedule
	for attempt := 0; ; attempt++ {
		schedule = buildUsageRateSchedule(pricing, keys, startUS, endUS)
		var missing []usageRateKey
		groups, missing, err = s.readActivityUsageGroups(ctx, with, withArgs, schedule, q, startUS, endUS)
		if err != nil {
			return nil, err
		}
		if len(missing) == 0 {
			break
		}
		if attempt == 2 {
			return nil, fmt.Errorf("pricing activity usage for model %q: no rate was resolved for it", missing[0].model)
		}
		keys = append(keys, missing...)
	}
	var out []activity.UsageRow
	var raw []activityUsageGroup
	type recordKey struct {
		rate int64
		kind string
		band int64
	}
	records := map[recordKey]int{}
	unattributed := false
	for _, g := range groups {
		if err := g.check(schedule); err != nil {
			return nil, err
		}
		records[recordKey{g.rate, g.kind, g.band}] += int(g.count)
		if g.sessionCost != nil {
			unattributed = true
		}
		if g.raw == 1 {
			raw = append(raw, g)
			continue
		}
		out = append(out, g.usageRow())
	}
	if unattributed {
		pricing.RecordUnattributedReported()
	}
	for key, n := range records {
		replayUsagePricing(pricing, schedule.rates[key.rate], key.kind, key.band, n)
	}
	// A session's carrier is its last session cost in report order, and
	// its remainder goes to its last row, so its rows stay in that order.
	// A row of such a session is its own group, keyed by its report order.
	slices.SortFunc(raw, func(a, b activityUsageGroup) int {
		return cmp.Or(
			cmp.Compare(a.session, b.session), cmp.Compare(a.rawTS, b.rawTS),
			cmp.Compare(a.rawSession, b.rawSession), cmp.Compare(a.rawOrd, b.rawOrd),
			cmp.Compare(a.rawSource, b.rawSource), cmp.Compare(a.rawKey, b.rawKey))
	})
	for _, g := range raw {
		out = append(out, g.usageRow())
	}
	return out, nil
}

// readActivityUsageGroups runs the usage query under schedule. It returns
// the models of rows the schedule has no rate for instead of their groups.
func (s *Store) readActivityUsageGroups(
	ctx context.Context, with string, withArgs []any, schedule usageRateSchedule,
	q activity.Query, startUS, endUS int64,
) ([]activityUsageGroup, []usageRateKey, error) {
	sqlText, args := activityUsageSQL(with, withArgs, schedule, activity.QueryWindows(q), startUS, endUS)
	rows, err := s.queryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("querying activity usage: %w", err)
	}
	defer rows.Close()
	var groups []activityUsageGroup
	var missing []usageRateKey
	for rows.Next() {
		var g activityUsageGroup
		if err := rows.Scan(
			&g.session, &g.model, &g.providerID, &g.bucket, &g.rate, &g.kind, &g.band,
			&g.raw, &g.rawTS, &g.rawSession, &g.rawOrd, &g.rawSource, &g.rawKey,
			&g.count, &g.input, &g.output, &g.cost, &g.overflow, &g.minTS, &g.sessionCost,
		); err != nil {
			return nil, nil, fmt.Errorf("scanning activity usage: %w", err)
		}
		if g.kind == usageKindMissingRate {
			missing = append(missing, usageRateKey{model: g.model, providerID: g.providerID})
			continue
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterating activity usage: %w", err)
	}
	slices.SortFunc(missing, compareUsageRateKeys)
	return groups, slices.Compact(missing), nil
}

// Pricing kinds. Each survivor makes exactly one.
const (
	usageKindReported    = "r" // a reported cost prices it
	usageKindZero        = "z" // no tokens and no web searches
	usageKindUnresolved  = "u" // no catalog rate
	usageKindRequest     = "q" // one request, priced with bands
	usageKindAggregate   = "a" // a sum of requests, priced at base rates
	usageKindBillingErr  = "e" // the provider adjustment failed
	usageKindMissingRate = "m" // no rate was sent for its model
)

type activityUsageGroup struct {
	session, model, providerID, kind            string
	bucket, rate, band                          int64
	raw, rawTS, rawOrd                          int64
	rawSession, rawSource, rawKey               string
	count, input, output, cost, overflow, minTS int64
	sessionCost                                 *int64
}

func (g activityUsageGroup) check(schedule usageRateSchedule) error {
	switch {
	case g.rate < 0 || int(g.rate) >= len(schedule.rates):
		return fmt.Errorf("pricing activity usage for model %q: unknown rate %d", g.model, g.rate)
	case g.kind == usageKindBillingErr:
		return schedule.rates[g.rate].BilledErr
	case g.overflow != 0:
		return fmt.Errorf("pricing activity usage for model %q: %w", g.model, money.ErrOverflow)
	}
	return nil
}

func (g activityUsageGroup) usageRow() activity.UsageRow {
	row := activity.UsageRow{
		SessionID:    g.session,
		Model:        g.model,
		Timestamp:    time.UnixMicro(g.minTS).UTC().Format(time.RFC3339Nano),
		InputTokens:  int(g.input),
		OutputTokens: int(g.output),
		Cost:         money.Money{Microdollars: g.cost},
		CostSource:   export.CostSourceComputed,
		Priced:       g.kind != usageKindUnresolved,
		Contributes:  g.kind != usageKindZero,
	}
	if g.kind == usageKindReported {
		row.CostSource = export.CostSourceReported
	}
	if g.sessionCost != nil {
		row.SessionCost = &money.Money{Microdollars: *g.sessionCost}
	}
	return row
}

// replayUsagePricing records n survivors priced under rate as kind, as
// pricing each of them one by one would record them.
func replayUsagePricing(
	pricing *export.PricingResolver, rate usageRate, kind string, band int64, n int,
) {
	switch kind {
	case usageKindReported:
		pricing.RecordResolvedReported(rate.model, rate.CatalogModel, rate.Catalog)
	case usageKindUnresolved:
		pricing.RecordResolvedComputed(rate.model, rate.BilledModel, rate.Billed)
	case usageKindRequest:
		var threshold *int
		if band >= 0 {
			b := int(band)
			threshold = &b
		}
		pricing.RecordResolvedCounts(rate.model, rate.BilledModel, rate.Billed, 0, n, 0, threshold)
	case usageKindAggregate:
		pricing.RecordResolvedCounts(rate.model, rate.BilledModel, rate.Billed, 0, 0, n, nil)
	}
}

type usageRateKey struct{ model, providerID string }

// usageRate is one resolution of a model and provider, valid from fromUS
// until the next segment of the same key.
type usageRate struct {
	export.UsageRate
	model, providerID string
	fromUS            int64
}

type usageRateSchedule struct {
	rates []usageRate
}

// buildUsageRateSchedule resolves every key over [startUS, endUS) and for
// usage without a timestamp. A segment starts at the range start and at
// every instant the resolver says a price can change; adjacent segments
// that resolve alike merge.
func buildUsageRateSchedule(
	pricing *export.PricingResolver, keys []usageRateKey, startUS, endUS int64,
) usageRateSchedule {
	starts := []int64{startUS}
	for _, change := range pricing.PriceChanges(time.UnixMicro(startUS), time.UnixMicro(endUS)) {
		starts = append(starts, ceilMicros(change))
	}
	starts = slices.Compact(starts)
	var schedule usageRateSchedule
	for _, key := range keys {
		schedule.rates = append(schedule.rates, usageRate{
			UsageRate: pricing.UsageRateAt(key.providerID, key.model, time.Time{}),
			model:     key.model, providerID: key.providerID,
			fromUS: activityUsageNullPricingUS,
		})
		var previous *export.UsageRate
		for i, from := range starts {
			rate := pricing.UsageRateAt(key.providerID, key.model, time.UnixMicro(from).UTC())
			if previous != nil && rate.Equal(*previous) {
				continue
			}
			if i == 0 {
				// Real timestamps sort after every row without one.
				from = activityUsageNullPricingUS + 1
			}
			schedule.rates = append(schedule.rates, usageRate{
				UsageRate: rate, model: key.model, providerID: key.providerID, fromUS: from,
			})
			previous = &schedule.rates[len(schedule.rates)-1].UsageRate
		}
	}
	return schedule
}

// activityUsageRateKeys reads the pairs activityUsageModels selects.
func (s *Store) activityUsageRateKeys(ctx context.Context) ([]usageRateKey, error) {
	rows, err := s.queryContext(ctx, activityUsageModels)
	if err != nil {
		return nil, fmt.Errorf("querying activity usage models: %w", err)
	}
	defer rows.Close()
	var keys []usageRateKey
	for rows.Next() {
		var key usageRateKey
		if err := rows.Scan(&key.model, &key.providerID); err != nil {
			return nil, fmt.Errorf("scanning activity usage models: %w", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating activity usage models: %w", err)
	}
	slices.SortFunc(keys, compareUsageRateKeys)
	return slices.Compact(keys), nil
}

func compareUsageRateKeys(a, b usageRateKey) int {
	return cmp.Or(cmp.Compare(a.model, b.model), cmp.Compare(a.providerID, b.providerID))
}

// ceilMicros returns the first whole microsecond at or after t, so a
// microsecond timestamp is at or after t exactly when it is at or after
// ceilMicros(t).
func ceilMicros(t time.Time) int64 {
	us := t.UnixMicro()
	if time.UnixMicro(us).Before(t) {
		us++
	}
	return us
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
	with string, withArgs []any, schedule usageRateSchedule,
	windows []activity.BucketWindow, startUS, endUS int64,
) (string, []any) {
	args := slices.Clone(withArgs)
	args = append(args, startUS, endUS)

	var segmentArgs, bandArgs, bucketArgs []any
	bands := 0
	for id, rate := range schedule.rates {
		r := rate.Billed.Rates
		segmentArgs = append(segmentArgs, int64(id), rate.model, rate.providerID, rate.fromUS,
			boolInt(rate.Billed.OK), boolInt(rate.Catalog.OK), boolInt(rate.BilledErr != nil),
			r.InputPerMTok.Microdollars, r.OutputPerMTok.Microdollars, r.CacheWritePerMTok.Microdollars,
			r.EffectiveCacheWrite1hPerMTok().Microdollars, r.CacheReadPerMTok.Microdollars)
		seen := map[int]bool{}
		for _, band := range r.Bands {
			// The first band with a threshold prices it.
			if seen[band.AboveInputTokens] {
				continue
			}
			seen[band.AboveInputTokens] = true
			bandRates := export.ModelRates{
				CacheWritePerMTok: band.CacheWritePerMTok, CacheWrite1hPerMTok: band.CacheWrite1hPerMTok,
			}
			bandArgs = append(bandArgs, int64(id), int64(band.AboveInputTokens)+1, int64(band.AboveInputTokens),
				band.InputPerMTok.Microdollars, band.OutputPerMTok.Microdollars, band.CacheWritePerMTok.Microdollars,
				bandRates.EffectiveCacheWrite1hPerMTok().Microdollars, band.CacheReadPerMTok.Microdollars)
			bands++
		}
	}
	// A relation needs a row. These match no usage row: a NULL model equals
	// nothing, and no rate has ID -1.
	segments := len(schedule.rates)
	if segments == 0 {
		segmentArgs = []any{
			int64(-1), nil, nil, int64(0), int64(0), int64(0), int64(0),
			int64(0), int64(0), int64(0), int64(0), int64(0),
		}
		segments = 1
	}
	if bands == 0 {
		bandArgs = []any{int64(-1), int64(math.MaxInt64), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)}
		bands = 1
	}
	for i, window := range windows {
		bucketArgs = append(bucketArgs, ceilMicros(window.Start), int64(i))
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
					WHEN rate_id IS NULL THEN '` + usageKindMissingRate + `'
					WHEN reported_cost IS NOT NULL THEN '` + usageKindReported + `'
					WHEN input = 0 AND output = 0 AND reasoning = 0 AND cache_create = 0
						AND cache_read = 0 AND web_billed = 0 THEN '` + usageKindZero + `'
					WHEN billed_err = 1 THEN '` + usageKindBillingErr + `'
					WHEN billed_ok = 0 THEN '` + usageKindUnresolved + `'
					WHEN ` + requestScoped + ` THEN '` + usageKindRequest + `'
					ELSE '` + usageKindAggregate + `'
				END AS price_kind
			FROM activity_usage_classified
		),
		activity_usage_banded AS (
			SELECT k.*, b.band_rate, b.band_above, b.band_in, b.band_out, b.band_cw, b.band_cw1h, b.band_cr
			FROM (
				SELECT *, CASE WHEN price_kind = '` + usageKindRequest + `'
					THEN input + cache_create + cache_read ELSE -1 END AS band_probe
				FROM activity_usage_kinds
			) k
			ASOF LEFT JOIN ` + duckValuesRelation(usageBandColumns, bands) + ` b
				ON k.rate_id = b.band_rate AND k.band_probe >= b.band_floor
		),
		activity_usage_priced AS (
			SELECT *,
				CASE price_kind
					WHEN '` + usageKindReported + `' THEN ` + wide("reported_cost") + `
					WHEN '` + usageKindUnresolved + `' THEN ` + webFee + `
					WHEN '` + usageKindRequest + `' THEN ` + tokenCost + ` + ` + webFee + `
					WHEN '` + usageKindAggregate + `' THEN ` + tokenCost + ` + ` + webFee + `
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
			ASOF LEFT JOIN ` + duckValuesRelation(usageBucketColumns, len(windows)) + ` w
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
