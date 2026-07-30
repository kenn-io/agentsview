package db

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// activityReportRangeBoundsUTC returns the exact [start, end) UTC bounds
// of the resolved range `q` as zone-less strings. It generalizes the old
// per-day window helper so the candidate-session predicate selects exactly
// the sessions whose window intersects the range, with no padding slop.
//
// The layout omits the zone suffix deliberately. SQLite compares timestamp
// TEXT lexicographically; a Z-suffixed bound sorts a sub-second value
// (".123Z") before a whole-second bound ("Z") because '.' < 'Z', dropping
// sessions in the first sub-second of the range. A zone-less bound is a
// strict prefix of every stored RFC3339Nano-UTC value at that second, so
// whole-second and fractional values both compare correctly.
// PostgreSQL/DuckDB compare parsed instants and keep the zone in their own
// copies of this helper; this divergence makes SQLite match their
// already-correct boundary behavior.
func activityReportRangeBoundsUTC(q activity.Query) (string, string) {
	const boundLayout = "2006-01-02T15:04:05"
	return q.RangeStart.UTC().Format(boundLayout),
		q.RangeEnd.UTC().Format(boundLayout)
}

// GetActivityReport assembles a concurrency- and usage-oriented report
// for the resolved range `q`. It runs three fetches scoped to the SAME
// candidate session-ID set so the concurrency timeline, sessions table,
// and usage totals stay mutually consistent (no orphan usage rows), then
// hands the in-memory streams to activity.Aggregate.
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
func (db *DB) GetActivityReport(
	ctx context.Context, f AnalyticsFilter, q activity.Query,
) (activity.Report, error) {
	f.IncludeSubagents = true
	f.IncludeForks = true
	rangeStartUTC, rangeEndUTC := activityReportRangeBoundsUTC(q)
	lowerBound := paddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
	upperBound := paddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)

	sessions, ids, err := db.activityReportSessions(
		ctx, f, rangeStartUTC, rangeEndUTC)
	if err != nil {
		return activity.Report{}, err
	}

	acts, err := db.activityReportActivity(ctx, ids)
	if err != nil {
		return activity.Report{}, err
	}

	usage, pricing, err := db.activityReportUsage(ctx, ids, lowerBound, upperBound, q)
	if err != nil {
		return activity.Report{}, err
	}

	report, err := activity.Aggregate(activity.Params{
		RangeStart:    q.RangeStart,
		RangeEnd:      q.RangeEnd,
		Loc:           q.Loc,
		EffectiveEnd:  q.EffectiveEnd,
		Partial:       q.Partial,
		GapCapSeconds: q.GapCapSeconds,
		Bucket:        q.Bucket,
	}, sessions, acts, usage)
	if err != nil {
		return activity.Report{}, fmt.Errorf("aggregating activity report: %w", err)
	}
	report.SchemaVersion = export.ActivityReportSchemaVersion
	report.Pricing = pricing
	projects, err := db.BuildProjectIdentityMap(ctx,
		activityReportProjectLabels(sessions))
	if err != nil {
		return activity.Report{}, err
	}
	activity.SanitizeProjectLabels(&report, projects)
	report.Projects = export.ProjectMapForWire(projects)
	return report, nil
}

// GetSessionUsageRows returns the backend-priced usage rows for the supplied
// sessions, with the same cross-session deduplication as activity reports.
type sqliteSessionUsageOrderedRow struct {
	scan    usageScanRow
	ts      time.Time
	validTS bool
	ordinal int64
}

func (db *DB) GetSessionUsageRows(
	ctx context.Context, ids []string,
) ([]activity.UsageRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	pricing, err := db.loadPricingMap(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)
	sessionOrder := make(map[string]int, len(ids))
	for i, id := range ids {
		sessionOrder[id] = i
	}
	var rowsAcc []sqliteSessionUsageOrderedRow
	err = queryChunked(ids, func(chunk []string) error {
		ph, args := inPlaceholders(chunk)
		query := usageRowSelect() + ` AND u.session_id IN ` + ph
		rows, queryErr := db.getReader().QueryContext(ctx, query, args...)
		if queryErr != nil {
			return fmt.Errorf("querying session usage rows: %w", queryErr)
		}
		defer rows.Close()
		for rows.Next() {
			r, scanErr := scanUsageRow(rows)
			if scanErr != nil {
				return fmt.Errorf("scanning session usage rows: %w", scanErr)
			}
			ordinal := int64(-1)
			if r.messageOrdinal.Valid {
				ordinal = r.messageOrdinal.Int64
			}
			parsedTS, tsErr := parseTimestamp(r.ts)
			rowsAcc = append(rowsAcc, sqliteSessionUsageOrderedRow{
				scan:    r,
				ts:      parsedTS,
				validTS: tsErr == nil,
				ordinal: ordinal,
			})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rowsAcc, func(i, j int) bool {
		return sqliteSessionUsageRowLess(rowsAcc[i], rowsAcc[j], sessionOrder)
	})
	seen := make(map[usageDedupToken]struct{})
	out := make([]activity.UsageRow, 0, len(rowsAcc))
	for _, o := range rowsAcc {
		r := o.scan
		if key, ok := usageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}
		_, outputTok, _, _, _ := sqliteSessionUsageRowTokens(r)
		costRow := r
		var sessionCost *money.Money
		if r.costSource == CopilotReportedCostSource && r.cost.Valid {
			v := money.Money{Microdollars: r.cost.Int64}
			sessionCost = &v
			costRow.cost = sql.NullInt64{}
			rateResolver.RecordUnattributedReported()
		}
		cost, priced, contributes, priceErr := sessionRowCost(costRow, rateResolver)
		if priceErr != nil {
			return nil, priceErr
		}
		costSource := export.CostSourceComputed
		if costRow.cost.Valid {
			costSource = export.CostSourceReported
		}
		out = append(out, activity.UsageRow{
			SessionID:       r.sessionID,
			Model:           r.model,
			Timestamp:       r.ts,
			OutputTokens:    outputTok,
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

func sqliteSessionUsageRowTokens(
	r usageScanRow,
) (inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok int) {
	if r.usageSource == "message" {
		return clampedUsageTokenCountersWithReasoning(r.tokenJSON)
	}
	inputTok, outputTok, cacheCrTok, cacheRdTok = usageEventRowTokens(
		r.usageSource,
		r.inputTokens, r.outputTokens,
		r.cacheCreationInputTokens, r.cacheReadInputTokens,
	)
	return inputTok, outputTok, cacheCrTok, cacheRdTok, r.reasoningTokens
}

func sqliteSessionUsageRowLess(
	a, b sqliteSessionUsageOrderedRow,
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
	if a.scan.usageSource != b.scan.usageSource {
		return a.scan.usageSource < b.scan.usageSource
	}
	return a.scan.usageDedupKey < b.scan.usageDedupKey
}

func activityReportProjectLabels(
	sessions []activity.SessionMeta,
) []string {
	set := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		set[session.Project] = struct{}{}
	}
	return sortedSetKeys(set)
}

// activityReportSessions returns the candidate sessions whose window
// overlaps the exact range [rangeStartUTC, rangeEndUTC), plus their
// IDs. The ID set defines the scope for the activity and usage fetches.
// NULLIF guards the empty-string timestamp fallbacks SQLite stores so a
// session with an empty ended_at but a valid started_at still falls back
// correctly, matching the activity-expression convention elsewhere.
//
// The effective-end fallback for a session with no ended_at uses its
// latest message timestamp before started_at, so a still-open or
// partially-parsed session that began before the range but has messages
// inside it is not dropped. COALESCE short-circuits, so the correlated
// MAX subquery runs only for the rare sessions missing an ended_at.
func (db *DB) activityReportSessions(
	ctx context.Context, f AnalyticsFilter, rangeStartUTC, rangeEndUTC string,
) ([]activity.SessionMeta, []string, error) {
	return db.activityReportSessionsFrom(
		ctx, db.getReader(), f, rangeStartUTC, rangeEndUTC,
	)
}

func (db *DB) activityReportSessionsFrom(
	ctx context.Context,
	q sessionExportQuerier,
	f AnalyticsFilter,
	rangeStartUTC, rangeEndUTC string,
) ([]activity.SessionMeta, []string, error) {
	where, args := f.buildWhereWithDate("", false, "s.id")
	args = append(args, rangeStartUTC, rangeEndUTC)

	// Each Title candidate is NULLIF'd independently (not a nested
	// COALESCE-then-NULLIF) so an empty display_name cannot mask a real
	// session_name.
	query := `SELECT
		s.id,
		COALESCE(NULLIF(s.display_name, ''), NULLIF(s.session_name, ''),
			NULLIF(s.project, ''), s.id),
		s.project,
		s.agent,
		s.machine,
		COALESCE(s.started_at, ''),
		COALESCE(s.ended_at, ''),
		COALESCE(s.is_automated, 0)
	FROM sessions s
	WHERE ` + where + `
		AND COALESCE(NULLIF(s.ended_at, ''),
			(SELECT MAX(m.timestamp) FROM messages m
				WHERE m.session_id = s.id AND m.timestamp != ''),
			NULLIF(s.started_at, ''), s.created_at) >= ?
		AND COALESCE(NULLIF(s.started_at, ''), s.created_at) < ?`

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"querying activity report sessions: %w", err)
	}
	defer rows.Close()

	var sessions []activity.SessionMeta
	var ids []string
	for rows.Next() {
		var s activity.SessionMeta
		if err := rows.Scan(
			&s.SessionID, &s.Title, &s.Project, &s.Agent,
			&s.Machine, &s.StartedAt, &s.EndedAt, &s.IsAutomated,
		); err != nil {
			return nil, nil, fmt.Errorf(
				"scanning activity report session: %w", err)
		}
		sessions = append(sessions, s)
		ids = append(ids, s.SessionID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf(
			"iterating activity report sessions: %w", err)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].SessionID < sessions[j].SessionID
	})
	ids = ids[:0]
	for _, session := range sessions {
		ids = append(ids, session.SessionID)
	}
	return sessions, ids, nil
}

// activityReportActivity returns every timestamped message for the
// candidate sessions, ordered for the aggregator's per-session
// interval walk.
func (db *DB) activityReportActivity(
	ctx context.Context, ids []string,
) ([]activity.ActivityEvent, error) {
	return db.activityReportActivityFrom(ctx, db.getReader(), ids)
}

func (db *DB) activityReportActivityFrom(
	ctx context.Context, q sessionExportQuerier, ids []string,
) ([]activity.ActivityEvent, error) {
	var out []activity.ActivityEvent
	if len(ids) == 0 {
		return out, nil
	}
	err := queryChunked(ids, func(chunk []string) error {
		ph, args := inPlaceholders(chunk)
		query := `SELECT session_id, ordinal, role,
			COALESCE(timestamp, ''), model
		FROM messages
		WHERE session_id IN ` + ph + `
			AND timestamp IS NOT NULL
			AND timestamp != ''
		ORDER BY session_id, ordinal`

		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf(
				"querying activity report activity: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var e activity.ActivityEvent
			if err := rows.Scan(
				&e.SessionID, &e.Ordinal, &e.Role,
				&e.Timestamp, &e.Model,
			); err != nil {
				return fmt.Errorf(
					"scanning activity report activity: %w", err)
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		if a.Ordinal != b.Ordinal {
			return a.Ordinal < b.Ordinal
		}
		if a.Timestamp != b.Timestamp {
			return a.Timestamp < b.Timestamp
		}
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		return a.Model < b.Model
	})
	return out, nil
}

// activityReportUsage returns the usage rows for the candidate sessions
// within the padded range bounds, with per-row cost computed up front
// (mirroring GetDailyUsage) so cost logic stays in the backend. Rows
// are ordered by (ts, session_id, message_ordinal) as the aggregator
// requires for its first-seen-wins dedup. The order is computed on the
// parsed instant, not the RFC3339 text, so a whole-second value ("...00Z")
// and a fractional one ("...00.123Z") in the same second sort
// chronologically (matching PostgreSQL/DuckDB); lexically '.' < 'Z' would
// otherwise invert them and let SQLite keep a different duplicate row.
func (db *DB) activityReportUsage(
	ctx context.Context, ids []string, lowerBound, upperBound string, q activity.Query,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	return db.activityReportUsageFrom(
		ctx, db.getReader(), ids, lowerBound, upperBound, q,
	)
}

func (db *DB) activityReportUsageFrom(
	ctx context.Context,
	source sessionExportQuerier,
	ids []string,
	lowerBound, upperBound string,
	q activity.Query,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	candidates, rateResolver, err := db.loadActivityReportUsageCandidatesFrom(
		ctx, source, ids, lowerBound, upperBound,
	)
	if err != nil {
		return nil, nil, err
	}
	sortActivityReportUsageCandidates(candidates)
	baseRows := make([]activity.UsageRow, len(candidates))
	for i, candidate := range candidates {
		baseRows[i] = candidate.row
	}
	mask := activity.UsageSurvivorMask(
		q.RangeStart, q.RangeEnd, q.EffectiveEnd, baseRows,
	)
	return materializeActivityReportUsageCandidates(
		candidates, mask, rateResolver,
	)
}

// activityReportUsageCandidate retains the scanned source fields until the
// survivor set is known. Pricing provenance is recorded only for survivors in
// the ordinary activity report, while reporting export can materialize every
// raw candidate and perform its one combined survivor pass later.
type activityReportUsageCandidate struct {
	row     activity.UsageRow
	scan    dailyUsageScanRow
	ts      time.Time
	ordinal int64
}

func (db *DB) loadActivityReportUsageCandidatesFrom(
	ctx context.Context,
	source sessionExportQuerier,
	ids []string,
	lowerBound, upperBound string,
) ([]activityReportUsageCandidate, *export.PricingResolver, error) {
	pricing, err := db.loadPricingMapFrom(ctx, source)
	if err != nil {
		return nil, nil, fmt.Errorf("loading pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)
	if len(ids) == 0 {
		return []activityReportUsageCandidate{}, rateResolver, nil
	}

	// This query binds each id chunk twice (message-where and usage-event-where)
	// plus two time bounds, so the generic maxSQLVars chunk (bound once) would
	// emit 2*maxSQLVars+2 > 999 variables and overflow SQLite at ~500 candidate
	// sessions. Cap the chunk so 2*chunk+2 stays within maxSQLVars.
	const usageVarChunk = (maxSQLVars - 2) / 2
	var candidates []activityReportUsageCandidate
	err = queryChunkedSize(ids, usageVarChunk, func(chunk []string) error {
		ph, chunkArgs := inPlaceholders(chunk)
		// Apply the same eligibility filters as GetDailyUsage so empty
		// token_usage, empty, and synthetic models are excluded from the
		// daily totals and dedup, keeping parity with the Usage dashboard.
		rowsSQL := dailyUsageRowsSQLWithWhere(
			usageMessageEligibility+" AND m.session_id IN "+ph,
			usageEventEligibility+" AND ue.session_id IN "+ph)
		query := dailyUsageRowSelectFromRowsWithMachine(rowsSQL, true) + `
			AND u.ts >= ? AND u.ts <= ?`

		args := make([]any, 0, len(chunkArgs)*2+2)
		args = append(args, chunkArgs...) // message-where chunk
		args = append(args, chunkArgs...) // usage-event-where chunk
		args = append(args, lowerBound, upperBound)

		rows, err := source.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("querying activity report usage: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			r, scanErr := scanDailyUsageRowWithMachine(rows, true)
			if scanErr != nil {
				return fmt.Errorf(
					"scanning activity report usage: %w", scanErr)
			}
			ord := int64(-1)
			if r.messageOrdinal.Valid {
				ord = r.messageOrdinal.Int64
			}
			parsedTS, _ := parseTimestamp(r.ts)
			candidates = append(candidates, activityReportUsageCandidate{
				ts:      parsedTS,
				ordinal: ord,
				scan:    r,
				row: activity.UsageRow{
					SessionID:       r.sessionID,
					Model:           r.model,
					Timestamp:       r.ts,
					Project:         r.project,
					Machine:         r.machine,
					MessageOrdinal:  ord,
					UsageSource:     r.usageSource,
					Agent:           r.agent,
					ClaudeMessageID: r.claudeMessageID,
					ClaudeRequestID: r.claudeRequestID,
					SourceUUID:      r.sourceUUID,
					UsageDedupKey:   r.usageDedupKey,
				},
			})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, nil, err
	}
	return candidates, rateResolver, nil
}

func sortActivityReportUsageCandidates(
	candidates []activityReportUsageCandidate,
) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if !a.ts.Equal(b.ts) {
			return a.ts.Before(b.ts)
		}
		if a.row.SessionID != b.row.SessionID {
			return a.row.SessionID < b.row.SessionID
		}
		if a.ordinal != b.ordinal {
			return a.ordinal < b.ordinal
		}
		return compareDailyUsageSemantic(a.scan, b.scan) < 0
	})
}

// activityReportUsageCandidatesFrom returns normalized padded-range rows
// without sorting or applying a survivor mask. Reporting export merges these
// rows with standalone candidates before imposing either operation.
func (db *DB) activityReportUsageCandidatesFrom(
	ctx context.Context,
	source sessionExportQuerier,
	ids []string,
	lowerBound, upperBound string,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	candidates, rateResolver, err := db.loadActivityReportUsageCandidatesFrom(
		ctx, source, ids, lowerBound, upperBound,
	)
	if err != nil {
		return nil, nil, err
	}
	return materializeActivityReportUsageCandidates(
		candidates, nil, rateResolver,
	)
}

func materializeActivityReportUsageCandidates(
	candidates []activityReportUsageCandidate,
	mask []bool,
	rateResolver *export.PricingResolver,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	out := make([]activity.UsageRow, 0, len(candidates))
	for i, candidate := range candidates {
		if mask != nil && !mask[i] {
			continue
		}
		inputTok, outputTok, cacheCrTok, cacheRdTok, _ :=
			dailyUsageRowTokens(candidate.scan)
		costRow := candidate.scan
		var sessionCost *money.Money
		if candidate.scan.costSource == CopilotReportedCostSource &&
			candidate.scan.cost.Valid {
			v := money.Money{Microdollars: candidate.scan.cost.Int64}
			sessionCost = &v
			costRow.cost = sql.NullInt64{}
			rateResolver.RecordUnattributedReported()
		}
		cost, priced, contributes, priceErr :=
			sqliteActivityReportRowStatus(costRow, rateResolver)
		if priceErr != nil {
			return nil, nil, priceErr
		}
		costSource := export.CostSourceComputed
		if costRow.cost.Valid {
			costSource = export.CostSourceReported
		}
		row := candidate.row
		row.InputTokens = inputTok
		row.OutputTokens = outputTok
		row.CacheCreationTokens = cacheCrTok
		row.CacheReadTokens = cacheRdTok
		row.Cost = cost
		row.CostSource = costSource
		row.SessionCost = sessionCost
		row.Priced = priced
		row.Contributes = contributes
		out = append(out, row)
	}
	block, err := rateResolver.BuildBlock()
	if err != nil {
		return nil, nil, fmt.Errorf("building pricing block: %w", err)
	}
	return out, &block, nil
}

func compareDailyUsageSemantic(a, b dailyUsageScanRow) int {
	for _, compared := range []int{
		cmp.Compare(a.usageSource, b.usageSource),
		cmp.Compare(a.model, b.model),
		cmp.Compare(a.tokenJSON, b.tokenJSON),
		cmp.Compare(a.inputTokens, b.inputTokens),
		cmp.Compare(a.outputTokens, b.outputTokens),
		cmp.Compare(
			a.cacheCreationInputTokens,
			b.cacheCreationInputTokens,
		),
		cmp.Compare(a.cacheReadInputTokens, b.cacheReadInputTokens),
		cmp.Compare(a.reasoningTokens, b.reasoningTokens),
		compareNullInt64(a.cost, b.cost),
		cmp.Compare(a.costSource, b.costSource),
		cmp.Compare(a.claudeMessageID, b.claudeMessageID),
		cmp.Compare(a.claudeRequestID, b.claudeRequestID),
		cmp.Compare(a.sourceUUID, b.sourceUUID),
		cmp.Compare(a.usageDedupKey, b.usageDedupKey),
		cmp.Compare(a.project, b.project),
		cmp.Compare(a.agent, b.agent),
		cmp.Compare(a.machine, b.machine),
		compareNullInt64(a.messageOrdinal, b.messageOrdinal),
	} {
		if compared != 0 {
			return compared
		}
	}
	return 0
}

func compareNullInt64(a, b sql.NullInt64) int {
	if a.Valid != b.Valid {
		if !a.Valid {
			return -1
		}
		return 1
	}
	return cmp.Compare(a.Int64, b.Int64)
}

func sqliteActivityReportRowStatus(
	r dailyUsageScanRow, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	pricedModel, lookup := pricing.Resolve(
		r.model, usageLookupModel(r.model, r.ts))
	var inTok, outTok, crTok, rdTok int
	reasoningTok := r.reasoningTokens
	if r.usageSource == "message" {
		inTok, outTok, crTok, rdTok, reasoningTok =
			clampedUsageTokenCountersWithReasoning(r.tokenJSON)
	} else {
		inTok, outTok, crTok, rdTok = usageEventRowTokens(
			r.usageSource,
			r.inputTokens, r.outputTokens,
			r.cacheCreationInputTokens, r.cacheReadInputTokens)
	}

	if r.cost.Valid {
		pricing.RecordResolvedReported(r.model, pricedModel, lookup)
		return money.Money{Microdollars: r.cost.Int64}, true, true, nil
	}
	if inTok == 0 && outTok == 0 && reasoningTok == 0 &&
		crTok == 0 && rdTok == 0 {
		return money.Money{}, true, false, nil
	}
	if !lookup.OK {
		pricing.RecordResolvedComputed(r.model, pricedModel, lookup)
		return money.Money{}, false, true, nil
	}
	requestScoped := usageRowIsRequestScoped(r.usageSource, r.messageOrdinal)
	cost, err = lookup.Rates.CostForTokensScoped(
		requestScoped,
		inTok, outTok, reasoningTok, crTok, rdTok)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing activity usage for model %q: %w", r.model, err)
	}
	recordComputedUsagePricing(
		pricing,
		r.model,
		pricedModel,
		lookup,
		requestScoped,
		inTok,
		crTok,
		rdTok,
	)
	return cost, true, true, nil
}
