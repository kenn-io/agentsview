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

// activityReportRangeBoundsUTC returns the exact [start, end) UTC bounds
// of the resolved range `q` as RFC3339 strings. It mirrors the SQLite and
// PostgreSQL backends so the candidate-session predicate selects exactly
// the sessions whose window intersects the range, with no padding slop.
// DuckDB compares parsed instants (the bounds are cast to TIMESTAMP), so
// it keeps the zone suffix, unlike SQLite's zone-less TEXT comparison.
func activityReportRangeBoundsUTC(q activity.Query) (string, string) {
	return q.RangeStart.UTC().Format(time.RFC3339),
		q.RangeEnd.UTC().Format(time.RFC3339)
}

// GetActivityReport assembles a concurrency- and usage-oriented report
// for the resolved range `q`, reading from the DuckDB store. It mirrors
// the SQLite (*DB).GetActivityReport and PostgreSQL
// (*Store).GetActivityReport: sessions and activity come from the filtered
// candidate set. Usage loads candidate rows plus only the cross-session Claude
// peers needed for complete-snapshot selection.
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
func (s *Store) GetActivityReport(
	ctx context.Context, f db.AnalyticsFilter, q activity.Query,
) (activity.Report, error) {
	f.IncludeSubagents = true
	f.IncludeForks = true
	rangeStartUTC, rangeEndUTC := activityReportRangeBoundsUTC(q)
	lowerBound := duckUsagePaddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
	upperBound := duckUsagePaddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)

	sessions, ids, err := s.activityReportSessions(
		ctx, f, rangeStartUTC, rangeEndUTC)
	if err != nil {
		return activity.Report{}, err
	}

	acts, err := s.activityReportActivity(ctx, ids)
	if err != nil {
		return activity.Report{}, err
	}

	usage, pricing, err := s.activityReportUsage(ctx, ids, lowerBound, upperBound, q)
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
		return activity.Report{}, fmt.Errorf("aggregating duckdb activity report: %w", err)
	}
	report.SchemaVersion = export.ActivityReportSchemaVersion
	report.Pricing = pricing
	projects, err := s.BuildProjectIdentityMap(ctx,
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
type duckSessionUsageOrderedRow struct {
	scan    duckActivityReportUsageRow
	ts      time.Time
	validTS bool
	ordinal int64
}

func (s *Store) GetSessionUsageRows(
	ctx context.Context, ids []string,
) (*activity.SessionUsageRows, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	pricing, err := s.loadPricing(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading duckdb pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(duckPricingRows(pricing))
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
	// Read every normalized row here. The session-aware ordering below applies
	// complete-snapshot selection before its own cross-session dedup pass.
	cte, queryArgs := duckUsageCTEFromRaw(
		db.UsageFilter{}, rawSQL, queryArgs, false)
	query := cte + `
		SELECT session_id, message_ordinal, ts, source, model,
			agent, claude_message_id, claude_request_id, source_uuid,
			usage_dedup_key, input_tokens_norm, output_tokens_norm,
			cache_create_norm, cache_read_norm, reasoning_tokens_norm,
			web_search_requests_norm, cost_microdollars, cost_source
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
			&r.reasoningTok, &r.webSearchRequests, &r.cost, &r.costSource,
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
	snapshotRows := make([]activity.UsageRow, len(rowsAcc))
	rowContributes := make([]bool, len(rowsAcc))
	rawOutputTokensBySession := make(map[string]int)
	for i, o := range rowsAcc {
		snapshotRows[i] = activity.UsageRow{
			SessionID:         o.scan.sessionID,
			Timestamp:         o.scan.ts,
			MessageOrdinal:    o.ordinal,
			OutputTokens:      o.scan.outputTok,
			WebSearchRequests: o.scan.webSearchRequests,
			ClaudeMessageID:   o.scan.claudeMessageID,
			ClaudeRequestID:   o.scan.claudeRequestID,
		}
		rowContributes[i] = activity.UsageDataContributes(
			o.scan.cost != nil, o.scan.inputTok, o.scan.outputTok,
			o.scan.reasoningTok, o.scan.cacheCr, o.scan.cacheRd,
			o.scan.webSearchRequests)
		rawOutputTokensBySession[o.scan.sessionID] += o.scan.outputTok
	}
	snapshotMask, snapshotAttribution, snapshotWebSearchRequests :=
		activity.ClaudeSnapshotSurvivorSelection(snapshotRows)
	seen := make(map[string]struct{})
	deduplicatedOutputTokens := make(map[string]int)
	discardedContributingSessions := make(map[string]struct{})
	out := make([]activity.UsageRow, 0, len(rowsAcc))
	for i, o := range rowsAcc {
		if !snapshotMask[i] {
			deduplicatedOutputTokens[o.scan.sessionID] +=
				snapshotRows[i].OutputTokens
			if rowContributes[i] {
				discardedContributingSessions[o.scan.sessionID] = struct{}{}
			}
			continue
		}
		r := o.scan
		r.webSearchRequests = snapshotWebSearchRequests[i]
		attributionSessionID := snapshotAttribution[i]
		if attributionSessionID != r.sessionID {
			deduplicatedOutputTokens[r.sessionID] += r.outputTok
			if rowContributes[i] {
				discardedContributingSessions[r.sessionID] = struct{}{}
			}
		}
		if key, ok := duckSessionUsageDedupKey(r); ok {
			if _, dup := seen[key]; dup {
				deduplicatedOutputTokens[r.sessionID] += r.outputTok
				if rowContributes[i] {
					discardedContributingSessions[r.sessionID] = struct{}{}
				}
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
			SessionID:       attributionSessionID,
			SourceSessionID: r.sessionID,
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

			UsageSource:         r.source,
			MessageOrdinal:      duckUsageOrdinalOrNeg(r.messageOrdinal),
			InputTokens:         r.inputTok,
			CacheCreationTokens: r.cacheCr,
			CacheReadTokens:     r.cacheRd,
			WebSearchRequests:   r.webSearchRequests,
		})
	}
	return &activity.SessionUsageRows{
		Rows:                          out,
		RawOutputTokensBySession:      rawOutputTokensBySession,
		DeduplicatedOutputTokens:      deduplicatedOutputTokens,
		DiscardedContributingSessions: discardedContributingSessions,
	}, nil
}

// duckUsageOrdinalOrNeg renders a nullable message ordinal in
// activity.UsageRow's COALESCE(message_ordinal, -1) convention.
func duckUsageOrdinalOrNeg(v any) int64 {
	o, ok := duckUsageOrdinal(v)
	if !ok {
		return -1
	}
	return o
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
	if a.scan.usageDedupKey != b.scan.usageDedupKey {
		return a.scan.usageDedupKey < b.scan.usageDedupKey
	}
	return !a.validTS && a.scan.ts < b.scan.ts
}

func activityReportProjectLabels(sessions []activity.SessionMeta) []string {
	set := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		set[session.Project] = true
	}
	return sortedBoolKeys(set)
}

// activityReportSessions returns the candidate sessions whose window
// overlaps the exact range [rangeStartUTC, rangeEndUTC), plus their
// IDs. The ID set defines the scope for the activity and usage fetches.
// DuckDB stores native timestamps, so the timestamp fallbacks need no
// NULLIF guard. The Title expression mirrors SQLite and PostgreSQL while
// intentionally excluding first_message because activity reports cross the
// summary export boundary. Empty display_name/session_name/project values do
// not win the fallback.
//
// The effective-end fallback for a session with no ended_at uses its
// latest message timestamp before started_at, so a still-open session
// that began before the range but has messages inside it is not dropped,
// matching SQLite and PostgreSQL. COALESCE short-circuits, so the
// correlated MAX subquery runs only for the rare sessions missing an
// ended_at.
func (s *Store) activityReportSessions(
	ctx context.Context, f db.AnalyticsFilter, rangeStartUTC, rangeEndUTC string,
) ([]activity.SessionMeta, []string, error) {
	where, args := duckBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", false, false)
	args = append(args, rangeStartUTC, rangeEndUTC)

	query := `SELECT
		s.id,
		COALESCE(NULLIF(s.display_name, ''), NULLIF(s.session_name, ''), NULLIF(s.project, ''), s.id) AS display_name,
		s.project,
		s.agent,
		s.machine,
		s.started_at,
		s.ended_at,
		COALESCE(s.is_automated, false) AS is_automated
	FROM sessions s
	WHERE ` + where + `
		AND COALESCE(s.ended_at,
			(SELECT MAX(m.timestamp) FROM messages m
				WHERE m.session_id = s.id AND m.timestamp IS NOT NULL),
			s.started_at, s.created_at) >= CAST(? AS TIMESTAMP)
		AND COALESCE(s.started_at, s.created_at) < CAST(? AS TIMESTAMP)`

	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"querying duckdb activity report sessions: %w", err)
	}
	defer rows.Close()

	var sessions []activity.SessionMeta
	var ids []string
	for rows.Next() {
		var m activity.SessionMeta
		var startedAt, endedAt any
		if err := rows.Scan(
			&m.SessionID, &m.Title, &m.Project, &m.Agent,
			&m.Machine, &startedAt, &endedAt, &m.IsAutomated,
		); err != nil {
			return nil, nil, fmt.Errorf(
				"scanning duckdb activity report session: %w", err)
		}
		m.StartedAt = formatDBTime(startedAt)
		m.EndedAt = formatDBTime(endedAt)
		sessions = append(sessions, m)
		ids = append(ids, m.SessionID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf(
			"iterating duckdb activity report sessions: %w", err)
	}
	return sessions, ids, nil
}

// activityReportActivity returns every timestamped message for the
// candidate sessions, ordered for the aggregator's per-session interval
// walk. It is not time-bounded so cross-boundary successor messages are
// present.
func (s *Store) activityReportActivity(
	ctx context.Context, ids []string,
) ([]activity.ActivityEvent, error) {
	var out []activity.ActivityEvent
	if len(ids) == 0 {
		return out, nil
	}
	args, placeholders := stringInArgs(ids)
	query := `SELECT session_id, ordinal, role, timestamp, model
		FROM messages
		WHERE session_id IN (` + strings.Join(placeholders, ",") + `)
			AND timestamp IS NOT NULL
		ORDER BY session_id, ordinal`

	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf(
			"querying duckdb activity report activity: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var e activity.ActivityEvent
		var ts any
		if err := rows.Scan(
			&e.SessionID, &e.Ordinal, &e.Role, &ts, &e.Model,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning duckdb activity report activity: %w", err)
		}
		e.Timestamp = formatDBTime(ts)
		if e.Timestamp == "" {
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating duckdb activity report activity: %w", err)
	}
	return out, nil
}

// duckActivityReportUsageRow is one scanned usage-union row before mapping
// into an activity.UsageRow, carrying the normalized token amounts and
// dedup keys the aggregator and per-row cost need.
type duckActivityReportUsageRow struct {
	sessionID         string
	source            string
	model             string
	ts                string
	messageOrdinal    any
	agent             string
	claudeMessageID   string
	claudeRequestID   string
	sourceUUID        string
	usageDedupKey     string
	inputTok          int
	outputTok         int
	cacheCr           int
	cacheRd           int
	reasoningTok      int
	webSearchRequests int
	cost              *int64
	costSource        string
}

// activityReportUsage selects complete snapshots across the padded range,
// then keeps rows attributed to the candidate sessions. Cost is computed
// after selection so filtered rows do not affect pricing metadata.
func (s *Store) activityReportUsage(
	ctx context.Context, ids []string, lowerBound, upperBound string, q activity.Query,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	out := []activity.UsageRow{}

	pricing, err := s.loadPricing(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("loading duckdb pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(duckPricingRows(pricing))
	if len(ids) == 0 {
		block, err := rateResolver.BuildBlock()
		if err != nil {
			return nil, nil, fmt.Errorf("building pricing block: %w", err)
		}
		return out, &block, nil
	}

	// Accumulate the parsed ts and dedup ordinal alongside each mapped row
	// so a single global (ts, session_id, ordinal) order can be imposed
	// before the aggregator's first-seen dedup.
	type ordered struct {
		row     activity.UsageRow
		scan    duckActivityReportUsageRow
		ts      time.Time
		ordinal int64
	}
	var rowsAcc []ordered
	loadRows := func(
		query string, args []any, skipSessionIDs map[string]struct{},
	) error {
		rows, queryErr := s.queryContext(ctx, query, args...)
		if queryErr != nil {
			return fmt.Errorf("querying duckdb activity report usage: %w", queryErr)
		}
		defer rows.Close()
		for rows.Next() {
			var r duckActivityReportUsageRow
			if err := rows.Scan(
				&r.sessionID, &r.messageOrdinal, &r.ts, &r.source, &r.model,
				&r.agent, &r.claudeMessageID, &r.claudeRequestID, &r.sourceUUID,
				&r.usageDedupKey,
				&r.inputTok, &r.outputTok, &r.cacheCr, &r.cacheRd,
				&r.reasoningTok, &r.webSearchRequests, &r.cost, &r.costSource,
			); err != nil {
				return fmt.Errorf(
					"scanning duckdb activity report usage: %w", err)
			}
			if _, skip := skipSessionIDs[r.sessionID]; skip {
				continue
			}
			tsStr := formatDBTime(r.ts)
			ord := int64(-1)
			if o, ok := duckUsageOrdinal(r.messageOrdinal); ok {
				ord = o
			}
			parsedTS, _ := parseTimestamp(tsStr)
			rowsAcc = append(rowsAcc, ordered{
				ts:      parsedTS,
				ordinal: ord,
				scan:    r,
				row: activity.UsageRow{
					SessionID:         r.sessionID,
					Model:             r.model,
					Timestamp:         tsStr,
					OutputTokens:      r.outputTok,
					WebSearchRequests: r.webSearchRequests,
					Agent:             r.agent,
					ClaudeMessageID:   r.claudeMessageID,
					ClaudeRequestID:   r.claudeRequestID,
					SourceUUID:        r.sourceUUID,
					UsageDedupKey:     r.usageDedupKey,
				},
			})
		}
		return rows.Err()
	}

	const usageChunk = 500
	for i := 0; i < len(ids); i += usageChunk {
		end := min(i+usageChunk, len(ids))
		chunkArgs, placeholders := stringInArgs(ids[i:end])
		inClause := "(" + strings.Join(placeholders, ",") + ")"
		query := duckActivityReportUsageQuery(
			"AND m.session_id IN "+inClause,
			"AND ue.session_id IN "+inClause)
		args := make([]any, 0, len(chunkArgs)*2+2)
		args = append(args, chunkArgs...)
		args = append(args, chunkArgs...)
		args = append(args, lowerBound, upperBound)
		if err := loadRows(query, args, nil); err != nil {
			return nil, nil, err
		}
	}

	type snapshotKey struct {
		messageID string
		requestID string
	}
	keySet := make(map[snapshotKey]struct{})
	for _, candidate := range rowsAcc {
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
	for i := 0; i < len(keys); i += usageChunk {
		end := min(i+usageChunk, len(keys))
		predicates := make([]string, 0, end-i)
		args := make([]any, 0, (end-i)*2+2)
		for _, key := range keys[i:end] {
			predicates = append(predicates,
				"(m.claude_message_id = ? AND m.claude_request_id = ?)")
			args = append(args, key.messageID, key.requestID)
		}
		query := duckActivityReportUsageQuery(
			"AND ("+strings.Join(predicates, " OR ")+")", "AND FALSE")
		args = append(args, lowerBound, upperBound)
		if err := loadRows(query, args, candidateIDs); err != nil {
			return nil, nil, err
		}
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
	mask, attribution, webSearchRequests :=
		activity.UsageSurvivorSelectionForSessions(
			q.RangeStart, q.RangeEnd, q.EffectiveEnd, baseRows, ids,
		)
	out = make([]activity.UsageRow, 0, len(rowsAcc))
	for i, o := range rowsAcc {
		if !mask[i] {
			continue
		}
		costRow := o.scan
		costRow.webSearchRequests = webSearchRequests[i]
		cost, costSource, priced, contributes, sessionCost, priceErr :=
			duckActivityUsageCost(costRow, rateResolver)
		if priceErr != nil {
			return nil, nil, priceErr
		}
		row := o.row
		row.SessionID = attribution[i]
		row.WebSearchRequests = webSearchRequests[i]
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

// duckActivityReportUsageQuery builds the per-row usage-union SQL across the
// padded time range. It applies the same message and usage-event
// eligibility predicates as GetDailyUsage (empty token_usage, empty, and
// synthetic models excluded) so the daily totals match the Usage
// dashboard, normalizes the per-source token columns in SQL, and bounds
// rows to the padded range window. Cost is computed per row in Go.
func duckActivityReportUsageQuery(messageFilter, eventFilter string) string {
	return fmt.Sprintf(`
		WITH usage_raw AS (
			SELECT m.session_id AS session_id, m.ordinal AS message_ordinal,
				'message' AS source, COALESCE(m.timestamp, s.started_at) AS ts,
				m.model AS model, m.token_usage AS token_json,
				s.agent AS agent,
				m.claude_message_id AS claude_message_id,
				m.claude_request_id AS claude_request_id,
				m.source_uuid AS source_uuid,
				'' AS usage_dedup_key,
				0 AS input_tokens, 0 AS output_tokens,
					0 AS cache_create, 0 AS cache_read,
					COALESCE(TRY_CAST(json_extract_string(m.token_usage, '$.reasoning_tokens') AS BIGINT), 0) AS reasoning_tokens,
					NULL AS cost_microdollars, '' AS cost_source
			FROM messages m
			JOIN sessions s ON s.id = m.session_id
			WHERE m.token_usage != ''
				AND m.model != ''
				AND m.model != '<synthetic>'
				AND s.deleted_at IS NULL
				%[2]s
			UNION ALL
			SELECT ue.session_id AS session_id, ue.message_ordinal AS message_ordinal,
				ue.source AS source, COALESCE(ue.occurred_at, s.started_at) AS ts,
				ue.model AS model, '' AS token_json,
				s.agent AS agent,
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
					ue.cost_source AS cost_source
			FROM usage_events ue
			JOIN sessions s ON s.id = ue.session_id
			WHERE ue.model != ''
				AND s.deleted_at IS NULL
				%[3]s
		),
		usage_normalized AS (
			SELECT session_id, message_ordinal, ts, source, model, agent,
				claude_message_id, claude_request_id, source_uuid, usage_dedup_key,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.input_tokens') AS BIGINT), 0), 0), %[1]d)
					WHEN source = 'session' THEN GREATEST(input_tokens, 0)
					ELSE LEAST(GREATEST(input_tokens, 0), %[1]d)
				END AS input_tokens_norm,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.output_tokens') AS BIGINT), 0), 0), %[1]d)
					WHEN source = 'session' THEN GREATEST(output_tokens, 0)
					ELSE LEAST(GREATEST(output_tokens, 0), %[1]d)
				END AS output_tokens_norm,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_creation_input_tokens') AS BIGINT), 0), 0), %[1]d)
					WHEN source = 'session' THEN GREATEST(cache_create, 0)
					ELSE LEAST(GREATEST(cache_create, 0), %[1]d)
				END AS cache_create_norm,
					CASE
						WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_read_input_tokens') AS BIGINT), 0), 0), %[1]d)
						WHEN source = 'session' THEN GREATEST(cache_read, 0)
						ELSE LEAST(GREATEST(cache_read, 0), %[1]d)
					END AS cache_read_norm,
					CASE
						WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.reasoning_tokens') AS BIGINT), 0), 0), %[1]d)
						WHEN source = 'session' THEN GREATEST(reasoning_tokens, 0)
						ELSE LEAST(GREATEST(reasoning_tokens, 0), %[1]d)
					END AS reasoning_tokens_norm,
					CASE
						WHEN source = 'message' THEN GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.server_tool_use.web_search_requests') AS BIGINT), 0), 0)
						ELSE 0
					END AS web_search_requests_norm,
					cost_microdollars, cost_source
			FROM usage_raw
		)
		SELECT session_id, message_ordinal, ts, source, model, agent,
				claude_message_id, claude_request_id, source_uuid, usage_dedup_key,
				input_tokens_norm, output_tokens_norm,
				cache_create_norm, cache_read_norm, reasoning_tokens_norm,
				web_search_requests_norm,
				cost_microdollars, cost_source
		FROM usage_normalized
		WHERE ts >= CAST(? AS TIMESTAMP)
			AND ts <= CAST(? AS TIMESTAMP)`,
		db.MaxPlausibleTokens, messageFilter, eventFilter)
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
	var billableWebSearch int
	if r.cost != nil {
		explicitCost = *r.cost
		priced = true
		contributes = true
	} else if activity.UsageDataContributes(
		false, r.inputTok, r.outputTok, r.reasoningTok,
		r.cacheCr, r.cacheRd, r.webSearchRequests,
	) {
		contributes = true
		_, lookup := pricing.Resolve(r.model, canonicalModel)
		priced = lookup.OK
		billableInput = r.inputTok
		billableOutput = r.outputTok
		billableReasoning = r.reasoningTok
		billableCacheCr = r.cacheCr
		billableCacheRd = r.cacheRd
		billableWebSearch = r.webSearchRequests
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
		billableCacheCr, billableCacheRd, billableWebSearch,
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
