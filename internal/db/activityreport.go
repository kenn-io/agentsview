package db

import (
	"context"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
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

	report := activity.Aggregate(activity.Params{
		RangeStart:    q.RangeStart,
		RangeEnd:      q.RangeEnd,
		Loc:           q.Loc,
		EffectiveEnd:  q.EffectiveEnd,
		Partial:       q.Partial,
		GapCapSeconds: q.GapCapSeconds,
		Bucket:        q.Bucket,
	}, sessions, acts, usage)
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
		cost, priced, contributes := sessionRowCost(r, rateResolver)
		out = append(out, activity.UsageRow{
			SessionID:       r.sessionID,
			Model:           r.model,
			Timestamp:       r.ts,
			OutputTokens:    outputTok,
			Cost:            cost,
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

	rows, err := db.getReader().QueryContext(ctx, query, args...)
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
	return sessions, ids, nil
}

// activityReportActivity returns every timestamped message for the
// candidate sessions, ordered for the aggregator's per-session
// interval walk.
func (db *DB) activityReportActivity(
	ctx context.Context, ids []string,
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

		rows, err := db.getReader().QueryContext(ctx, query, args...)
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
	out := []activity.UsageRow{}

	pricing, err := db.loadPricingMap(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("loading pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)
	if len(ids) == 0 {
		block, err := rateResolver.BuildBlock()
		if err != nil {
			return nil, nil, fmt.Errorf("building pricing block: %w", err)
		}
		return out, &block, nil
	}

	// Accumulate the parsed ts and dedup ordinal alongside each mapped row so
	// we can impose one global (ts, session_id, ordinal) order across all
	// chunks. The same (claude_message_id, claude_request_id) can recur in
	// different sessions (resumed/forked) and thus different chunks, so
	// per-chunk ordering is not enough for the aggregator's first-seen dedup.
	type ordered struct {
		row     activity.UsageRow
		scan    dailyUsageScanRow
		ts      time.Time
		ordinal int64
	}
	var rowsAcc []ordered

	// This query binds each id chunk twice (message-where and usage-event-where)
	// plus two time bounds, so the generic maxSQLVars chunk (bound once) would
	// emit 2*maxSQLVars+2 > 999 variables and overflow SQLite at ~500 candidate
	// sessions. Cap the chunk so 2*chunk+2 stays within maxSQLVars.
	const usageVarChunk = (maxSQLVars - 2) / 2
	err = queryChunkedSize(ids, usageVarChunk, func(chunk []string) error {
		ph, chunkArgs := inPlaceholders(chunk)
		// Apply the same eligibility filters as GetDailyUsage so empty
		// token_usage, empty, and synthetic models are excluded from the
		// daily totals and dedup, keeping parity with the Usage dashboard.
		rowsSQL := dailyUsageRowsSQLWithWhere(
			usageMessageEligibility+" AND m.session_id IN "+ph,
			usageEventEligibility+" AND ue.session_id IN "+ph)
		query := dailyUsageRowSelectFromRows(rowsSQL) + `
			AND u.ts >= ? AND u.ts <= ?`

		args := make([]any, 0, len(chunkArgs)*2+2)
		args = append(args, chunkArgs...) // message-where chunk
		args = append(args, chunkArgs...) // usage-event-where chunk
		args = append(args, lowerBound, upperBound)

		rows, err := db.getReader().QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("querying activity report usage: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			r, scanErr := scanDailyUsageRow(rows)
			if scanErr != nil {
				return fmt.Errorf(
					"scanning activity report usage: %w", scanErr)
			}
			ord := int64(-1)
			if r.messageOrdinal.Valid {
				ord = r.messageOrdinal.Int64
			}
			parsedTS, _ := parseTimestamp(r.ts)
			rowsAcc = append(rowsAcc, ordered{
				ts:      parsedTS,
				ordinal: ord,
				scan:    r,
				row: activity.UsageRow{
					SessionID:       r.sessionID,
					Model:           r.model,
					Timestamp:       r.ts,
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

	sort.SliceStable(rowsAcc, func(i, j int) bool {
		a, b := rowsAcc[i], rowsAcc[j]
		if !a.ts.Equal(b.ts) {
			return a.ts.Before(b.ts)
		}
		if a.row.SessionID != b.row.SessionID {
			return a.row.SessionID < b.row.SessionID
		}
		return a.ordinal < b.ordinal
	})
	baseRows := make([]activity.UsageRow, len(rowsAcc))
	for i, o := range rowsAcc {
		baseRows[i] = o.row
	}
	mask := activity.UsageSurvivorMask(q.RangeStart, q.RangeEnd, q.EffectiveEnd, baseRows)
	out = make([]activity.UsageRow, 0, len(rowsAcc))
	for i, o := range rowsAcc {
		if !mask[i] {
			continue
		}
		_, outputTok, _, _, _, _ := dailyUsageAmounts(o.scan, rateResolver)
		cost, priced, contributes := sqliteActivityReportRowStatus(o.scan, rateResolver)
		row := o.row
		row.OutputTokens = outputTok
		row.Cost = cost
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

func sqliteActivityReportRowStatus(
	r dailyUsageScanRow, pricing *export.PricingResolver,
) (cost float64, priced, contributes bool) {
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

	if r.costUSD.Valid {
		pricing.RecordReported(r.model, pricing.Lookup(r.model))
		return r.costUSD.Float64, true, true
	}
	if inTok == 0 && outTok == 0 && reasoningTok == 0 &&
		crTok == 0 && rdTok == 0 {
		return 0, true, false
	}
	lookup := pricing.Lookup(r.model)
	if !lookup.OK {
		pricing.RecordComputed(r.model, lookup)
		return 0, false, true
	}
	cost = lookup.Rates.CostForTokens(
		inTok, outTok, reasoningTok, crTok, rdTok)
	pricing.RecordComputed(r.model, lookup)
	return cost, true, true
}
