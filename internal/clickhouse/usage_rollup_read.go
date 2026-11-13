package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// chUsageRollupRead is a usage read's use of one rollup partition.
type chUsageRollupRead struct {
	key string
	// delta and replaced report that the state has changed sessions, whose
	// rows the usage_delta_rows external table holds, and sessions whose
	// stored rows a read skips, which usage_replaced_sessions lists.
	delta, replaced bool
	// fromBucket and toBucket bound the buckets whose rows can fall in the
	// read's padded range; hasFrom and hasTo report that the range has the
	// bound.
	fromBucket, toBucket int64
	hasFrom, hasTo       bool
	// cursorSQL selects the cursor usage rows the read ranks, if any, with
	// cursorArgs.
	cursorSQL  string
	cursorArgs []any
}

// dirtySQL selects the summed units an event of the read's own rows also
// names: a delta row's or a cursor row's. Those units are ranked from
// prepared_usage instead. It is "" when the read has no such rows.
func (r *chUsageRollupRead) dirtySQL() (string, []any) {
	var sources []string
	var args []any
	if r.delta {
		sources = append(sources, "SELECT "+chUsageDedupGroupSQL+" AS rollup_event FROM usage_delta_rows")
	}
	if r.cursorSQL != "" {
		sources = append(sources, "SELECT "+chUsageDedupGroupSQL+" AS rollup_event FROM ("+r.cursorSQL+")")
		args = append(args, r.cursorArgs...)
	}
	if len(sources) == 0 {
		return "", nil
	}
	return `SELECT session_id, bucket_key FROM ` + usageRollupTable + `
			WHERE rollup_key = ? AND row_kind = 'identity'
				AND sort_key IN (SELECT sipHash64(rollup_event) FROM (` + strings.Join(sources, " UNION ALL ") + `))`,
		append([]any{r.key}, args...)
}

// skipSQL renders the condition that a rollup row of the session column and
// bucket column is not one the read replaces with live rows.
func (r *chUsageRollupRead) skipSQL(sessionColumn, bucketColumn string) (string, []any) {
	where := ""
	if r.replaced {
		where += " AND " + sessionColumn + " NOT IN (SELECT id FROM usage_replaced_sessions)"
	}
	dirty, args := r.dirtySQL()
	if dirty != "" {
		where += " AND (" + sessionColumn + ", " + bucketColumn + ") NOT IN (" + dirty + ")"
	}
	return where, args
}

// sourceSQL renders, in place of chPreparedUsageSourceSQL, the prepared rows
// the read ranks: the exception rows, every row of the summed units the
// read's own rows name, and the delta rows. where names prepared columns
// without a prefix. A state with raw hosts never reads a rollup.
func (r *chUsageRollupRead) sourceSQL(state preparedUsageState, where string, whereArgs []any) (string, []any) {
	// Exception units hold no summed event, so no row of the read names one.
	query := "SELECT " + chPreparedUsageRowColumns + " FROM " + usageRollupTable +
		" WHERE rollup_key = ? AND row_kind = 'exception' AND " + where
	if r.replaced {
		query += " AND session_id NOT IN (SELECT id FROM usage_replaced_sessions)"
	}
	args := append([]any{r.key}, whereArgs...)
	if dirty, dirtyArgs := r.dirtySQL(); dirty != "" {
		query += "\n\t\tUNION ALL\n\t\tSELECT " + chPreparedUsageRowColumns + " FROM prepared_usage WHERE " + where
		args = append(args, whereArgs...)
		if r.replaced {
			query += " AND session_id NOT IN (SELECT id FROM usage_replaced_sessions)"
		}
		query += " AND (session_id, " + chUsageRollupBucketSQL(chPreparedUsageKeyColumn) + ") IN (" + dirty + ")"
		args = append(args, dirtyArgs...)
	}
	if len(state.changed) > 0 {
		query += "\n\t\tUNION ALL\n\t\tSELECT " + chPreparedUsageRowColumns + " FROM usage_delta_rows WHERE " + where
		args = append(args, whereArgs...)
	}
	return query, args
}

// chUsageRollupSumsSQL selects the rollup's groups a read under f takes, as
// usage pieces: the summed groups of live sessions that pass f's session and
// model filters, dated in f's zone and range, except those the read ranks
// itself.
func chUsageRollupSumsSQL(r *chUsageRollupRead, f db.UsageFilter, evidence bool) (string, []any) {
	localDate, localDateArg := chUsageLocalDateOf(f, "r.ts_null", "toDateTime64(r.bucket_key, 0, 'UTC')")
	machine := "''"
	if f.Breakdowns {
		machine = "a.machine"
	}
	evidenceSelect := ""
	if evidence {
		// A summed event has one session and, unless it is a Claude request,
		// one model, so it gives no evidence.
		evidenceSelect = `,
				CAST([] AS Array(String)) AS piece_inexact_models,
				CAST([] AS Array(String)) AS piece_inexact_projects`
	}
	args := []any{localDateArg}
	sessionWhere, sessionArgs := appendChUsageSessionFilterClauses("s.deleted_at IS NULL", nil, f, "")
	args = append(args, sessionArgs...)
	where := "r.rollup_key = ? AND r.row_kind = 'aggregate' AND r.sort_key = 0"
	args = append(args, r.key)
	if r.hasFrom || r.hasTo {
		// The range's timestamp bounds leave rows without a timestamp out.
		where += " AND NOT r.ts_null"
	}
	if r.hasFrom {
		where += " AND r.bucket_key >= ?"
		args = append(args, r.fromBucket)
	}
	if r.hasTo {
		where += " AND r.bucket_key <= ?"
		args = append(args, r.toBucket)
	}
	where, args = appendChUsageSourceFilterClauses(where, args, "r.model", f)
	skip, skipArgs := r.skipSQL("r.session_id", "r.bucket_key")
	where += skip
	args = append(args, skipArgs...)
	datePred := "1"
	if f.From != "" {
		datePred += " AND local_date >= ?"
		args = append(args, f.From)
	}
	if f.To != "" {
		datePred += " AND local_date <= ?"
		args = append(args, f.To)
	}
	return `SELECT * FROM (
			SELECT r.session_id AS session_id, ` + localDate + ` AS local_date,
				a.project AS project, a.agent AS agent, ` + machine + ` AS group_machine,
				r.model AS model, r.provider_id AS provider_id,
				r.r_explicit_key AS explicit_key, r.r_group_kind AS group_kind,
				r.r_group_context AS group_context, r.r_group_band AS group_band,
				r.r_events AS piece_events, r.r_input_tokens AS piece_input,
				r.r_output_tokens AS piece_output, r.r_cache_create AS piece_cache_create,
				r.r_cache_read AS piece_cache_read, r.r_token_cost AS piece_token_cost,
				r.r_savings AS piece_savings, r.r_explicit_cost AS piece_explicit_cost,
				r.r_web_search AS piece_web_search, r.r_price_error AS piece_price_error,
				r.price_model AS piece_price_model, r.source AS piece_source,
				r.message_ordinal AS piece_message_ordinal, r.ts AS piece_ts,
				r.pricing_ts AS piece_pricing_ts, r.cache_create_1h_norm AS piece_cache_create_1h,
				r.r_billable_input AS piece_billable_input, r.r_billable_output AS piece_billable_output,
				r.r_billable_reasoning AS piece_billable_reasoning,
				r.r_billable_cache_create AS piece_billable_cache_create,
				r.r_billable_cache_create_1h AS piece_billable_cache_create_1h,
				r.r_billable_cache_read AS piece_billable_cache_read,
				r.r_reported_cost_rows AS piece_reported_cost_rows,
				r.r_authoritative_cost AS piece_authoritative_cost,
				r.r_authoritative_cost_rows AS piece_authoritative_cost_rows,
				r.recorded_cost_microdollars AS piece_recorded_cost,
				r.usage_dedup_key AS piece_usage_dedup_key` + evidenceSelect + `
			FROM ` + usageRollupTable + ` r
			INNER JOIN (
				SELECT s.id AS id, s.project AS project, s.agent AS agent, s.machine AS machine
				FROM sessions s WHERE ` + sessionWhere + `
			) a ON a.id = r.session_id
			WHERE ` + where + `
		) WHERE ` + datePred, args
}

// chUsageRollupGroupsSQL renders, after usage_classified, the daily usage
// group query of a rollup read: the groups of the ranked rows and the
// rollup's sums, as pieces, merged into the groups the query without the
// rollup returns.
func chUsageRollupGroupsSQL(r *chUsageRollupRead, f db.UsageFilter, top, evidence bool) (string, []any) {
	sumsSQL, args := chUsageRollupSumsSQL(r, f, evidence)
	liveEvidence, mergedEvidence := "", ""
	if evidence {
		liveEvidence = `,
				groupUniqArrayArray(inexact_models) AS piece_inexact_models,
				groupUniqArrayArray(inexact_projects) AS piece_inexact_projects`
		mergedEvidence = ", groupUniqArrayArray(piece_inexact_models), groupUniqArrayArray(piece_inexact_projects)"
	}
	return `,
		usage_pieces AS (
			SELECT session_id, local_date, project, agent, group_machine, model, provider_id,
				` + chUsageGroupKeySelect + `,
				toInt64(count()) AS piece_events,
				sum(input_tokens_norm) AS piece_input, sum(output_tokens_norm) AS piece_output,
				sum(cache_create_norm) AS piece_cache_create, sum(cache_read_norm) AS piece_cache_read,
				sum(toInt128(p_token_cost)) AS piece_token_cost, sum(toInt128(p_savings)) AS piece_savings,
				sum(toInt128(explicit_cost)) AS piece_explicit_cost,
				sum(billable_web_search_requests) AS piece_web_search,
				` + chUsageGroupPriceErrorSQL + ` AS piece_price_error,
				any(price_model) AS piece_price_model, any(source) AS piece_source,
				any(message_ordinal) AS piece_message_ordinal, any(ts) AS piece_ts,
				any(pricing_ts) AS piece_pricing_ts, any(cache_create_1h_norm) AS piece_cache_create_1h,
				any(billable_input_tokens) AS piece_billable_input,
				any(billable_output_tokens) AS piece_billable_output,
				any(billable_reasoning_tokens) AS piece_billable_reasoning,
				any(billable_cache_creation_tokens) AS piece_billable_cache_create,
				any(billable_cache_creation_1h_tokens) AS piece_billable_cache_create_1h,
				any(billable_cache_read_tokens) AS piece_billable_cache_read,
				toInt64(any(reported_cost_rows)) AS piece_reported_cost_rows,
				any(authoritative_cost) AS piece_authoritative_cost,
				toInt64(any(authoritative_cost_rows)) AS piece_authoritative_cost_rows,
				any(recorded_cost_microdollars) AS piece_recorded_cost,
				any(usage_dedup_key) AS piece_usage_dedup_key` + liveEvidence + `
			FROM usage_classified
			GROUP BY session_id, local_date, project, agent, group_machine, model, provider_id,
				explicit_key, group_kind, group_context, group_band
			UNION ALL
			` + sumsSQL + `
		)
		SELECT session_id, local_date, project, agent, group_machine, model, provider_id,
			explicit_key, group_kind, group_context, group_band,
			sum(piece_events), sum(piece_input), sum(piece_output),
			sum(piece_cache_create), sum(piece_cache_read),
			toInt64(sum(piece_token_cost)), toInt64(sum(piece_savings)),
			toInt64(sum(piece_explicit_cost)), sum(piece_web_search),
			max(piece_price_error),
			arrayExists(total -> total < toInt128('-9223372036854775808')
				OR total > toInt128('9223372036854775807'),
				[sum(piece_token_cost), sum(piece_savings), sum(piece_explicit_cost)]),
			any(piece_price_model) AS row_price_model, any(piece_source) AS row_source,
			any(piece_message_ordinal) AS row_message_ordinal,
			any(piece_ts) AS row_ts, any(piece_pricing_ts),
			any(piece_cache_create_1h),
			any(piece_billable_input), any(piece_billable_output),
			any(piece_billable_reasoning), any(piece_billable_cache_create),
			any(piece_billable_cache_create_1h), any(piece_billable_cache_read),
			any(piece_reported_cost_rows),
			any(piece_authoritative_cost), any(piece_authoritative_cost_rows),
			any(piece_recorded_cost)` + mergedEvidence + `
		FROM usage_pieces
		GROUP BY session_id, local_date, project, agent, group_machine, model, provider_id,
			explicit_key, group_kind, group_context, group_band
		` + chUsageGroupOrder(top, "any(piece_usage_dedup_key)"), args
}

// usageRollupCache keeps what reads learn about one immutable rollup
// partition: the (model, price_model) pairs of its summed groups and, per
// zone, whether every summed bucket falls on one local date there.
type usageRollupCache struct {
	sync.Mutex
	key   string
	pairs map[[2]string]bool
	zones map[string]bool
}

// usageRollupRead decides whether a read under f can use the rollup of the
// state's prepared rows, and returns its use when it can. priced reports
// that the read prices its groups, which the rollup did under the prepared
// rows' digest, with no custom rates.
func (s *Store) usageRollupRead(
	ctx context.Context, state preparedUsageState, f db.UsageFilter,
	pricingDigest string, customModels [][2]string, priced bool,
) (*chUsageRollupRead, error) {
	if !state.ready || len(state.raw) > 0 {
		return nil, nil
	}
	if priced && (state.pricingDigest == "" || state.pricingDigest != pricingDigest) {
		return nil, nil
	}
	key := usageRollupKey(state.stamp)
	if key == "" {
		return nil, nil
	}
	r := &chUsageRollupRead{key: key, delta: len(state.changed) > 0, replaced: len(state.replaced()) > 0}
	// A bound that is not a time leaves the read to the prepared rows,
	// which apply it as they always have.
	unix := func(bound string) (int64, bool) {
		t, err := time.Parse(time.RFC3339, bound)
		return t.Unix(), err == nil
	}
	bounds := chUsageBoundsForFilter(f)
	if bounds.from != "" {
		from, ok := unix(bounds.from)
		if !ok {
			return nil, nil
		}
		// A bucket holds rows up to 15 minutes after its start.
		r.hasFrom, r.fromBucket = true, from-chUsageRollupBucketSeconds+1
	}
	if bounds.to != "" {
		to, ok := unix(bounds.to)
		if !ok {
			return nil, nil
		}
		r.hasTo, r.toBucket = true, to
	}
	pairs, ok, err := s.usageRollupPairs(ctx, key)
	if err != nil || !ok {
		return nil, err
	}
	if priced {
		for _, pair := range customModels {
			if pairs[pair] {
				return nil, nil
			}
		}
	}
	aligned, err := s.usageRollupZoneAligned(ctx, key, f)
	if err != nil || !aligned {
		return nil, err
	}
	return r, nil
}

// usageRollupPairs returns the (model, price_model) pairs of the partition
// key's summed groups that Go does not see whole. ok is false when the
// partition does not exist.
func (s *Store) usageRollupPairs(ctx context.Context, key string) (map[[2]string]bool, bool, error) {
	c := &s.usageRollups
	c.Lock()
	if c.key == key && c.pairs != nil {
		pairs := c.pairs
		c.Unlock()
		return pairs, true, nil
	}
	c.Unlock()
	var rows uint64
	var found [][]string
	err := s.queryRowContext(ctx, `SELECT count(),
			arrayMap(pair -> [pair.1, pair.2],
				groupUniqArrayIf((model, price_model), row_kind = 'aggregate' AND r_explicit_key = ''))
		FROM `+usageRollupTable+` WHERE rollup_key = ?`, key).Scan(&rows, &found)
	if isMissingTableError(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading usage rollup models: %w", err)
	}
	if rows == 0 {
		return nil, false, nil
	}
	pairs := make(map[[2]string]bool, len(found))
	for _, pair := range found {
		pairs[[2]string{pair[0], pair[1]}] = true
	}
	c.Lock()
	if c.key != key {
		c.key, c.zones = key, map[string]bool{}
	}
	c.pairs = pairs
	c.Unlock()
	return pairs, true, nil
}

// usageRollupZoneAligned reports whether every summed bucket of the
// partition key falls on one local date in f's zone: the zone's offset is a
// multiple of 15 minutes and does not change within the bucket. A read
// without a zone dates rows at one fixed offset.
func (s *Store) usageRollupZoneAligned(ctx context.Context, key string, f db.UsageFilter) (bool, error) {
	if f.Timezone == "" {
		return chUsageFixedOffset(f)%chUsageRollupBucketSeconds == 0, nil
	}
	c := &s.usageRollups
	c.Lock()
	if c.key == key {
		if aligned, ok := c.zones[f.Timezone]; ok {
			c.Unlock()
			return aligned, nil
		}
	}
	c.Unlock()
	var misaligned uint64
	err := s.queryRowContext(ctx, `SELECT count() FROM (
			SELECT DISTINCT bucket_key FROM `+usageRollupTable+`
			WHERE rollup_key = ? AND row_kind = 'aggregate' AND sort_key = 0 AND NOT ts_null
		)
		WHERE timeZoneOffset(toDateTime64(bucket_key, 0, ?)) != timeZoneOffset(toDateTime64(bucket_key + ?, 0, ?))
			OR timeZoneOffset(toDateTime64(bucket_key, 0, ?)) % ? != 0`,
		key, f.Timezone, chUsageRollupBucketSeconds-1, f.Timezone, f.Timezone, chUsageRollupBucketSeconds,
	).Scan(&misaligned)
	if isMissingTableError(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking usage rollup dates in %s: %w", f.Timezone, err)
	}
	aligned := misaligned == 0
	c.Lock()
	if c.key == key {
		c.zones[f.Timezone] = aligned
	}
	c.Unlock()
	return aligned, nil
}

// visitRollupDailyUsageGroups reads the daily usage groups with the rollup
// when the read can use it and passes them to visit, reporting whether it
// did. It reads every group before visiting any: a push may drop the
// partition while the query starts, and the read then serves the groups
// without the rollup instead.
func (s *Store) visitRollupDailyUsageGroups(
	ctx, readCtx context.Context, state preparedUsageState, f db.UsageFilter,
	pricingDigest string, customModels [][2]string, top bool,
	visit func(chDailyUsageGroupRow) error,
) (bool, error) {
	rollup, err := s.usageRollupRead(ctx, state, f, pricingDigest, customModels, true)
	if err != nil || rollup == nil {
		return false, err
	}
	query, args, err := s.dailyUsageGroupSQL(ctx, state, f, pricingDigest, customModels, top, rollup)
	if err != nil {
		return false, err
	}
	var groups []chDailyUsageGroupRow
	err = s.scanDailyUsageGroups(readCtx, query, args, f.PairwiseSides && !top, func(r chDailyUsageGroupRow) error {
		groups = append(groups, r)
		return nil
	})
	if isMissingTableError(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	present, err := usageRollupPresent(ctx, s.conn, rollup.key)
	if err != nil || !present {
		return false, err
	}
	for _, r := range groups {
		if err := visit(r); err != nil {
			return true, err
		}
	}
	return true, nil
}

// rollupUsageSessionRows reads the sessions with usage under f with the
// rollup when the read can use it, reporting whether it did; see
// visitRollupDailyUsageGroups.
func (s *Store) rollupUsageSessionRows(
	ctx, readCtx context.Context, state preparedUsageState, f db.UsageFilter,
) ([]chUsageSessionRow, bool, error) {
	rollup, err := s.usageRollupRead(ctx, state, f, "", nil, false)
	if err != nil || rollup == nil {
		return nil, false, err
	}
	source := usageCTEWith(state, f, "", rollup)
	sumsSQL, sumsArgs := chUsageRollupSumsSQL(rollup, f, false)
	counted, err := s.scanUsageSessionRows(readCtx, source.cte+`
		SELECT DISTINCT session_id, project, agent
		FROM (
			SELECT session_id, project, agent FROM usage_localized WHERE session_id != ''
			UNION ALL
			SELECT session_id, project, agent FROM (`+sumsSQL+`)
		)
		ORDER BY session_id`, append(source.args, sumsArgs...))
	if isMissingTableError(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	present, err := usageRollupPresent(ctx, s.conn, rollup.key)
	if err != nil || !present {
		return nil, false, err
	}
	return counted, true, nil
}

// scanUsageSessionRows runs a usage session count query.
func (s *Store) scanUsageSessionRows(ctx context.Context, query string, args []any) ([]chUsageSessionRow, error) {
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse usage session counts: %w", err)
	}
	defer rows.Close()
	var counted []chUsageSessionRow
	for rows.Next() {
		var r chUsageSessionRow
		if err := rows.Scan(&r.sessionID, &r.project, &r.agent); err != nil {
			return nil, fmt.Errorf("scanning clickhouse usage session count: %w", err)
		}
		counted = append(counted, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse usage session counts: %w", err)
	}
	return counted, nil
}
