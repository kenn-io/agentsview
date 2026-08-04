package db

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/uptrace/bun"
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

func activityReportProjectLabels(
	sessions []activity.SessionMeta,
) []string {
	set := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		set[session.Project] = struct{}{}
	}
	return sortedSetKeys(set)
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

// activityReportUsageCandidate retains the scanned source fields until the
// survivor set is known. Pricing provenance is recorded only for survivors in
// the ordinary activity report, while reporting export can materialize every
// raw candidate and perform its one combined survivor pass later.
type activityReportUsageCandidate struct {
	row     activity.UsageRow
	scan    dailyUsageScanRow
	ts      time.Time
	validTS bool
	ordinal int64
}

func (db *BunStore) loadActivityReportUsageCandidatesFrom(
	ctx context.Context,
	source bun.IDB,
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
			parsedTS, tsErr := parseTimestamp(r.ts)
			candidates = append(candidates, activityReportUsageCandidate{
				ordinal: ord,
				scan:    r,
				ts:      parsedTS,
				validTS: tsErr == nil,
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
		if a.validTS && b.validTS {
			if !a.ts.Equal(b.ts) {
				return a.ts.Before(b.ts)
			}
		} else {
			if a.validTS != b.validTS {
				return a.validTS
			}
			if a.row.Timestamp != b.row.Timestamp {
				return a.row.Timestamp < b.row.Timestamp
			}
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
func (db *BunStore) activityReportUsageCandidatesFrom(
	ctx context.Context,
	source bun.IDB,
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
	out, err := materializeActivityReportUsageRows(candidates, mask, rateResolver)
	if err != nil {
		return nil, nil, err
	}
	block, err := rateResolver.BuildBlock()
	if err != nil {
		return nil, nil, fmt.Errorf("building pricing block: %w", err)
	}
	return out, &block, nil
}

func materializeActivityReportUsageRows(
	candidates []activityReportUsageCandidate,
	mask []bool,
	rateResolver *export.PricingResolver,
) ([]activity.UsageRow, error) {
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
			return nil, priceErr
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
	return out, nil
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
	pricedModel, lookup := pricing.Resolve(r.model, dailyUsageLookupModel(r))
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
