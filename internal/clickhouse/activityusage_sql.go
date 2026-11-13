package clickhouse

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unsafe"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/readbase"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// activityUsageSource is the SQL of the usage rows an Activity report reads,
// in the columns of chPreparedUsageColumns, with its arguments, the
// settings it needs, and the external tables it names. tables builds a
// fresh set for each query, since a query consumes the tables it sends.
type activityUsageSource struct {
	sql      string
	args     []any
	settings string
	tables   func() ([]*ext.Table, error)
}

// activityUsageSourceFor returns the usage rows in the inclusive range
// bounds: prepared rows when state is ready, else the rows of the raw usage
// tables. A raw read takes every candidate row and the Claude message rows
// of every session; the query only needs the latter for the Claude pairs
// candidates hold.
func activityUsageSourceFor(state preparedUsageState, lowerBound, upperBound string) activityUsageSource {
	if state.ready {
		rangeSQL, rangeArgs := chPreparedActivityRangeSQL(lowerBound, upperBound)
		sourceSQL, sourceArgs := chPreparedUsageSourceSQL(state, rangeSQL, rangeArgs)
		return activityUsageSource{
			sql: sourceSQL, args: sourceArgs,
			tables: func() ([]*ext.Table, error) { return usageDeltaTables(state) },
		}
	}
	messageBound := " AND COALESCE(m.timestamp, s.started_at) >= " + chTimestampSQL +
		" AND COALESCE(m.timestamp, s.started_at) <= " + chTimestampSQL
	eventBound := " AND COALESCE(ue.occurred_at, s.started_at) >= " + chTimestampSQL +
		" AND COALESCE(ue.occurred_at, s.started_at) <= " + chTimestampSQL
	timestampBound := "(m.timestamp IS NULL OR (m.timestamp >= " + chTimestampSQL +
		" AND m.timestamp <= " + chTimestampSQL + "))"
	query := clickUsageNormalizedQuery(
		timestampBound+" AND "+chUsageStoredMessageEligibility+`
			AND (s.id IN (SELECT id FROM activity_usage_candidates) OR m.claude_message_id != '')`+messageBound,
		chUsageEventEligibility+" AND s.id IN (SELECT id FROM activity_usage_candidates)"+eventBound,
	)
	var args []any
	for range 3 {
		args = append(args, lowerBound, upperBound)
	}
	return activityUsageSource{
		sql: query, args: args,
		// Exact index filtering also reads overlapping newer parts before
		// FINAL. A timestamp correction must not resurrect an older
		// matching version.
		settings: "optimize_move_to_prewhere_if_final = 0, use_skip_indexes_if_final_exact_mode = 1",
		tables:   func() ([]*ext.Table, error) { return nil, nil },
	}
}

// keptActivityUsageSums returns the usage of readActivityUsageSums, reusing the sums
// of an earlier report over the same candidates, buckets, and pricing until
// a push changes the mirror's parts. A range in progress moves its effective
// end with every request; its sums still answer while no source row lies
// between the end they were read to and the new one.
func (s *Store) keptActivityUsageSums(
	ctx context.Context, source activityUsageSource, candidates chSessionSet, ids []string,
	lowerBound, upperBound string, q activity.Query, pricing *export.PricingResolver, pricingDigest string,
) ([]activity.UsageRow, error) {
	startUS, endUS := activityUsageSpan(q)
	if startUS >= endUS {
		return []activity.UsageRow{}, nil
	}
	// The parts are those of the request's snapshot, taken before any read,
	// so a push that lands during the read makes the next request miss.
	version, err := s.partsFingerprint(ctx)
	if err != nil {
		return nil, err
	}
	// The range, zone, and bucket decide the bucket windows.
	slot := fmt.Sprintf("%s|%s|%d|%d|%s|%+v|%s|%s", lowerBound, upperBound, q.RangeStart.UnixNano(), q.RangeEnd.UnixNano(),
		q.Loc, q.Bucket, pricingDigest, chSessionIDsDigest(ids))
	if kept, ok := s.activityUsageSums.Get(slot, version); ok && kept[0].endUS <= endUS && endUS <= kept[0].nextUS {
		return readbase.AssembleActivityUsage(pricing, kept[0].schedule, kept[0].groups)
	}
	sums, err := s.readActivityUsageSums(ctx, source, candidates, q, pricing, startUS, endUS)
	if err != nil {
		return nil, err
	}
	s.activityUsageSums.Put(slot, version, []*activityUsageSums{sums})
	return readbase.AssembleActivityUsage(pricing, sums.schedule, sums.groups)
}

// activityUsageSums is what one usage query returns: the groups, priced
// under schedule, of the survivors before endUS, and nextUS, the first
// source row at or after endUS, math.MaxInt64 when there is none.
type activityUsageSums struct {
	schedule      readbase.UsageRateSchedule
	groups        []readbase.ActivityUsageGroup
	endUS, nextUS int64
}

func (u *activityUsageSums) bytes() int64 {
	n := int64(unsafe.Sizeof(*u)) + int64(cap(u.schedule.Rates))*int64(unsafe.Sizeof(readbase.UsageRate{})) +
		int64(cap(u.groups))*int64(unsafe.Sizeof(readbase.ActivityUsageGroup{}))
	for _, rate := range u.schedule.Rates {
		n += kept.JSONBytes(rate)
	}
	for _, g := range u.groups {
		n += stringBytes(g.Session, g.Model, g.ProviderID, g.Kind, g.RawSession, g.RawSource, g.RawKey)
	}
	return n
}

// activityUsageSpan returns the microseconds a report's usage may fall in:
// from its range start up to the earlier of its range end and effective
// end.
func activityUsageSpan(q activity.Query) (startUS, endUS int64) {
	end := q.RangeEnd
	if q.EffectiveEnd.Before(end) {
		end = q.EffectiveEnd
	}
	return readbase.CeilMicros(q.RangeStart), readbase.CeilMicros(end)
}

// readActivityUsageSums selects, prices, and sums in SQL the usage
// survivors of an Activity report over the candidate sessions in [startUS,
// endUS). It applies the rules of activity.SelectUsageSurvivorsForSessions
// to the rows a report reads:
//
//   - A row counts when its timestamp lies in the report range and before
//     its effective end.
//   - Each Claude response keeps its snapshot with the most output, then
//     the latest, then the one in the later session, then the first in
//     report order. The snapshot counts for the session of the response's
//     earliest row, with the most web searches any snapshot reported, and
//     drops out when that session is not a candidate. Rows of other
//     sessions take part only as message rows.
//   - Of the survivors that carry one dedup token (see
//     activity.UsageDedupToken), the first in report order counts.
//
// Each survivor is priced as clickActivityUsagePrice prices one row, at the
// rates in effect at its pricing timestamp, and the survivors are summed per
// session, model, and bucket window. A session that may carry a Copilot
// session cost returns its survivors one by one, in report order, for
// activity.AllocateUsageCosts to apportion.
func (s *Store) readActivityUsageSums(
	ctx context.Context, source activityUsageSource, candidates chSessionSet,
	q activity.Query, pricing *export.PricingResolver, startUS, endUS int64,
) (*activityUsageSums, error) {
	keys, nextUS, err := s.activityUsageRateKeys(ctx, source, candidates, endUS)
	if err != nil {
		return nil, err
	}
	bucketStarts := readbase.ActivityUsageBucketStarts(q)
	// A push may add a model between the two reads; its rows come back
	// unpriced, and the read runs again with its model.
	for attempt := 0; ; attempt++ {
		schedule := readbase.BuildUsageRateSchedule(pricing, keys, startUS, endUS)
		groups, err := s.readActivityUsageGroups(ctx, source, candidates, schedule, bucketStarts, startUS, endUS)
		if err != nil {
			return nil, err
		}
		missing := readbase.MissingUsageRates(groups)
		if len(missing) == 0 {
			readbase.SortActivityUsageGroups(groups)
			return &activityUsageSums{schedule: schedule, groups: groups, endUS: endUS, nextUS: nextUS}, nil
		}
		if attempt == 2 {
			return nil, fmt.Errorf("pricing clickhouse activity usage for model %q: no rate was resolved for it", missing[0].Model)
		}
		keys = append(keys, missing...)
	}
}

// activityUsageRateKeys reads the models and providers of the source rows
// and the first source row at or after endUS.
func (s *Store) activityUsageRateKeys(
	ctx context.Context, source activityUsageSource, candidates chSessionSet, endUS int64,
) ([]readbase.UsageRateKey, int64, error) {
	readCtx, err := activityUsageReadContext(ctx, source, nil)
	if err != nil {
		return nil, 0, err
	}
	tsUS := "toUnixTimestamp64Micro(assumeNotNull(ts))"
	query := `WITH activity_usage_candidates AS (` + candidates.body + `)
		SELECT model, provider_id, min(if(ts IS NOT NULL AND ` + tsUS + ` >= ?, ` + tsUS + `, toInt64(` +
		strconv.FormatInt(math.MaxInt64, 10) + `)))
		FROM (` + source.sql + `) GROUP BY model, provider_id`
	if source.settings != "" {
		query += " SETTINGS " + source.settings
	}
	args := slices.Concat(candidates.args, []any{endUS}, source.args)
	rows, err := s.bulk.Query(readCtx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("querying clickhouse activity usage models: %w", err)
	}
	defer rows.Close()
	var keys []readbase.UsageRateKey
	nextUS := int64(math.MaxInt64)
	for rows.Next() {
		var key readbase.UsageRateKey
		var next int64
		if err := rows.Scan(&key.Model, &key.ProviderID, &next); err != nil {
			return nil, 0, fmt.Errorf("scanning clickhouse activity usage models: %w", err)
		}
		keys = append(keys, key)
		nextUS = min(nextUS, next)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterating clickhouse activity usage models: %w", err)
	}
	slices.SortFunc(keys, readbase.CompareUsageRateKeys)
	return keys, nextUS, nil
}

// activityUsageReadContext attaches the source's external tables and extra
// to the context a read runs under.
func activityUsageReadContext(ctx context.Context, source activityUsageSource, extra []*ext.Table) (context.Context, error) {
	tables, err := source.tables()
	if err != nil {
		return nil, err
	}
	tables = append(tables, extra...)
	if len(tables) == 0 {
		return ctx, nil
	}
	return chdriver.Context(ctx, chdriver.WithExternalTable(tables...)), nil
}

// readActivityUsageGroups runs the usage query under schedule.
func (s *Store) readActivityUsageGroups(
	ctx context.Context, source activityUsageSource, candidates chSessionSet,
	schedule readbase.UsageRateSchedule, bucketStarts []int64, startUS, endUS int64,
) ([]readbase.ActivityUsageGroup, error) {
	tables, err := activityUsageScheduleTables(schedule, bucketStarts)
	if err != nil {
		return nil, err
	}
	readCtx, err := activityUsageReadContext(ctx, source, tables)
	if err != nil {
		return nil, err
	}
	query, args := clickActivityUsageSQL(source, candidates, startUS, endUS)
	s.activityUsageQueries.Add(1)
	rows, err := s.bulk.Query(readCtx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse activity usage: %w", err)
	}
	defer rows.Close()
	var groups []readbase.ActivityUsageGroup
	for rows.Next() {
		var g readbase.ActivityUsageGroup
		var raw, overflow uint8
		if err := rows.Scan(
			&g.Session, &g.Model, &g.ProviderID, &g.Bucket, &g.Rate, &g.Kind, &g.Band,
			&raw, &g.RawTS, &g.RawSession, &g.RawOrd, &g.RawTie,
			&g.Count, &g.Input, &g.Output, &g.Cost, &overflow, &g.MinTS, &g.SessionCost,
		); err != nil {
			return nil, fmt.Errorf("scanning clickhouse activity usage: %w", err)
		}
		g.Raw, g.Overflow = raw != 0, overflow != 0
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse activity usage: %w", err)
	}
	return groups, nil
}

// activityUsageScheduleTables returns the external tables the usage query
// prices with: the schedule's rate segments and bands, and the starts of
// the report's bucket windows.
func activityUsageScheduleTables(schedule readbase.UsageRateSchedule, bucketStarts []int64) ([]*ext.Table, error) {
	newTable := func(name string, columns ...[2]string) (*ext.Table, error) {
		declared := make([]func(*ext.Table) error, len(columns))
		for i, c := range columns {
			declared[i] = chExternalColumn(ext.Column, c[0], c[1])
		}
		table, err := ext.NewTable(name, declared...)
		if err != nil {
			return nil, fmt.Errorf("creating %s table: %w", name, err)
		}
		return table, nil
	}
	flag := func(b bool) uint8 {
		if b {
			return 1
		}
		return 0
	}
	rates, err := newTable("activity_usage_rates",
		[2]string{"rate_id", "Int64"}, [2]string{"rate_model", "String"}, [2]string{"rate_provider", "String"},
		[2]string{"from_us", "Int64"}, [2]string{"billed_ok", "UInt8"}, [2]string{"catalog_ok", "UInt8"},
		[2]string{"billed_err", "UInt8"}, [2]string{"rate_in", "Int64"}, [2]string{"rate_out", "Int64"},
		[2]string{"rate_cw", "Int64"}, [2]string{"rate_cw1h", "Int64"}, [2]string{"rate_cr", "Int64"})
	if err != nil {
		return nil, err
	}
	for _, seg := range schedule.Segments() {
		if err := rates.Append(seg.ID, seg.Model, seg.ProviderID, seg.FromUS,
			flag(seg.BilledOK), flag(seg.CatalogOK), flag(seg.BilledErr),
			seg.Input, seg.Output, seg.CacheWrite, seg.CacheWrite1h, seg.CacheRead); err != nil {
			return nil, fmt.Errorf("adding activity usage rate: %w", err)
		}
	}
	bands, err := newTable("activity_usage_bands",
		[2]string{"band_rate", "Int64"}, [2]string{"band_floor", "Int64"}, [2]string{"band_above", "Int64"},
		[2]string{"band_in", "Int64"}, [2]string{"band_out", "Int64"}, [2]string{"band_cw", "Int64"},
		[2]string{"band_cw1h", "Int64"}, [2]string{"band_cr", "Int64"})
	if err != nil {
		return nil, err
	}
	for _, band := range schedule.Bands() {
		if err := bands.Append(band.RateID, band.Floor, band.Above,
			band.Input, band.Output, band.CacheWrite, band.CacheWrite1h, band.CacheRead); err != nil {
			return nil, fmt.Errorf("adding activity usage band: %w", err)
		}
	}
	buckets, err := newTable("activity_usage_buckets",
		[2]string{"bucket_start", "Int64"}, [2]string{"bucket", "Int64"})
	if err != nil {
		return nil, err
	}
	for i, start := range bucketStarts {
		if err := buckets.Append(start, int64(i)); err != nil {
			return nil, fmt.Errorf("adding activity usage bucket: %w", err)
		}
	}
	return []*ext.Table{rates, bands, buckets}, nil
}

// clickActivityUsageTieSQL is a hash, in [0, 2^63), of the columns of a
// source row that report order does not compare. Rows with the same time,
// session, and ordinal come back from a read in no set order, so the query
// orders them by this hash: report order is ts_us, session_id, ord, tie.
var clickActivityUsageTieSQL = `toInt64(bitShiftRight(cityHash64(source, model, provider_id, agent,
	claude_message_id, claude_request_id, source_uuid, usage_dedup_key, input_tokens_norm,
	output_tokens_norm, cache_create_norm, cache_create_1h_norm, cache_read_norm,
	reasoning_tokens_norm, web_search_requests_norm, isNull(cost_microdollars),
	ifNull(cost_microdollars, 0), cost_source, isNull(recorded_cost_microdollars),
	ifNull(recorded_cost_microdollars, 0), ` + clickActivityUsagePricingUSSQL + `), 1))`

// clickActivityUsagePricingUSSQL is a source row's pricing timestamp in
// microseconds, readbase.ActivityUsageNullPricingUS when it has none.
var clickActivityUsagePricingUSSQL = `ifNull(toUnixTimestamp64Micro(pricing_ts), toInt64(` +
	strconv.FormatInt(readbase.ActivityUsageNullPricingUS, 10) + `))`

// clickActivityUsageColumns are the columns each survivor carries into
// pricing and report order.
const clickActivityUsageColumns = `session_id, ord, has_ord, ts_us, tie, pricing_us, source, model,
	provider_id, usage_dedup_key, input, output, cache_create, cache_create_1h, cache_read,
	reasoning, cost, cost_source, recorded_cost`

// clickActivityUsageSQL renders the usage query of readActivityUsageSums over
// source for the candidate sessions and the eligible microseconds
// [startUS, endUS). It reads the external tables of
// activityUsageScheduleTables.
func clickActivityUsageSQL(source activityUsageSource, candidates chSessionSet, startUS, endUS int64) (string, []any) {
	requestSources := make([]string, len(usagefacts.RequestScopedSources))
	for i, s := range usagefacts.RequestScopedSources {
		requestSources[i] = chSQLString(s)
	}
	requestScoped := "has_ord OR source IN (" + strings.Join(requestSources, ", ") +
		") OR startsWith(source, " + chSQLString(usagefacts.RequestScopedSourcePrefix) + ")"
	copilot := chSQLString(db.CopilotReportedCostSource)
	wide := func(expr string) string { return "toInt128(" + expr + ")" }
	cw1h := "least(cache_create_1h, cache_create)"
	pick := func(band, base string) string { return "if(band_found = 1, " + band + ", " + base + ")" }
	tokenCost := "intDiv(" + wide("input") + " * " + pick("band_in", "rate_in") + " + " +
		wide("if(output = 0, reasoning, output)") + " * " + pick("band_out", "rate_out") + " + " +
		wide("cache_create - "+cw1h) + " * " + pick("band_cw", "rate_cw") + " + " +
		wide(cw1h) + " * " + pick("band_cw1h", "rate_cw1h") + " + " +
		wide("cache_read") + " * " + pick("band_cr", "rate_cr") + " + 500000, 1000000)"
	webFee := wide("web_billed") + " * " + strconv.FormatInt(export.WebSearchRequestMicrodollars, 10)
	sessionCostRow := "cost_source = " + copilot + " AND cost IS NOT NULL"
	// A pair whose IDs hold no colon joins into a dedup token no other pair
	// shares, so only pairs with one need deduplicating by token.
	colon := "(position(claude_message_id, ':') > 0 OR position(claude_request_id, ':') > 0)"
	bestKey := "(output, ts_us, session_id, -ord, -tie)"
	snapshots := func(pairs string) string {
		return `SELECT ` + clickActivityUsageColumns + `, claude_message_id, claude_request_id, account_session, web_billed FROM (
				SELECT ` + clickActivityUsageColumns + `, claude_message_id, claude_request_id,
					max(` + bestKey + `) OVER pair AS best_key,
					argMin(session_id, (ts_us, session_id, ord)) OVER pair AS account_session,
					max(web) OVER pair AS web_billed
				FROM activity_usage_rows WHERE token_kind = 1 AND ` + pairs + `
				WINDOW pair AS (PARTITION BY claude_message_id, claude_request_id))
			WHERE ` + bestKey + ` = best_key AND account_session IN (SELECT id FROM activity_usage_candidates)`
	}
	const maxInt64, minInt64 = "toInt128(9223372036854775807)", "toInt128(-9223372036854775808)"
	overflows := "max(row_cost) > " + maxInt64 + " OR min(row_cost) < " + minInt64 +
		" OR sum(row_cost) > " + maxInt64 + " OR sum(row_cost) < " + minInt64

	query := `WITH activity_usage_candidates AS (` + candidates.body + `),
		activity_usage_source AS (` + source.sql + `),
		activity_usage_rows AS (
			SELECT session_id, toInt64(ifNull(message_ordinal, -1)) AS ord, message_ordinal IS NOT NULL AS has_ord,
				toUnixTimestamp64Micro(assumeNotNull(ts)) AS ts_us,
				` + clickActivityUsagePricingUSSQL + ` AS pricing_us, ` + clickActivityUsageTieSQL + ` AS tie,
				source, model, provider_id, agent, claude_message_id, claude_request_id, source_uuid, usage_dedup_key,
				input_tokens_norm AS input, output_tokens_norm AS output, cache_create_norm AS cache_create,
				cache_create_1h_norm AS cache_create_1h, cache_read_norm AS cache_read,
				reasoning_tokens_norm AS reasoning, web_search_requests_norm AS web,
				cost_microdollars AS cost, cost_source, recorded_cost_microdollars AS recorded_cost,
				session_id IN (SELECT id FROM activity_usage_candidates) AS candidate,
				multiIf(claude_message_id != '', 1, agent != '' AND source_uuid != '', 2, usage_dedup_key != '', 3, 0) AS token_kind
			FROM activity_usage_source
			WHERE ts IS NOT NULL AND ts_us >= ? AND ts_us < ?
				AND (candidate OR (source = 'message' AND claude_message_id != ''))
		),
		activity_usage_survivors AS (
			SELECT ` + clickActivityUsageColumns + `, session_id AS account_session, web AS web_billed
			FROM activity_usage_rows WHERE token_kind = 0
			UNION ALL
			SELECT ` + clickActivityUsageColumns + `, session_id AS account_session, web AS web_billed FROM (
				SELECT *, row_number() OVER (
					PARTITION BY token_kind, if(token_kind = 2, concat(agent, ':', source_uuid), usage_dedup_key)
					ORDER BY ts_us, session_id, ord, tie) AS token_rank
				FROM activity_usage_rows WHERE token_kind IN (2, 3))
			WHERE token_rank = 1
			UNION ALL
			SELECT ` + clickActivityUsageColumns + `, account_session, web_billed FROM (` + snapshots("NOT "+colon) + `)
			UNION ALL
			SELECT ` + clickActivityUsageColumns + `, account_session, web_billed FROM (
				SELECT *, row_number() OVER (PARTITION BY concat(claude_message_id, ':', claude_request_id)
					ORDER BY ts_us, session_id, ord, tie) AS token_rank
				FROM (` + snapshots(colon) + `))
			WHERE token_rank = 1
		),
		activity_usage_raw_sessions AS (
			SELECT session_id FROM activity_usage_rows WHERE ` + sessionCostRow + `
			UNION ALL
			SELECT argMin(session_id, (ts_us, session_id, ord)) FROM activity_usage_rows
			WHERE token_kind = 1 AND (claude_message_id, claude_request_id) IN (
				SELECT claude_message_id, claude_request_id FROM activity_usage_rows
				WHERE token_kind = 1 AND ` + sessionCostRow + `)
			GROUP BY claude_message_id, claude_request_id
		),
		activity_usage_rated AS (
			SELECT u.*, r.rate_found, r.rate_id, r.billed_ok, r.catalog_ok, r.billed_err,
				r.rate_in, r.rate_out, r.rate_cw, r.rate_cw1h, r.rate_cr
			FROM activity_usage_survivors u
			ASOF LEFT JOIN (SELECT *, toUInt8(1) AS rate_found FROM activity_usage_rates) r
				ON u.model = r.rate_model AND u.provider_id = r.rate_provider AND u.pricing_us >= r.from_us
		),
		activity_usage_classified AS (
			SELECT *,
				if(cost IS NOT NULL AND cost_source = ` + copilot + `, cost, NULL) AS session_cost,
				multiIf(cost IS NOT NULL AND cost_source != ` + copilot + `, cost,
					recorded_cost > 0 AND catalog_ok = 0, recorded_cost, NULL) AS reported_cost
			FROM activity_usage_rated
		),
		activity_usage_kinds AS (
			SELECT *,
				multiIf(
					rate_found = 0, '` + readbase.UsageKindMissingRate + `',
					reported_cost IS NOT NULL, '` + readbase.UsageKindReported + `',
					input = 0 AND output = 0 AND reasoning = 0 AND cache_create = 0
						AND cache_read = 0 AND web_billed = 0, '` + readbase.UsageKindZero + `',
					billed_err = 1, '` + readbase.UsageKindBillingErr + `',
					billed_ok = 0, '` + readbase.UsageKindUnresolved + `',
					` + requestScoped + `, '` + readbase.UsageKindRequest + `',
					'` + readbase.UsageKindAggregate + `') AS price_kind,
				if(price_kind = '` + readbase.UsageKindRequest + `', input + cache_create + cache_read, -1) AS band_probe
			FROM activity_usage_classified
		),
		activity_usage_banded AS (
			SELECT k.*, b.band_found, b.band_above, b.band_in, b.band_out, b.band_cw, b.band_cw1h, b.band_cr
			FROM activity_usage_kinds k
			ASOF LEFT JOIN (SELECT *, toUInt8(1) AS band_found FROM activity_usage_bands) b
				ON k.rate_id = b.band_rate AND k.band_probe >= b.band_floor
		),
		activity_usage_priced AS (
			SELECT *,
				multiIf(
					price_kind = '` + readbase.UsageKindReported + `', ` + wide("reported_cost") + `,
					price_kind = '` + readbase.UsageKindUnresolved + `', ` + webFee + `,
					price_kind IN ('` + readbase.UsageKindRequest + `', '` + readbase.UsageKindAggregate + `'), ` + tokenCost + ` + ` + webFee + `,
					` + wide("0") + `) AS row_cost,
				if(band_found = 1, band_above, -1) AS price_band,
				toUInt8(0) AS bucket_key,
				account_session IN (SELECT session_id FROM activity_usage_raw_sessions) AS raw
			FROM activity_usage_banded
		),
		activity_usage_bucketed AS (
			SELECT p.*, if(w.bucket_found = 1, w.bucket, -1) AS bucket_index
			FROM activity_usage_priced p
			ASOF LEFT JOIN (SELECT *, toUInt8(0) AS bucket_key, toUInt8(1) AS bucket_found FROM activity_usage_buckets) w
				ON p.bucket_key = w.bucket_key AND p.ts_us >= w.bucket_start
		)
		SELECT account_session, model, provider_id, bucket_index, if(rate_found = 1, rate_id, -1) AS rate,
			price_kind, toInt64(price_band) AS band, toUInt8(raw) AS raw_group,
			if(raw, ts_us, 0) AS raw_ts, if(raw, session_id, '') AS raw_session, if(raw, ord, 0) AS raw_ord,
			if(raw, tie, 0) AS raw_tie,
			toInt64(count()) AS rows, toInt64(sum(input)) AS input_sum, toInt64(sum(output)) AS output_sum,
			toInt64(if(` + overflows + `, 0, sum(row_cost))) AS cost_sum,
			toUInt8(` + overflows + `) AS overflow, min(ts_us) AS min_ts, max(session_cost) AS session_cost_max
		FROM activity_usage_bucketed
		GROUP BY account_session, model, provider_id, bucket_index, rate, price_kind, band,
			raw_group, raw_ts, raw_session, raw_ord, raw_tie`
	if source.settings != "" {
		query += " SETTINGS " + source.settings
	}
	args := slices.Clone(candidates.args)
	args = append(args, source.args...)
	args = append(args, startUS, endUS)
	return query, args
}
