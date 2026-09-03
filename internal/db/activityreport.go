package db

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
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

func reportProgress(callback activity.ProgressFunc, progress activity.Progress) {
	if callback != nil {
		callback(progress)
	}
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

// activityReportCandidates returns adjacent timestamped-message pairs whose
// start can contribute to the report. The timestamp bound applies only to the
// start row; the correlated successor lookup preserves true ordinal adjacency
// and intentionally has no right timestamp bound.
func (db *DB) activityReportCandidates(
	ctx context.Context, ids []string, q activity.Query,
) ([]activity.IntervalCandidate, error) {
	var out []activity.IntervalCandidate
	err := db.activityReportCandidateSource(ids, q)(
		ctx, func(candidate activity.IntervalCandidate) error {
			out = append(out, candidate)
			return nil
		},
	)
	return out, err
}

const activityReportCandidatesSQL = `SELECT
	m.session_id, m.ordinal, successor.ordinal,
	m.timestamp, successor.timestamp,
	successor.role, successor.model,
	COALESCE((
		SELECT prior.model
		FROM messages prior
		WHERE prior.session_id = m.session_id
			AND prior.ordinal <= m.ordinal
			AND prior.role = 'assistant'
			AND prior.model != ''
			AND prior.timestamp IS NOT NULL
			AND prior.timestamp != ''
			AND agentsview_timestamp_unix_micro(prior.timestamp) IS NOT NULL
			AND agentsview_timestamp_unix_micro(prior.timestamp) > (
				SELECT agentsview_timestamp_unix_micro(prior_previous.timestamp)
				FROM messages prior_previous
				WHERE prior_previous.session_id = prior.session_id
					AND prior_previous.ordinal < prior.ordinal
					AND prior_previous.timestamp IS NOT NULL
					AND prior_previous.timestamp != ''
					AND agentsview_timestamp_unix_micro(
						prior_previous.timestamp) IS NOT NULL
				ORDER BY prior_previous.ordinal DESC
				LIMIT 1
			)
		ORDER BY prior.ordinal DESC
		LIMIT 1
	), 'unknown')
FROM messages m INDEXED BY idx_messages_velocity
JOIN messages successor ON successor.id = (
	SELECT next.id
	FROM messages next
	WHERE next.session_id = m.session_id
		AND next.ordinal > m.ordinal
		AND next.timestamp IS NOT NULL
		AND next.timestamp != ''
		AND agentsview_timestamp_unix_micro(next.timestamp) IS NOT NULL
	ORDER BY next.ordinal
	LIMIT 1
)
WHERE m.session_id IN (SELECT value FROM json_each(?))
	AND m.timestamp IS NOT NULL
	AND m.timestamp != ''
	AND m.timestamp >= ?
	AND m.timestamp < ?
	AND agentsview_timestamp_unix_micro(m.timestamp) IS NOT NULL
	AND agentsview_timestamp_unix_micro(m.timestamp) >= ?
	AND agentsview_timestamp_unix_micro(m.timestamp) < ?
ORDER BY agentsview_timestamp_unix_micro(m.timestamp),
	m.session_id, m.ordinal`

func (db *DB) activityReportCandidateSource(
	ids []string, q activity.Query,
) activity.CandidateSource {
	return func(
		ctx context.Context,
		yield func(activity.IntervalCandidate) error,
	) error {
		if len(ids) == 0 {
			return nil
		}
		encodedIDs, err := json.Marshal(ids)
		if err != nil {
			return fmt.Errorf("encoding activity report session IDs: %w", err)
		}
		gapCap := time.Duration(q.GapCapSeconds) * time.Second
		lowerTime := q.RangeStart.Add(-gapCap).UTC()
		upperTime := q.EffectiveEnd.UTC()
		lower := lowerTime.UnixMicro()
		upper := upperTime.UnixMicro()
		paddedLower := paddedUTCBound(lowerTime.Format(time.RFC3339), -14)
		paddedUpper := paddedUTCBound(upperTime.Format(time.RFC3339), 14)
		args := []any{string(encodedIDs), paddedLower, paddedUpper, lower, upper}
		rows, err := db.getReader().QueryContext(
			ctx, activityReportCandidatesSQL, args...,
		)
		if err != nil {
			return fmt.Errorf("querying activity report candidates: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var candidate activity.IntervalCandidate
			var start, end string
			if err := rows.Scan(
				&candidate.SessionID, &candidate.StartOrdinal,
				&candidate.EndOrdinal, &start, &end,
				&candidate.ClosingRole, &candidate.ClosingModel,
				&candidate.PriorModel,
			); err != nil {
				return fmt.Errorf("scanning activity report candidate: %w", err)
			}
			var err error
			candidate.Start, err = time.Parse(time.RFC3339Nano, start)
			if err != nil {
				return fmt.Errorf("parsing activity candidate start: %w", err)
			}
			candidate.End, err = time.Parse(time.RFC3339Nano, end)
			if err != nil {
				return fmt.Errorf("parsing activity candidate end: %w", err)
			}
			candidate.Start = candidate.Start.UTC()
			candidate.End = candidate.End.UTC()
			if err := yield(candidate); err != nil {
				return err
			}
		}
		return rows.Err()
	}
}

// ActivityReportCandidateSource exposes the backend's mechanical pairing
// stream for cross-backend contract tests. Activity semantics remain in the
// shared aggregator.
func (db *DB) ActivityReportCandidateSource(
	ids []string, q activity.Query,
) activity.CandidateSource {
	return db.activityReportCandidateSource(ids, q)
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
	restrictToIDs bool,
) ([]activityReportUsageCandidate, *export.PricingResolver, error) {
	pricing, err := db.loadPricingMapFrom(ctx, source)
	if err != nil {
		return nil, nil, fmt.Errorf("loading pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)
	if len(ids) == 0 {
		return []activityReportUsageCandidate{}, rateResolver, nil
	}

	var candidates []activityReportUsageCandidate
	loadRows := func(
		rowsSQL string, args []any, skipSessionIDs map[string]struct{},
	) error {
		query := dailyUsageRowSelectFromRowsWithMachine(rowsSQL, true) + `
			AND u.ts >= ? AND u.ts <= ?`
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
			if _, skip := skipSessionIDs[r.sessionID]; skip {
				continue
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
					ProviderID:      r.providerID,
					ClaudeMessageID: r.claudeMessageID,
					ClaudeRequestID: r.claudeRequestID,
					SourceUUID:      r.sourceUUID,
					UsageDedupKey:   r.usageDedupKey,
				},
			})
		}
		return rows.Err()
	}

	// Load only rows owned by candidate sessions first. Reporting export stops
	// here because it combines multiple candidate sets before selecting usage.
	// Ordinary reports then fetch only cross-session Claude peers needed to
	// choose complete snapshots for those candidates.
	const usageVarChunk = (maxSQLVars - 2) / 2
	err = queryChunkedSize(ids, usageVarChunk, func(chunk []string) error {
		ph, chunkArgs := inPlaceholders(chunk)
		rowsSQL := dailyUsageRowsSQLWithWhere(
			usageMessageEligibility+" AND m.session_id IN "+ph,
			usageEventEligibility+" AND ue.session_id IN "+ph)
		args := make([]any, 0, len(chunkArgs)*2)
		args = append(args, chunkArgs...)
		args = append(args, chunkArgs...)
		return loadRows(rowsSQL, args, nil)
	})
	if err != nil {
		return nil, nil, err
	}
	if restrictToIDs {
		return candidates, rateResolver, nil
	}

	type snapshotKey struct {
		messageID string
		requestID string
	}
	keySet := make(map[snapshotKey]struct{})
	for _, candidate := range candidates {
		if candidate.row.ClaudeMessageID == "" || candidate.row.ClaudeRequestID == "" {
			continue
		}
		keySet[snapshotKey{
			messageID: candidate.row.ClaudeMessageID,
			requestID: candidate.row.ClaudeRequestID,
		}] = struct{}{}
	}
	keys := make([]snapshotKey, 0, len(keySet))
	for key := range keySet {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].messageID != keys[j].messageID {
			return keys[i].messageID < keys[j].messageID
		}
		return keys[i].requestID < keys[j].requestID
	})
	candidateIDs := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		candidateIDs[id] = struct{}{}
	}
	if len(keys) > 0 {
		pairs := make([][2]string, len(keys))
		for i, key := range keys {
			pairs[i] = [2]string{key.messageID, key.requestID}
		}
		encodedPairs, marshalErr := json.Marshal(pairs)
		if marshalErr != nil {
			return nil, nil, fmt.Errorf(
				"encoding activity report Claude snapshot keys: %w", marshalErr)
		}
		rowsSQL := dailyUsageRowsSQLWithWhere(
			usageMessageEligibility+` AND m.claude_message_id != ''
				AND m.claude_request_id != ''
				AND (m.claude_message_id, m.claude_request_id) IN (
					SELECT json_extract(peer.value, '$[0]'),
					       json_extract(peer.value, '$[1]')
					FROM json_each(?) AS peer
				)`,
			usageEventEligibility+" AND 1 = 0")
		if err := loadRows(rowsSQL, []any{string(encodedPairs)}, candidateIDs); err != nil {
			return nil, nil, err
		}
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
	includeWebSearch bool,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	candidates, rateResolver, err := db.loadActivityReportUsageCandidatesFrom(
		ctx, source, ids, lowerBound, upperBound, true,
	)
	if err != nil {
		return nil, nil, err
	}
	var webSearchRequests []int
	if !includeWebSearch {
		webSearchRequests = make([]int, len(candidates))
	}
	return materializeActivityReportUsageCandidates(
		candidates, nil, nil, webSearchRequests, rateResolver,
	)
}

func materializeActivityReportUsageCandidates(
	candidates []activityReportUsageCandidate,
	mask []bool,
	attribution []string,
	webSearchRequests []int,
	rateResolver *export.PricingResolver,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	out, err := materializeActivityReportUsageRows(
		candidates, mask, attribution, webSearchRequests, rateResolver,
	)
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
	attribution []string,
	webSearchRequests []int,
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
		webSearches := usageRowWebSearchRequests(
			candidate.scan.usageSource, candidate.scan.tokenJSON)
		if webSearchRequests != nil {
			webSearches = webSearchRequests[i]
		}
		cost, priced, contributes, priceErr :=
			sqliteActivityReportRowStatusWithWebSearchRequests(
				costRow, webSearches, rateResolver)
		if priceErr != nil {
			return nil, priceErr
		}
		costSource := export.CostSourceComputed
		if costRow.cost.Valid {
			costSource = export.CostSourceReported
		}
		row := candidate.row
		if attribution != nil {
			row.SessionID = attribution[i]
		}
		row.InputTokens = inputTok
		row.OutputTokens = outputTok
		row.CacheCreationTokens = cacheCrTok
		row.CacheReadTokens = cacheRdTok
		row.WebSearchRequests = webSearches
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
	return sqliteActivityReportRowStatusWithWebSearchRequests(
		r, dailyUsageRowWebSearchRequests(r), pricing)
}

func sqliteActivityReportRowStatusWithWebSearchRequests(
	r dailyUsageScanRow, webSearches int, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	pricedModel, lookup := pricing.ResolveAt(
		r.model, dailyUsageLookupModel(r), r.pricingTime,
	)
	var inTok, outTok, crTok, cr1hTok, rdTok int
	reasoningTok := r.reasoningTokens
	if r.usageSource == "message" {
		inTok, outTok, crTok, rdTok, reasoningTok =
			clampedUsageTokenCountersWithReasoning(r.tokenJSON)
		cr1hTok = clampedCacheCreation1hTokens(r.tokenJSON)
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
		crTok == 0 && rdTok == 0 && webSearches == 0 {
		return money.Money{}, true, false, nil
	}
	if !lookup.OK {
		pricing.RecordResolvedComputed(r.model, pricedModel, lookup)
		fee, feeErr := export.WebSearchFee(webSearches)
		if feeErr != nil {
			return money.Money{}, false, false, feeErr
		}
		return fee, false, true, nil
	}
	pricedModel, lookup, err = pricing.ResolveBilledAt(
		r.providerID, r.model, usageLookupModel(r.model, r.pricingTS),
		usagePricingTimestamp(r.pricingTS))
	if err != nil {
		return money.Money{}, false, false, err
	}
	requestScoped := usageRowIsRequestScoped(r.usageSource, r.messageOrdinal)
	cost, err = lookup.Rates.CostForTokensScoped(
		requestScoped,
		inTok, outTok, reasoningTok, crTok, cr1hTok, rdTok)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing activity usage for model %q: %w", r.model, err)
	}
	cost, err = export.AddWebSearchFee(cost, webSearches)
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
