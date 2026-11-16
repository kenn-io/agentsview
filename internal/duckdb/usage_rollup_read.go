package duckdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

// A rollup read splits the usage rows into two disjoint sets, each a union
// of whole dedup groups:
//
//   - rollup rows of fresh sessions (unchanged since the build), minus the
//     (session, bucket) units whose finalized groups now share an identity
//     with a changed session's row or a Cursor account row;
//   - live rows, ranked with today's SQL: every row of changed and new
//     sessions, the stored exception rows of fresh sessions, the rows of the
//     units above, and Cursor account rows.
//
// A finalized group lies in one fresh session and bucket and shared its
// identity with nothing else at build time, so only a changed session or a
// new Cursor row can join it later, and that sends its unit live.

// usageRollupPlan is a read's view of the rollup.
type usageRollupPlan struct {
	// changed reports whether any live session is new or changed.
	changed bool
	// units lists the (session, bucket) units read live instead of from the
	// rollup, as duckUsageUnitKeySQL names them.
	units []string
}

// usageRollupPlan returns nil when the read must use the live path: no
// rollup, another format, other pricing (pricingDigest "" skips the pricing
// check), or a zone whose local days do not fall on bucket boundaries.
func (s *Store) usageRollupPlan(
	ctx context.Context, f db.UsageFilter, pricingDigest string, cursor bool,
) (*usageRollupPlan, error) {
	meta, ok, err := s.readUsageRollupMeta(ctx)
	if err != nil || !ok || meta.Format != usageRollupFormat {
		return nil, err
	}
	if pricingDigest != "" && (meta.PricingDigest != pricingDigest ||
		meta.AliasesDigest != pricingpkg.ConfiguredAliasesDigest()) {
		return nil, nil
	}
	if !usageRollupZoneAligned(f, meta) {
		return nil, nil
	}
	plan := &usageRollupPlan{}
	if err := s.queryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sessions s
		WHERE s.deleted_at IS NULL AND s.id NOT IN (`+usageRollupFreshSessionsSQL+`))`).
		Scan(&plan.changed); err != nil {
		return nil, fmt.Errorf("checking duckdb sessions changed since the usage rollup: %w", err)
	}
	var sources []string
	var args []any
	if plan.changed {
		raw, rawArgs := duckUsageScopedRawSQL(duckUsageSnapshotInputFilter(f), duckUsageChangedScope())
		sources = append(sources, raw)
		args = append(args, rawArgs...)
	}
	if cursor {
		if raw, rawArgs, ok := duckCursorUsageRowsSQLForBounds(f, duckUsageBoundsForFilter(f)); ok {
			sources = append(sources, raw)
			args = append(args, rawArgs...)
		}
	}
	if len(sources) == 0 {
		return plan, nil
	}
	query := `SELECT DISTINCT ` + duckUsageUnitKeySQL("i.session_id", "i.bucket") + `
		FROM usage_rollup_identities i
		WHERE i.session_id IN (` + usageRollupFreshSessionsSQL + `)
			AND i.identity IN (
				SELECT hash(` + duckUsageDedupGroupSQL + `)
				FROM (` + joinUnionAll(sources) + `)
			)`
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("matching duckdb usage rollup identities: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var unit string
		if err := rows.Scan(&unit); err != nil {
			return nil, fmt.Errorf("scanning duckdb usage rollup unit: %w", err)
		}
		plan.units = append(plan.units, unit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb usage rollup units: %w", err)
	}
	return plan, nil
}

func joinUnionAll(parts []string) string {
	return strings.Join(parts, "\n\t\tUNION ALL\n")
}

// duckUsageChangedScope selects the rows of sessions the rollup does not
// describe. It reads s.id so the filter applies to the small sessions side.
func duckUsageChangedScope() duckUsageRowScope {
	pred := "s.id NOT IN (" + usageRollupFreshSessionsSQL + ")"
	return duckUsageRowScope{message: pred, event: pred}
}

// liveScope selects the live rows of changed sessions and of live units.
func (p *usageRollupPlan) liveScope() duckUsageRowScope {
	scope := duckUsageChangedScope()
	if len(p.units) == 0 {
		return scope
	}
	unitPred := func(ts string) string {
		return " OR " + duckUsageUnitKeySQL("s.id", duckUsageBucketSQL("COALESCE("+ts+", s.started_at)")) +
			" IN (SELECT unnest(?))"
	}
	scope.message += unitPred("m.timestamp")
	scope.event += unitPred("ue.occurred_at")
	scope.messageArgs = []any{p.units}
	scope.eventArgs = []any{p.units}
	return scope
}

// unitFilter excludes live units from a rollup table aliased alias.
func (p *usageRollupPlan) unitFilter(alias string) (string, []any) {
	if len(p.units) == 0 {
		return "", nil
	}
	return "\n\t\t\tAND " + duckUsageUnitKeySQL(alias+".session_id", alias+".bucket") +
		" NOT IN (SELECT unnest(?))", []any{p.units}
}

// liveCTE renders today's dedup CTE over the live rows, with Cursor account
// rows when cursor is set.
func (p *usageRollupPlan) liveCTE(f db.UsageFilter, cursor bool) (string, []any) {
	var sources []string
	var args []any
	if p.changed || len(p.units) > 0 {
		raw, rawArgs := duckUsageScopedRawSQL(duckUsageSnapshotInputFilter(f), p.liveScope())
		sources = append(sources, raw)
		args = append(args, rawArgs...)
	}
	exceptionWhere := "e.session_id IN (" + usageRollupFreshSessionsSQL + ")"
	unitWhere, unitArgs := p.unitFilter("e")
	exceptionWhere += unitWhere
	exceptionArgs := unitArgs
	exceptionWhere, exceptionArgs = appendDuckUsageColumnBounds(
		exceptionWhere, "COALESCE(e.pricing_ts, s.started_at)", duckUsageBoundsForFilter(f), exceptionArgs)
	sources = append(sources, `
		SELECT e.session_id AS session_id, e.message_ordinal AS message_ordinal,
			e.source AS source, COALESCE(e.pricing_ts, s.started_at) AS ts,
			e.pricing_ts AS pricing_ts, e.model AS model, e.provider_id AS provider_id,
			e.claude_message_id AS claude_message_id,
			e.claude_request_id AS claude_request_id, e.source_uuid AS source_uuid,
			e.usage_dedup_key AS usage_dedup_key,
			e.input_tokens AS input_tokens, e.output_tokens AS output_tokens,
			e.cache_create AS cache_create, e.cache_read AS cache_read,
			e.reasoning_tokens AS reasoning_tokens,
			e.cache_create_1h AS cache_create_1h,
			e.web_search_requests AS web_search_requests,
			e.cost_microdollars AS cost_microdollars, e.cost_source AS cost_source,
			e.recorded_cost_microdollars AS recorded_cost_microdollars,
			s.project AS project, s.agent AS agent, s.machine AS machine,
			s.user_message_count AS user_message_count, s.is_automated AS is_automated,
			COALESCE(s.display_name, s.session_name, s.first_message, s.project) AS display_name,
			s.group_key AS group_key, COALESCE(s.session_name, '') AS session_name,
			s.started_at AS started_at,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS activity_at
		FROM usage_rollup_exceptions e
		JOIN sessions s ON s.id = e.session_id
		WHERE `+exceptionWhere)
	args = append(args, exceptionArgs...)
	if cursor {
		if raw, rawArgs, ok := duckCursorUsageRowsSQLForBounds(f, duckUsageBoundsForFilter(f)); ok {
			sources = append(sources, raw)
			args = append(args, rawArgs...)
		}
	}
	return duckUsageCTEFromRaw(f, joinUnionAll(sources), args, true)
}

// rollupWhere selects the rollup rows of table alias that a read with f
// keeps: fresh sessions outside live units, inside the local-date window,
// and passing the session and model filters. For a Claude group the model
// is its winner's, which is the model today's filter reads after ranking.
func (p *usageRollupPlan) rollupWhere(f db.UsageFilter, alias string) (string, []any) {
	where := alias + ".session_id IN (" + usageRollupFreshSessionsSQL + ")"
	var args []any
	unitWhere, unitArgs := p.unitFilter(alias)
	where += unitWhere
	args = append(args, unitArgs...)
	where, args = appendDuckUsageColumnBounds(where, alias+".bucket", duckUsageBoundsForFilter(f), args)
	if f.From != "" || f.To != "" {
		localDate, localArg := duckUsageLocalDateSQLFor(f, alias+".bucket")
		if f.From != "" {
			where += "\n\t\t\tAND " + localDate + " >= ?"
			args = append(args, localArg, f.From)
		}
		if f.To != "" {
			where += "\n\t\t\tAND " + localDate + " <= ?"
			args = append(args, localArg, f.To)
		}
	}
	filter, filterArgs := duckUsageSessionFilterSQL(f, alias+".session_id", alias+".model")
	if filter != "TRUE" {
		where += "\n\t\t\tAND " + filter
		args = append(args, filterArgs...)
	}
	return where, args
}

// usageRollupZoneAligned reports whether every rollup bucket a read with f
// can select falls on one local date, and lies inside the padded UTC bounds
// whenever its date is inside the window. Both hold when the zone's offset
// is a multiple of 15 minutes between -13:45 and +14:00 and changes only on
// 15-minute UTC boundaries across those buckets.
func usageRollupZoneAligned(f db.UsageFilter, meta usageRollupMeta) bool {
	if meta.MinBucket == nil {
		return true
	}
	lo, hi := *meta.MinBucket, meta.MaxBucket.Add(usageRollupBucketSeconds*time.Second)
	bounds := duckUsageBoundsForFilter(f)
	if bounds.from != "" {
		t, err := time.Parse(time.RFC3339, bounds.from)
		if err != nil {
			return false
		}
		lo = maxTime(lo, t)
	}
	if bounds.to != "" {
		t, err := time.Parse(time.RFC3339, bounds.to)
		if err != nil {
			return false
		}
		hi = minTime(hi, t.Add(usageRollupBucketSeconds*time.Second))
	}
	if !lo.Before(hi) {
		return true
	}
	_, zone := duckUsageLocalDateSQL(f)
	switch zone := zone.(type) {
	case int:
		return usageRollupOffsetAligned(zone)
	case string:
		loc, err := time.LoadLocation(zone)
		if err != nil {
			return false
		}
		for t := lo; ; {
			local := t.In(loc)
			if _, offset := local.Zone(); !usageRollupOffsetAligned(offset) {
				return false
			}
			_, end := local.ZoneBounds()
			if !end.IsZero() && !end.After(t) {
				// Past a zone's last listed transition, Go derives offsets
				// from the zone's rule string and ends a year's last period
				// 365 days after the year starts. In a leap year that is
				// December 31 at 00:00 UTC, at or before t, and the walk
				// would never advance. Go keeps one offset until the UTC
				// year ends, so resume there.
				end = time.Date(t.UTC().Year()+1, time.January, 1, 0, 0, 0, 0, time.UTC)
			}
			if end.IsZero() || !end.Before(hi) {
				return true
			}
			if end.Unix()%usageRollupBucketSeconds != 0 || end.Nanosecond() != 0 {
				return false
			}
			t = end
		}
	}
	return false
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func usageRollupOffsetAligned(offset int) bool {
	return offset%usageRollupBucketSeconds == 0 &&
		offset >= -(13*3600+45*60) && offset <= 14*3600
}

// dailyPartsFromRollup builds the per-session daily parts from the live rows
// and the rollup, and replays the rollup's pricing records into resolver.
func (s *Store) dailyPartsFromRollup(
	ctx context.Context, f db.UsageFilter, plan *usageRollupPlan,
	resolver *export.PricingResolver,
) (map[string]duckDailyPart, error) {
	cte, args := plan.liveCTE(f, true)
	parts, err := dailyPartsFromRows(f, resolver, func(visit func(duckUsageAggregateRow) error) error {
		return s.forEachDailyUsageAggregateRowFrom(ctx, f, cte, args, visit)
	})
	if err != nil {
		return nil, err
	}
	where, whereArgs := plan.rollupWhere(f, "r")
	localDate, localArg := duckUsageLocalDateSQLFor(f, "r.bucket")
	machine := "''"
	if f.Breakdowns {
		machine = "s.machine"
	}
	rows, err := s.queryContext(ctx, `
		SELECT r.session_id, r.local_date, s.project, s.agent, `+machine+`, r.model, r.provider_id,
			CAST(SUM(r.input_tokens) AS BIGINT), CAST(SUM(r.output_tokens) AS BIGINT),
			CAST(SUM(r.cache_creation_tokens) AS BIGINT), CAST(SUM(r.cache_read_tokens) AS BIGINT),
			CAST(SUM(r.cost_microdollars) AS BIGINT), CAST(SUM(r.savings_microdollars) AS BIGINT)
		FROM (
			SELECT r.*, `+localDate+` AS local_date
			FROM usage_rollup r
			WHERE `+where+`
		) r
		JOIN sessions s ON s.id = r.session_id
		GROUP BY ALL`, append([]any{localArg}, whereArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb usage rollup: %w", err)
	}
	defer rows.Close()
	index := map[string]map[duckUsageAccumKey]int{}
	for rows.Next() {
		var id string
		var key duckUsageAccumKey
		var input, output, cacheCr, cacheRd, cost, savings int64
		if err := rows.Scan(&id, &key.date, &key.project, &key.agent, &key.machine,
			&key.model, &key.providerID, &input, &output, &cacheCr, &cacheRd, &cost, &savings); err != nil {
			return nil, fmt.Errorf("scanning duckdb usage rollup: %w", err)
		}
		part := parts[id]
		keys := index[id]
		if keys == nil {
			keys = make(map[duckUsageAccumKey]int, len(part.Rows))
			for i, row := range part.Rows {
				keys[duckUsageAccumKey{row.Date, row.Project, row.Agent, row.Machine, row.Model, row.ProviderID}] = i
			}
			index[id] = keys
		}
		i, ok := keys[key]
		if !ok {
			i = len(part.Rows)
			keys[key] = i
			part.Rows = append(part.Rows, duckDailyPartRow{
				Date: key.date, Project: key.project, Agent: key.agent, Machine: key.machine,
				Model: key.model, ProviderID: key.providerID,
			})
		}
		row := &part.Rows[i]
		row.Input += int(input)
		row.Output += int(output)
		row.CacheCreate += int(cacheCr)
		row.CacheRead += int(cacheRd)
		if row.Estimated, err = money.Add(row.Estimated, money.Money{Microdollars: cost}); err != nil {
			return nil, fmt.Errorf("summing duckdb usage rollup cost: %w", err)
		}
		if part.Savings, err = money.Add(part.Savings, money.Money{Microdollars: savings}); err != nil {
			return nil, fmt.Errorf("summing duckdb usage rollup savings: %w", err)
		}
		parts[id] = part
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb usage rollup: %w", err)
	}
	records, err := s.usageRollupPricingRecords(ctx, f, plan)
	if err != nil {
		return nil, err
	}
	for id, recs := range records {
		part := parts[id]
		part.Records = append(part.Records, recs...)
		parts[id] = part
	}
	return parts, nil
}

// usageRollupRecordsReadObserver, when set, runs after a rollup read loads
// the pricing records and before it reads their counts.
var usageRollupRecordsReadObserver func()

// usageRollupPricingRecords returns, per session, the pricing provenance of
// the rollup rows a read keeps, as the live path records it row by row.
func (s *Store) usageRollupPricingRecords(
	ctx context.Context, f db.UsageFilter, plan *usageRollupPlan,
) (map[string][]export.PricingRecord, error) {
	dictionary := map[int]usageRollupRecord{}
	rows, err := s.queryContext(ctx, `SELECT record_id, record FROM usage_rollup_records`)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb usage rollup records: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return nil, fmt.Errorf("scanning duckdb usage rollup record: %w", err)
		}
		var rec usageRollupRecord
		if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&rec); err != nil {
			return nil, fmt.Errorf("decoding duckdb usage rollup record %d: %w", id, err)
		}
		dictionary[id] = rec
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb usage rollup records: %w", err)
	}
	if usageRollupRecordsReadObserver != nil {
		usageRollupRecordsReadObserver()
	}
	where, args := plan.rollupWhere(f, "p")
	counts, err := s.queryContext(ctx, `
		SELECT p.session_id, p.record_id, p.band, CAST(SUM(p.requests) AS BIGINT),
			CAST(SUM(p.aggregates) AS BIGINT), BOOL_OR(p.computed), BOOL_OR(p.reported)
		FROM usage_rollup_pricing p
		WHERE `+where+`
		GROUP BY p.session_id, p.record_id, p.band
		ORDER BY p.session_id, p.record_id, p.band NULLS FIRST`, args...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb usage rollup pricing: %w", err)
	}
	defer counts.Close()
	records := map[string][]export.PricingRecord{}
	for counts.Next() {
		var sessionID string
		var id int
		var band sql.NullInt64
		var requests, aggregates int64
		var computed, reported bool
		if err := counts.Scan(&sessionID, &id, &band, &requests, &aggregates, &computed, &reported); err != nil {
			return nil, fmt.Errorf("scanning duckdb usage rollup pricing: %w", err)
		}
		rec, ok := dictionary[id]
		if !ok {
			return nil, fmt.Errorf("duckdb usage rollup pricing names missing record %d", id)
		}
		out := export.PricingRecord{
			ReportedModel: rec.ReportedModel, PricedModel: rec.PricedModel, Lookup: rec.Lookup,
			Computed: computed, Reported: reported,
		}
		if band.Valid {
			out.BandRequests = map[int]int{int(band.Int64): int(requests)}
		} else {
			out.BaseRequests, out.AggregateRows = int(requests), int(aggregates)
		}
		records[sessionID] = append(records[sessionID], out)
	}
	if err := counts.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb usage rollup pricing: %w", err)
	}
	return records, nil
}

// topSessionsFromRollup sums Top sessions over the live rows and the rollup.
func (s *Store) topSessionsFromRollup(
	ctx context.Context, f db.UsageFilter, plan *usageRollupPlan,
	resolver *export.PricingResolver,
) (map[string]db.TopSessionEntry, error) {
	bySession := map[string]*duckTopSessionAcc{}
	cte, args := plan.liveCTE(f, false)
	if err := accumulateTopSessionRows(f, resolver, bySession, func(visit func(duckUsageAggregateRow) error) error {
		return s.forEachSessionUsageAggregateRowFrom(ctx, cte, args, visit)
	}); err != nil {
		return nil, err
	}
	where, whereArgs := plan.rollupWhere(f, "r")
	rows, err := s.queryContext(ctx, `
		SELECT r.session_id,
			COALESCE(s.display_name, s.session_name, s.first_message, s.project),
			s.agent, s.project, s.started_at, s.group_key, COALESCE(s.session_name, ''), s.machine,
			CAST(SUM(r.input_tokens) AS BIGINT), CAST(SUM(r.output_tokens) AS BIGINT),
			CAST(SUM(r.cache_creation_tokens) AS BIGINT), CAST(SUM(r.cache_read_tokens) AS BIGINT),
			CAST(SUM(r.cost_microdollars) AS BIGINT)
		FROM usage_rollup r
		JOIN sessions s ON s.id = r.session_id
		WHERE `+where+`
		GROUP BY ALL`, whereArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb usage rollup sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry db.TopSessionEntry
		var startedAt any
		var input, output, cacheCr, cacheRd, cost int64
		if err := rows.Scan(&entry.SessionID, &entry.DisplayName, &entry.Agent, &entry.Project,
			&startedAt, &entry.GroupKey, &entry.SessionName, &entry.Machine,
			&input, &output, &cacheCr, &cacheRd, &cost); err != nil {
			return nil, fmt.Errorf("scanning duckdb usage rollup session: %w", err)
		}
		entry.StartedAt = formatDBTime(startedAt)
		a := bySession[entry.SessionID]
		if a == nil {
			a = &duckTopSessionAcc{row: entry}
			bySession[entry.SessionID] = a
		}
		a.row.InputTokens += int(input)
		a.row.OutputTokens += int(output)
		a.row.CacheCreationTokens += int(cacheCr)
		a.row.CacheReadTokens += int(cacheRd)
		a.tokens += int(input + output + cacheCr + cacheRd)
		if a.cost, err = money.Add(a.cost, money.Money{Microdollars: cost}); err != nil {
			return nil, fmt.Errorf("summing duckdb top-session cost: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb usage rollup sessions: %w", err)
	}
	return finishTopSessions(bySession), nil
}

// rollupSessionInfo adds the sessions with a kept rollup row to seen.
func (s *Store) rollupSessionInfo(
	ctx context.Context, f db.UsageFilter, plan *usageRollupPlan,
	seen map[string]db.UsageSessionInfo,
) error {
	where, args := plan.rollupWhere(f, "r")
	rows, err := s.queryContext(ctx, `
		SELECT DISTINCT r.session_id, s.project, s.agent
		FROM usage_rollup r
		JOIN sessions s ON s.id = r.session_id
		WHERE `+where, args...)
	if err != nil {
		return fmt.Errorf("querying duckdb usage rollup session counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var info db.UsageSessionInfo
		if err := rows.Scan(&id, &info.Project, &info.Agent); err != nil {
			return fmt.Errorf("scanning duckdb usage rollup session count: %w", err)
		}
		seen[id] = info
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating duckdb usage rollup session counts: %w", err)
	}
	return nil
}
