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
// (*Store).GetActivityReport: three fetches scoped to the SAME candidate
// session-ID set so the concurrency timeline, sessions table, and usage
// totals stay mutually consistent (no orphan usage rows), then the
// in-memory streams are handed to activity.Aggregate.
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
	projects, err := s.BuildProjectIdentityMap(ctx,
		activityReportProjectLabels(sessions))
	if err != nil {
		return activity.Report{}, err
	}
	report.Projects = projects
	return report, nil
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
// NULLIF guard. The Title expression mirrors the shared NULLIF/COALESCE
// shape used by SQLite and PostgreSQL so an empty-string
// display_name/session_name/first_message/project never wins the COALESCE
// fallback (those columns can be empty here, and project is NOT NULL with an
// empty-string default), yielding the same Title as the other two backends
// instead of a possibly-empty one.
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
		COALESCE(NULLIF(s.display_name, ''), NULLIF(s.session_name, ''), NULLIF(s.first_message, ''), NULLIF(s.project, ''), s.id) AS display_name,
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
	sessionID       string
	source          string
	model           string
	ts              string
	messageOrdinal  any
	agent           string
	claudeMessageID string
	claudeRequestID string
	sourceUUID      string
	usageDedupKey   string
	inputTok        int
	outputTok       int
	cacheCr         int
	cacheRd         int
	reasoningTok    int
	costUSD         *float64
}

// activityReportUsage returns the usage rows for the candidate sessions
// within the padded range bounds, with per-row cost computed up front
// (mirroring GetDailyUsage's cost logic) so cost stays in the backend.
// Rows are delivered as one globally ordered stream by
// (ts, session_id, message_ordinal) as the aggregator's first-seen-wins
// dedup requires. The ordering is computed in Go on the parsed time
// value, not the formatted string, to avoid fractional-second lexical
// issues.
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

	idArgs, placeholders := stringInArgs(ids)
	inClause := strings.Join(placeholders, ",")
	query := duckActivityReportUsageQuery(inClause)
	args := make([]any, 0, len(idArgs)*2+2)
	args = append(args, idArgs...) // message-source IN
	args = append(args, idArgs...) // event-source IN
	args = append(args, lowerBound, upperBound)

	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("querying duckdb activity report usage: %w", err)
	}
	defer rows.Close()

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

	for rows.Next() {
		var r duckActivityReportUsageRow
		if err := rows.Scan(
			&r.sessionID, &r.messageOrdinal, &r.ts, &r.source, &r.model,
			&r.agent, &r.claudeMessageID, &r.claudeRequestID, &r.sourceUUID,
			&r.usageDedupKey,
			&r.inputTok, &r.outputTok, &r.cacheCr, &r.cacheRd,
			&r.reasoningTok, &r.costUSD,
		); err != nil {
			return nil, nil, fmt.Errorf(
				"scanning duckdb activity report usage: %w", err)
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
				SessionID:       r.sessionID,
				Model:           r.model,
				Timestamp:       tsStr,
				OutputTokens:    r.outputTok,
				Agent:           r.agent,
				ClaudeMessageID: r.claudeMessageID,
				ClaudeRequestID: r.claudeRequestID,
				SourceUUID:      r.sourceUUID,
				UsageDedupKey:   r.usageDedupKey,
			},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf(
			"iterating duckdb activity report usage: %w", err)
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
		_, cost := duckActivityReportRowCost(o.scan, rateResolver)
		row := o.row
		row.Cost = cost
		out = append(out, row)
	}
	block, err := rateResolver.BuildBlock()
	if err != nil {
		return nil, nil, fmt.Errorf("building pricing block: %w", err)
	}
	return out, &block, nil
}

// duckActivityReportUsageQuery builds the per-row usage-union SQL scoped to
// the candidate sessions. It applies the same message and usage-event
// eligibility predicates as GetDailyUsage (empty token_usage, empty, and
// synthetic models excluded) so the daily totals match the Usage
// dashboard, normalizes the per-source token columns in SQL, and bounds
// rows to the padded range window. inClause is the comma-joined "?"
// placeholder list; it is interpolated twice (message source, event
// source). Cost is computed per row in Go.
func duckActivityReportUsageQuery(inClause string) string {
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
					NULL AS cost_usd
			FROM messages m
			JOIN sessions s ON s.id = m.session_id
			WHERE m.token_usage != ''
				AND m.model != ''
				AND m.model != '<synthetic>'
				AND s.deleted_at IS NULL
				AND m.session_id IN (%[1]s)
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
					ue.cost_usd AS cost_usd
			FROM usage_events ue
			JOIN sessions s ON s.id = ue.session_id
			WHERE ue.model != ''
				AND s.deleted_at IS NULL
				AND ue.session_id IN (%[1]s)
		),
		usage_normalized AS (
			SELECT session_id, message_ordinal, ts, source, model, agent,
				claude_message_id, claude_request_id, source_uuid, usage_dedup_key,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.input_tokens') AS BIGINT), 0), 0), %[2]d)
					WHEN source = 'session' THEN GREATEST(input_tokens, 0)
					ELSE LEAST(GREATEST(input_tokens, 0), %[2]d)
				END AS input_tokens_norm,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.output_tokens') AS BIGINT), 0), 0), %[2]d)
					WHEN source = 'session' THEN GREATEST(output_tokens, 0)
					ELSE LEAST(GREATEST(output_tokens, 0), %[2]d)
				END AS output_tokens_norm,
				CASE
					WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_creation_input_tokens') AS BIGINT), 0), 0), %[2]d)
					WHEN source = 'session' THEN GREATEST(cache_create, 0)
					ELSE LEAST(GREATEST(cache_create, 0), %[2]d)
				END AS cache_create_norm,
					CASE
						WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_read_input_tokens') AS BIGINT), 0), 0), %[2]d)
						WHEN source = 'session' THEN GREATEST(cache_read, 0)
						ELSE LEAST(GREATEST(cache_read, 0), %[2]d)
					END AS cache_read_norm,
					CASE
						WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.reasoning_tokens') AS BIGINT), 0), 0), %[2]d)
						WHEN source = 'session' THEN GREATEST(reasoning_tokens, 0)
						ELSE LEAST(GREATEST(reasoning_tokens, 0), %[2]d)
					END AS reasoning_tokens_norm,
					cost_usd
			FROM usage_raw
		)
		SELECT session_id, message_ordinal, ts, source, model, agent,
				claude_message_id, claude_request_id, source_uuid, usage_dedup_key,
				input_tokens_norm, output_tokens_norm,
				cache_create_norm, cache_read_norm, reasoning_tokens_norm, cost_usd
		FROM usage_normalized
		WHERE ts >= CAST(? AS TIMESTAMP)
			AND ts <= CAST(? AS TIMESTAMP)`, inClause, db.MaxPlausibleTokens)
}

// duckActivityReportRowCost computes one usage row's cost the same way
// GetDailyUsage does: an explicit cost_usd wins, otherwise the per-model
// rates price the normalized token amounts. Billable amounts equal the
// normalized amounts when there is no explicit cost (mirroring the
// billable_* SQL in dailyUsageAggregateRows). It returns the cache
// savings delta and the cost.
func duckActivityReportRowCost(
	r duckActivityReportUsageRow, pricing *export.PricingResolver,
) (savings, cost float64) {
	var explicitCost float64
	var billableInput, billableOutput, billableReasoning, billableCacheCr, billableCacheRd int
	if r.costUSD != nil {
		explicitCost = *r.costUSD
	} else {
		billableInput = r.inputTok
		billableOutput = r.outputTok
		billableReasoning = r.reasoningTok
		billableCacheCr = r.cacheCr
		billableCacheRd = r.cacheRd
	}
	cost, savings, _, _ = duckUsageAggregateCost(
		r.model,
		r.inputTok, r.outputTok, r.cacheCr, r.cacheRd,
		billableInput, billableOutput, billableReasoning,
		billableCacheCr, billableCacheRd,
		explicitCost,
		r.costUSD != nil,
		pricing,
	)
	return savings, cost
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
