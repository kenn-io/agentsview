package duckdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/usagefacts"
)

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
	artifacts, err := s.BuildActivityReportArtifacts(ctx, f, q, nil)
	if err != nil {
		return activity.Report{}, err
	}
	return activity.ReportWithSessions(artifacts), nil
}

type activityReportBackend struct {
	*Store
	filter                 db.AnalyticsFilter
	rangeStartUTC          string
	rangeEndUTC            string
	lowerBound, upperBound string
	kept                   *activityReportKept
}

// activityReportKept holds the kept entries of the build's sessions, from
// the memo or its pairing scan, so message counts read no messages.
type activityReportKept struct {
	sessions map[string]activityRunsEntry
	complete bool
}

func (b activityReportBackend) ActivityReportRunSource(ids []string, q activity.Query) activity.RunSource {
	return func(ctx context.Context, yield func(activity.Run) error) error {
		runs, sessions, err := b.activityReportRuns(ctx, ids, q)
		if err != nil {
			return err
		}
		b.kept.sessions, b.kept.complete = sessions, true
		// The consumer checks for cancellation before each run.
		for _, run := range runs {
			if err := yield(run); err != nil {
				return err
			}
		}
		return nil
	}
}

func (b activityReportBackend) ActivityReportUsage(
	ctx context.Context, ids []string, q activity.Query,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	return b.activityReportUsage(
		ctx, ids, b.filter, b.rangeStartUTC, b.rangeEndUTC,
		b.lowerBound, b.upperBound, q,
	)
}

// ActivityReportMessageCounts counts the messages the kept entries hold.
func (b activityReportBackend) ActivityReportMessageCounts(
	ctx context.Context, ids []string, q activity.Query, artifacts *activity.CandidateArtifacts,
) error {
	if !b.kept.complete {
		return errors.New("activity message counts requested before pairing finished")
	}
	counts := activity.NewMessageAccumulator(q, artifacts)
	for _, id := range ids {
		session := b.kept.sessions[id]
		counts.AddSorted(id, "user", session.users)
		counts.AddSorted(id, "assistant", session.assistants)
	}
	return ctx.Err()
}

func (s *Store) BuildActivityReportArtifacts(
	ctx context.Context,
	f db.AnalyticsFilter,
	q activity.Query,
	onProgress activity.ProgressFunc,
) (activity.CandidateArtifacts, error) {
	activity.ReportProgress(onProgress, activity.Progress{Phase: activity.ProgressLoadingSessions})
	f.IncludeSubagents = true
	f.IncludeForks = true
	// Take the key before the first read. A build spanning a handle swap
	// stays under the old stamp, which later requests cannot reach.
	var selection, key string
	onDisk := !q.Partial && kept.DayPreset(q, time.Now())
	if stamp := s.mirrorStamp(); !q.Partial && stamp != "" && !db.TerminationUsesTime(f.Termination) {
		selection = kept.Selection(f, q)
		key = stamp + "|" + s.pricingDigest
		if report, ok := s.reports.Get(selection, key, onDisk); ok {
			activity.ReportProgress(onProgress, report.Done)
			return report.Artifacts, nil
		}
	}
	rangeStartUTC, rangeEndUTC := db.ActivityReportInstantBoundsUTC(q)
	lowerBound := db.PaddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
	upperBound := db.PaddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)

	sessions, ids, err := s.activityReportSessions(
		ctx, f, rangeStartUTC, rangeEndUTC)
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}
	activity.ReportProgress(onProgress, activity.Progress{
		Phase: activity.ProgressLoadingUsage, SessionsTotal: len(sessions),
	})

	backend := activityReportBackend{
		Store: s, filter: f,
		rangeStartUTC: rangeStartUTC, rangeEndUTC: rangeEndUTC,
		lowerBound: lowerBound, upperBound: upperBound,
		kept: &activityReportKept{},
	}
	artifacts, done, err := activity.BuildReportArtifacts(
		ctx, backend, sessions, ids, q, onProgress, "aggregating duckdb activity report")
	if err == nil && key != "" {
		s.reports.Put(selection, key, onDisk, kept.Report{Artifacts: artifacts, Done: done})
	}
	return artifacts, err
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
	rateResolver, err := s.loadPricingResolver(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading duckdb pricing: %w", err)
	}
	sessionOrder := make(map[string]int, len(ids))
	for i, id := range ids {
		sessionOrder[id] = i
	}
	inClause, args := db.InPlaceholders(ids)
	rawSQL := fmt.Sprintf(`
		SELECT m.session_id AS session_id, m.ordinal AS message_ordinal,
			'message' AS source, COALESCE(m.timestamp, s.started_at) AS ts,
			m.timestamp AS pricing_ts,
			m.model AS model, m.provider_id AS provider_id, m.token_usage AS token_json,
			m.claude_message_id AS claude_message_id,
			m.claude_request_id AS claude_request_id,
			m.source_uuid AS source_uuid,
			'' AS usage_dedup_key,
			0 AS input_tokens, 0 AS output_tokens,
			0 AS cache_create, 0 AS cache_read,
			COALESCE(TRY_CAST(json_extract_string(m.token_usage, '$.reasoning_tokens') AS BIGINT), 0) AS reasoning_tokens,
			CASE WHEN TRY_CAST(json_extract_string(m.token_usage, '$.recorded_cost_microdollars') AS BIGINT) >= 0 THEN TRY_CAST(json_extract_string(m.token_usage, '$.recorded_cost_microdollars') AS BIGINT) END AS cost_microdollars,
				'' AS cost_source,
				CASE WHEN TRY_CAST(json_extract_string(m.token_usage, '$.recorded_cost_microdollars') AS BIGINT) >= 0 THEN 'recorded' ELSE '' END AS cost_status,
			s.project AS project, s.agent AS agent, s.machine AS machine,
			s.user_message_count AS user_message_count, s.is_automated AS is_automated,
			COALESCE(s.display_name, s.session_name, s.first_message, s.project) AS display_name,
			s.started_at AS started_at,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS activity_at
		FROM messages m
		JOIN sessions s ON s.id = m.session_id
		WHERE %s
			AND s.id IN %s
		UNION ALL
		SELECT ue.session_id AS session_id, ue.message_ordinal AS message_ordinal,
			ue.source AS source, COALESCE(ue.occurred_at, s.started_at) AS ts,
			ue.occurred_at AS pricing_ts,
			ue.model AS model, ue.provider_id AS provider_id, '' AS token_json,
			'' AS claude_message_id, '' AS claude_request_id,
			'' AS source_uuid,
			CASE
				WHEN ue.dedup_key != '' THEN ue.session_id || ':' || ue.source || ':' || ue.dedup_key
				ELSE ue.session_id || ':' || ue.source || ':id:' || ue.id
			END AS usage_dedup_key,
			ue.input_tokens AS input_tokens, ue.output_tokens AS output_tokens,
			ue.cache_creation_input_tokens AS cache_create,
			ue.cache_read_input_tokens AS cache_read,
			ue.reasoning_tokens AS reasoning_tokens,
			ue.cost_microdollars AS cost_microdollars,
			ue.cost_source AS cost_source,
			ue.cost_status AS cost_status,
			s.project AS project, s.agent AS agent, s.machine AS machine,
			s.user_message_count AS user_message_count, s.is_automated AS is_automated,
			COALESCE(s.display_name, s.session_name, s.first_message, s.project) AS display_name,
			s.started_at AS started_at,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS activity_at
		FROM usage_events ue
		JOIN sessions s ON s.id = ue.session_id
		WHERE %s
			AND s.id IN %s`,
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
		SELECT session_id, message_ordinal, ts, pricing_ts, source, model,
			provider_id, agent, claude_message_id, claude_request_id, source_uuid,
			usage_dedup_key, input_tokens_norm, output_tokens_norm,
			cache_create_norm, cache_create_1h_norm, cache_read_norm,
			reasoning_tokens_norm,
			web_search_requests_norm, cost_microdollars, cost_source, cost_status
		FROM usage_normalized`
	rows, err := s.queryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb session usage rows: %w", err)
	}
	defer rows.Close()
	var rowsAcc []duckSessionUsageOrderedRow
	for rows.Next() {
		var r duckActivityReportUsageRow
		var ts, pricingTS any
		if err := rows.Scan(
			&r.sessionID, &r.messageOrdinal, &ts, &pricingTS, &r.source, &r.model,
			&r.providerID, &r.agent, &r.claudeMessageID, &r.claudeRequestID, &r.sourceUUID,
			&r.usageDedupKey,
			&r.inputTok, &r.outputTok, &r.cacheCr, &r.cacheCr1h, &r.cacheRd,
			&r.reasoningTok, &r.webSearchRequests, &r.cost, &r.costSource, &r.costStatus,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb session usage rows: %w", err)
		}
		r.ts = formatDBTime(ts)
		r.pricingTS = formatDBTime(pricingTS)
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
	for i, o := range rowsAcc {
		snapshotRows[i] = activity.UsageRow{
			Model:               o.scan.model,
			SessionID:           o.scan.sessionID,
			Timestamp:           o.scan.ts,
			MessageOrdinal:      o.ordinal,
			UsageSource:         o.scan.source,
			InputTokens:         o.scan.inputTok,
			OutputTokens:        o.scan.outputTok,
			CacheCreationTokens: o.scan.cacheCr,
			CacheReadTokens:     o.scan.cacheRd,
			WebSearchRequests:   o.scan.webSearchRequests,
			Agent:               o.scan.agent,
			ProviderID:          o.scan.providerID,
			ClaudeMessageID:     o.scan.claudeMessageID,
			ClaudeRequestID:     o.scan.claudeRequestID,
			SourceUUID:          o.scan.sourceUUID,
			UsageDedupKey:       o.scan.usageDedupKey,
		}
		snapshotRows[i].Contributes = activity.UsageDataContributes(
			o.scan.cost != nil, o.scan.inputTok, o.scan.outputTok,
			o.scan.reasoningTok, o.scan.cacheCr, o.scan.cacheRd,
			o.scan.webSearchRequests)
	}
	return activity.SelectSessionUsageRows(ctx, snapshotRows, func(i int, webSearches int) (activity.UsagePrice, error) {
		r := rowsAcc[i].scan
		r.webSearchRequests = webSearches
		cost, costSource, priced, contributes, sessionCost, priceErr := duckActivityUsageCost(r, rateResolver)
		if priceErr != nil {
			return activity.UsagePrice{}, priceErr
		}
		var recorded *money.Money
		if r.cost != nil && r.costStatus == usagefacts.CostStatusRecorded {
			recorded = &money.Money{Microdollars: *r.cost}
		}
		return activity.UsagePrice{
			Cost: cost, CostSource: costSource, SessionCost: sessionCost,
			RecordedCost: recorded, Priced: priced, Contributes: contributes,
		}, nil
	})
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
	where, args := duckActivityReportCandidateWhere(
		f, rangeStartUTC, rangeEndUTC)

	query := `SELECT
		s.id,
		COALESCE(NULLIF(s.display_name, ''), NULLIF(s.session_name, ''), NULLIF(s.project, ''), s.id) AS display_name,
		s.project,
		s.agent,
		s.machine,
		s.started_at,
		s.ended_at,
		s.is_automated,
		s.relationship_type = 'subagent' AS is_subagent
	FROM sessions s
	WHERE ` + where

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
			&m.Machine, &startedAt, &endedAt, &m.IsAutomated, &m.IsSubagent,
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

func duckActivityReportCandidateWhere(
	f db.AnalyticsFilter, rangeStartUTC, rangeEndUTC string,
) (string, []any) {
	where, args := duckBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", false, false)
	where += `
		AND (COALESCE(s.ended_at,
				(SELECT MAX(m.timestamp) FROM messages m
					WHERE m.session_id = s.id AND m.timestamp IS NOT NULL),
				s.started_at, s.created_at) >= CAST(? AS TIMESTAMP)
			OR EXISTS (
				SELECT 1 FROM tool_result_events tre
				WHERE tre.session_id = s.id
					AND tre.source = 'tool_execution'
					AND tre.status IN ('completed', 'errored')
					AND tre.timestamp >= CAST(? AS TIMESTAMP)
			))
		AND COALESCE(s.started_at, s.created_at) < CAST(? AS TIMESTAMP)`
	return where, append(args, rangeStartUTC, rangeStartUTC, rangeEndUTC)
}

func (s *Store) activityReportCandidateSource(
	ids []string, q activity.Query,
) activity.CandidateSource {
	return func(
		ctx context.Context,
		yield func(activity.IntervalCandidate) error,
	) error {
		if len(ids) == 0 {
			return nil
		}
		lower := q.RangeStart.Add(
			-time.Duration(q.GapCapSeconds) * time.Second,
		)
		terminalQuery := `WITH terminal_events AS (
			SELECT tre.session_id, tre.tool_call_message_ordinal AS ordinal,
				tre.call_index, tre.event_index, tre.timestamp
			FROM tool_result_events tre
			WHERE tre.session_id IN (SELECT unnest(?))
				AND tre.source = 'tool_execution'
				AND tre.status IN ('completed', 'errored')
				AND tre.timestamp IS NOT NULL
				AND tre.timestamp >= CAST(? AS TIMESTAMP)
		), terminal_sessions AS (
			SELECT DISTINCT session_id FROM terminal_events
		), ordered_terminal AS (
			SELECT te.*,
				LEAD(te.ordinal) OVER terminal_order AS next_terminal_ordinal,
				LEAD(te.timestamp) OVER terminal_order AS next_terminal_timestamp
			FROM terminal_events te
			WINDOW terminal_order AS (
				PARTITION BY te.session_id
				ORDER BY te.timestamp, te.call_index, te.event_index
			)
		), terminal_with_message AS (
			SELECT ot.*, next_message.ordinal AS next_message_ordinal,
				next_message.timestamp AS next_message_timestamp,
				next_message.role AS next_message_role,
				next_message.model AS next_message_model
			FROM ordered_terminal ot
			LEFT JOIN LATERAL (
				SELECT next.ordinal, next.timestamp, next.role, next.model
				FROM messages next
				WHERE next.session_id = ot.session_id
					AND next.ordinal > ot.ordinal
					AND next.timestamp IS NOT NULL
					AND next.timestamp > ot.timestamp
				ORDER BY next.ordinal
				LIMIT 1
			) next_message ON TRUE
		), last_messages AS (
			SELECT latest.session_id, latest.ordinal, latest.timestamp
			FROM terminal_sessions ts
			JOIN LATERAL (
				SELECT m.session_id, m.ordinal, m.timestamp
				FROM messages m
				WHERE m.session_id = ts.session_id
					AND m.timestamp IS NOT NULL
				ORDER BY m.ordinal DESC
				LIMIT 1
			) latest ON TRUE
		), first_tail_events AS (
			SELECT lm.session_id, lm.ordinal, lm.timestamp,
				te.call_index, te.event_index, te.timestamp AS terminal_timestamp,
				ROW_NUMBER() OVER (
					PARTITION BY lm.session_id
					ORDER BY te.timestamp, te.call_index, te.event_index
				) AS row_num
			FROM last_messages lm
			JOIN terminal_events te ON te.session_id = lm.session_id
			WHERE te.timestamp > lm.timestamp
		), candidates AS (
			SELECT twm.session_id, twm.ordinal AS start_ordinal,
				CASE
					WHEN twm.next_terminal_timestamp IS NOT NULL AND
						(twm.next_message_timestamp IS NULL OR
						 twm.next_terminal_timestamp < twm.next_message_timestamp)
					THEN twm.next_terminal_ordinal
					ELSE twm.next_message_ordinal
				END AS end_ordinal,
				twm.timestamp AS start_timestamp,
				CASE
					WHEN twm.next_terminal_timestamp IS NOT NULL AND
						(twm.next_message_timestamp IS NULL OR
						 twm.next_terminal_timestamp < twm.next_message_timestamp)
					THEN twm.next_terminal_timestamp
					ELSE twm.next_message_timestamp
				END AS end_timestamp,
				CASE
					WHEN twm.next_terminal_timestamp IS NOT NULL AND
						(twm.next_message_timestamp IS NULL OR
						 twm.next_terminal_timestamp < twm.next_message_timestamp)
					THEN 'tool'
					ELSE twm.next_message_role
				END AS closing_role,
				CASE
					WHEN twm.next_terminal_timestamp IS NOT NULL AND
						(twm.next_message_timestamp IS NULL OR
						 twm.next_terminal_timestamp < twm.next_message_timestamp)
					THEN ''
					ELSE twm.next_message_model
				END AS closing_model,
				twm.call_index, twm.event_index
			FROM terminal_with_message twm

			UNION ALL

			SELECT fte.session_id, fte.ordinal, fte.ordinal,
				fte.timestamp, fte.terminal_timestamp, 'tool', '',
				fte.call_index, fte.event_index
			FROM first_tail_events fte
			WHERE fte.row_num = 1
		)
		SELECT candidate.session_id, candidate.start_ordinal,
			candidate.end_ordinal, candidate.start_timestamp,
			candidate.end_timestamp, candidate.closing_role,
			candidate.closing_model,
			COALESCE((
				SELECT prior.model
				FROM messages prior
				WHERE prior.session_id = candidate.session_id
					AND prior.ordinal <= candidate.start_ordinal
					AND prior.role = 'assistant'
					AND prior.model != ''
				ORDER BY prior.ordinal DESC
				LIMIT 1
			), 'unknown')
		FROM candidates candidate
		WHERE candidate.end_timestamp IS NOT NULL
			AND candidate.start_timestamp < CAST(? AS TIMESTAMP)
		ORDER BY candidate.start_timestamp, candidate.session_id,
			candidate.start_ordinal, candidate.call_index, candidate.event_index`
		scanCandidate := func(
			row interface{ Scan(dest ...any) error },
		) (activity.IntervalCandidate, error) {
			var candidate activity.IntervalCandidate
			var start, end any
			if err := row.Scan(
				&candidate.SessionID, &candidate.StartOrdinal,
				&candidate.EndOrdinal, &start, &end,
				&candidate.ClosingRole, &candidate.ClosingModel,
				&candidate.PriorModel,
			); err != nil {
				return candidate, fmt.Errorf(
					"scanning duckdb activity report candidate: %w", err)
			}
			startText, endText := formatDBTime(start), formatDBTime(end)
			var err error
			candidate.Start, err = time.Parse(time.RFC3339Nano, startText)
			if err != nil {
				return candidate, fmt.Errorf(
					"parsing duckdb activity candidate start: %w", err)
			}
			candidate.End, err = time.Parse(time.RFC3339Nano, endText)
			if err != nil {
				return candidate, fmt.Errorf(
					"parsing duckdb activity candidate end: %w", err)
			}
			candidate.Start = candidate.Start.UTC()
			candidate.End = candidate.End.UTC()
			return candidate, nil
		}

		terminalRows, err := s.queryContext(
			ctx, terminalQuery, ids,
			duckMicrosBound(lower),
			duckMicrosBound(q.EffectiveEnd),
		)
		if err != nil {
			return fmt.Errorf("querying duckdb activity report terminal candidates: %w", err)
		}
		defer terminalRows.Close()
		var terminal []activity.IntervalCandidate
		for terminalRows.Next() {
			candidate, scanErr := scanCandidate(terminalRows)
			if scanErr != nil {
				terminalRows.Close()
				return scanErr
			}
			terminal = append(terminal, candidate)
		}
		if err := terminalRows.Err(); err != nil {
			terminalRows.Close()
			return err
		}
		if err := terminalRows.Close(); err != nil {
			return err
		}

		messageSource := func(
			ctx context.Context,
			yield func(activity.IntervalCandidate) error,
		) error {
			// Every subquery reads the requested sessions only. DuckDB turns
			// the correlated subqueries into joins over their whole source,
			// so reading messages here made each report scan and join the
			// entire archive before the session filter applied.
			query := `WITH scoped AS (
				SELECT session_id, ordinal, timestamp, role, model
				FROM messages
				WHERE session_id IN (SELECT unnest(?))
			)
			SELECT
				m.session_id, m.ordinal, successor.ordinal,
				m.timestamp, successor.timestamp,
				successor.role, successor.model,
				COALESCE((
					SELECT prior.model
					FROM scoped prior
					WHERE prior.session_id = m.session_id
						AND prior.ordinal <= m.ordinal
						AND prior.role = 'assistant'
						AND prior.model != ''
						AND prior.timestamp IS NOT NULL
						AND prior.timestamp > (
							SELECT prior_previous.timestamp
							FROM scoped prior_previous
							WHERE prior_previous.session_id = prior.session_id
								AND prior_previous.ordinal < prior.ordinal
								AND prior_previous.timestamp IS NOT NULL
							ORDER BY prior_previous.ordinal DESC
							LIMIT 1
						)
					ORDER BY prior.ordinal DESC
					LIMIT 1
				), 'unknown')
			FROM scoped m
			JOIN scoped successor
				ON successor.session_id = m.session_id
				AND successor.ordinal = (
					SELECT next.ordinal
					FROM scoped next
					WHERE next.session_id = m.session_id
						AND next.ordinal > m.ordinal
						AND next.timestamp IS NOT NULL
					ORDER BY next.ordinal
					LIMIT 1
				)
			WHERE m.timestamp IS NOT NULL
				AND m.timestamp >= CAST(? AS TIMESTAMP)
				AND m.timestamp < CAST(? AS TIMESTAMP)
			ORDER BY m.timestamp, m.session_id, m.ordinal`
			rows, queryErr := s.queryContext(
				ctx, query, ids,
				duckMicrosBound(lower),
				duckMicrosBound(q.EffectiveEnd),
			)
			if queryErr != nil {
				return fmt.Errorf(
					"querying duckdb activity report candidates: %w", queryErr)
			}
			defer rows.Close()
			for rows.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				candidate, scanErr := scanCandidate(rows)
				if scanErr != nil {
					return scanErr
				}
				if err := yield(candidate); err != nil {
					return err
				}
			}
			return rows.Err()
		}
		return activity.MergeCandidateSlice(terminal, messageSource)(ctx, yield)
	}
}

// duckMicrosBound renders t rounded up to DuckDB's microsecond TIMESTAMP,
// so a comparison with it matches a comparison with t itself.
func duckMicrosBound(t time.Time) string {
	return time.UnixMicro(ceilMicros(t)).UTC().Format(time.RFC3339Nano)
}

// ActivityReportCandidateSource exposes the backend's mechanical pairing
// stream for cross-backend contract tests. Activity semantics remain in the
// shared aggregator. Reports derive runs in SQL instead (see
// activityReportRuns); this stream stays as the tests' oracle, and the runs
// must match its sweep exactly.
func (s *Store) ActivityReportCandidateSource(
	ids []string, q activity.Query,
) activity.CandidateSource {
	return s.activityReportCandidateSource(ids, q)
}

// duckActivityReportUsageRow is one scanned usage-union row before mapping
// into an activity.UsageRow, carrying the normalized token amounts and
// dedup keys the aggregator and per-row cost need.
type duckActivityReportUsageRow struct {
	sessionID         string
	source            string
	model             string
	providerID        string
	ts                string
	pricingTS         string
	messageOrdinal    any
	agent             string
	claudeMessageID   string
	claudeRequestID   string
	sourceUUID        string
	usageDedupKey     string
	inputTok          int
	outputTok         int
	cacheCr           int
	cacheCr1h         int
	cacheRd           int
	reasoningTok      int
	webSearchRequests int
	cost              *int64
	costSource        string
	costStatus        string
}

// activityReportUsage derives the candidate sessions and their Claude snapshot
// peers inside one DuckDB query. Snapshot key count therefore does not expand
// the SQL text or bind list. Cost is computed after selection so filtered rows
// do not affect pricing metadata.
func (s *Store) activityReportUsage(
	ctx context.Context,
	ids []string,
	f db.AnalyticsFilter,
	rangeStartUTC, rangeEndUTC string,
	lowerBound, upperBound string,
	q activity.Query,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	out := []activity.UsageRow{}

	rateResolver, err := s.loadPricingResolver(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("loading duckdb pricing: %w", err)
	}
	if len(ids) == 0 {
		block, err := rateResolver.BuildBlock()
		if err != nil {
			return nil, nil, fmt.Errorf("building pricing block: %w", err)
		}
		return out, &block, nil
	}

	candidateWhere, candidateArgs := duckActivityReportCandidateWhere(
		f, rangeStartUTC, rangeEndUTC)
	out, err = s.sumActivityUsage(ctx, candidateWhere, candidateArgs, lowerBound, upperBound, q, rateResolver)
	if err != nil {
		return nil, nil, fmt.Errorf("selecting duckdb activity report usage: %w", err)
	}
	block, err := rateResolver.BuildBlock()
	if err != nil {
		return nil, nil, fmt.Errorf("building pricing block: %w", err)
	}
	return out, &block, nil
}

// duckActivityUsageWith defines the relations sumActivityUsage reads over
// the padded time range: activity_usage_candidates, the report's candidate
// sessions, and activity_usage_rows, their usage rows normalized to the
// columns activityUsageSQL reads. Candidate sessions, their usage rows,
// and matching Claude snapshot peers are relations in one statement, so the
// query shape stays constant as the peer-key set grows. It applies the same
// message and usage-event eligibility predicates as GetDailyUsage and
// normalizes rows in SQL. Its placeholders take the lower and upper bounds,
// the candidate predicate's arguments, then
// activityUsageNullPricingUS.
func duckActivityUsageWith(candidateWhere string) string {
	return fmt.Sprintf(`
		report_bounds AS (
			SELECT CAST(? AS TIMESTAMP) AS lower_bound,
				CAST(? AS TIMESTAMP) AS upper_bound
		),
		candidate_sessions AS (
			SELECT s.id
			FROM sessions s
			WHERE %[2]s
		),
		candidate_messages AS (
			SELECT m.session_id AS session_id, m.ordinal AS message_ordinal,
				'message' AS source, COALESCE(m.timestamp, s.started_at) AS ts,
				m.timestamp AS pricing_ts,
				m.model AS model, m.provider_id AS provider_id, m.token_usage AS token_json,
				s.agent AS agent,
				m.claude_message_id AS claude_message_id,
				m.claude_request_id AS claude_request_id,
				m.source_uuid AS source_uuid,
				'' AS usage_dedup_key,
				0 AS input_tokens, 0 AS output_tokens,
					0 AS cache_create, 0 AS cache_read,
					COALESCE(TRY_CAST(json_extract_string(m.token_usage, '$.reasoning_tokens') AS BIGINT), 0) AS reasoning_tokens,
					CASE WHEN TRY_CAST(json_extract_string(m.token_usage, '$.recorded_cost_microdollars') AS BIGINT) >= 0 THEN TRY_CAST(json_extract_string(m.token_usage, '$.recorded_cost_microdollars') AS BIGINT) END AS cost_microdollars,
				'' AS cost_source,
				CASE WHEN TRY_CAST(json_extract_string(m.token_usage, '$.recorded_cost_microdollars') AS BIGINT) >= 0 THEN 'recorded' ELSE '' END AS cost_status
			FROM messages m
			JOIN sessions s ON s.id = m.session_id
			JOIN candidate_sessions candidate ON candidate.id = m.session_id
			CROSS JOIN report_bounds bounds
			WHERE m.token_usage != ''
				AND m.model != ''
				AND m.model != '<synthetic>'
				AND s.deleted_at IS NULL
				AND COALESCE(m.timestamp, s.started_at) >= bounds.lower_bound
				AND COALESCE(m.timestamp, s.started_at) <= bounds.upper_bound
		),
		candidate_events AS (
			SELECT ue.session_id AS session_id, ue.message_ordinal AS message_ordinal,
				ue.source AS source, COALESCE(ue.occurred_at, s.started_at) AS ts,
				ue.occurred_at AS pricing_ts,
				ue.model AS model, ue.provider_id AS provider_id, '' AS token_json,
				s.agent AS agent,
				'' AS claude_message_id, '' AS claude_request_id,
				'' AS source_uuid,
				CASE
					WHEN ue.dedup_key != '' THEN ue.session_id || ':' || ue.source || ':' || ue.dedup_key
					ELSE ue.session_id || ':' || ue.source || ':id:' || ue.id
				END AS usage_dedup_key,
					ue.input_tokens AS input_tokens, ue.output_tokens AS output_tokens,
					ue.cache_creation_input_tokens AS cache_create,
					ue.cache_read_input_tokens AS cache_read,
					ue.reasoning_tokens AS reasoning_tokens,
					ue.cost_microdollars AS cost_microdollars,
					ue.cost_source AS cost_source,
			ue.cost_status AS cost_status
			FROM usage_events ue
			JOIN sessions s ON s.id = ue.session_id
			JOIN candidate_sessions candidate ON candidate.id = ue.session_id
			CROSS JOIN report_bounds bounds
			WHERE ue.model != ''
				AND s.deleted_at IS NULL
				AND COALESCE(ue.occurred_at, s.started_at) >= bounds.lower_bound
				AND COALESCE(ue.occurred_at, s.started_at) <= bounds.upper_bound
		),
		peer_keys AS (
			SELECT DISTINCT claude_message_id, claude_request_id
			FROM candidate_messages
			WHERE claude_message_id != ''
		),
		peer_messages AS (
			SELECT m.session_id AS session_id, m.ordinal AS message_ordinal,
				'message' AS source, COALESCE(m.timestamp, s.started_at) AS ts,
				m.timestamp AS pricing_ts,
				m.model AS model, m.provider_id AS provider_id, m.token_usage AS token_json,
				s.agent AS agent,
				m.claude_message_id AS claude_message_id,
				m.claude_request_id AS claude_request_id,
				m.source_uuid AS source_uuid,
				'' AS usage_dedup_key,
				0 AS input_tokens, 0 AS output_tokens,
				0 AS cache_create, 0 AS cache_read,
				COALESCE(TRY_CAST(json_extract_string(m.token_usage, '$.reasoning_tokens') AS BIGINT), 0) AS reasoning_tokens,
				CASE WHEN TRY_CAST(json_extract_string(m.token_usage, '$.recorded_cost_microdollars') AS BIGINT) >= 0 THEN TRY_CAST(json_extract_string(m.token_usage, '$.recorded_cost_microdollars') AS BIGINT) END AS cost_microdollars,
				'' AS cost_source,
				CASE WHEN TRY_CAST(json_extract_string(m.token_usage, '$.recorded_cost_microdollars') AS BIGINT) >= 0 THEN 'recorded' ELSE '' END AS cost_status
			FROM messages m
			JOIN sessions s ON s.id = m.session_id
			JOIN peer_keys peer
				ON peer.claude_message_id = m.claude_message_id
				AND peer.claude_request_id = m.claude_request_id
			LEFT JOIN candidate_sessions candidate ON candidate.id = m.session_id
			CROSS JOIN report_bounds bounds
			WHERE candidate.id IS NULL
				AND m.token_usage != ''
				AND m.model != ''
				AND m.model != '<synthetic>'
				AND s.deleted_at IS NULL
				AND COALESCE(m.timestamp, s.started_at) >= bounds.lower_bound
				AND COALESCE(m.timestamp, s.started_at) <= bounds.upper_bound
		),
		usage_raw AS (
			SELECT * FROM candidate_messages
			UNION ALL
			SELECT * FROM candidate_events
			UNION ALL
			SELECT * FROM peer_messages
		),
		usage_normalized AS (
			SELECT session_id, message_ordinal, ts, pricing_ts, source, model, provider_id, agent,
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
					-- 1h-TTL subset of cache_create_norm from the nested
					-- Anthropic breakdown; only message rows carry it.
					CASE
						WHEN source = 'message' THEN LEAST(GREATEST(COALESCE(TRY_CAST(json_extract_string(token_json, '$.cache_creation.ephemeral_1h_input_tokens') AS BIGINT), 0), 0), %[1]d)
						ELSE 0
					END AS cache_create_1h_norm,
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
					cost_microdollars, cost_source, cost_status
			FROM usage_raw
		),
		activity_usage_candidates AS (
			SELECT id FROM candidate_sessions
		),
		activity_usage_rows AS (
			SELECT session_id,
				COALESCE(message_ordinal, -1) AS ord,
				CASE WHEN message_ordinal IS NULL THEN 0 ELSE 1 END AS has_ordinal,
				epoch_us(ts) AS ts_us,
				COALESCE(epoch_us(pricing_ts), CAST(? AS BIGINT)) AS pricing_us,
				source, model, provider_id, agent,
				claude_message_id, claude_request_id, source_uuid, usage_dedup_key,
				input_tokens_norm AS input, output_tokens_norm AS output,
				reasoning_tokens_norm AS reasoning,
				cache_create_norm AS cache_create, cache_create_1h_norm AS cache_create_1h,
				cache_read_norm AS cache_read, web_search_requests_norm AS web,
				CASE WHEN cost_status = '%[3]s' AND cost_source != '%[4]s'
					THEN NULL ELSE cost_microdollars END AS cost,
				cost_source,
				CASE WHEN cost_status = '%[3]s' AND cost_source != '%[4]s'
					THEN cost_microdollars END AS recorded_cost
			FROM usage_normalized
			WHERE ts IS NOT NULL
		)`,
		db.MaxPlausibleTokens, candidateWhere,
		usagefacts.CostStatusRecorded, db.CopilotReportedCostSource)
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
	canonicalModel := duckUsageLookupModel(r.model, r.pricingTS)
	var explicitCost int64
	var billableInput, billableOutput, billableReasoning, billableCacheCr, billableCacheCr1h, billableCacheRd int
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
		_, lookup, resolveErr := pricing.ResolveBilledAt(
			r.providerID, r.model, canonicalModel, duckUsagePricingTimestamp(r.pricingTS),
		)
		if resolveErr != nil {
			return money.Money{}, money.Money{}, false, false, resolveErr
		}
		priced = lookup.OK
		billableInput = r.inputTok
		billableOutput = r.outputTok
		billableReasoning = r.reasoningTok
		billableCacheCr = r.cacheCr
		billableCacheCr1h = r.cacheCr1h
		billableCacheRd = r.cacheRd
		billableWebSearch = r.webSearchRequests
	} else {
		priced = true
		billableInput = r.inputTok
		billableOutput = r.outputTok
		billableReasoning = r.reasoningTok
		billableCacheCr = r.cacheCr
		billableCacheCr1h = r.cacheCr1h
		billableCacheRd = r.cacheRd
	}
	cost, savings, _, _, err = duckUsageAggregateResolvedCost(
		r.model, canonicalModel, r.providerID, duckUsagePricingTimestamp(r.pricingTS),
		r.inputTok, r.outputTok, r.cacheCr, r.cacheCr1h, r.cacheRd,
		billableInput, billableOutput, billableReasoning,
		billableCacheCr, billableCacheCr1h, billableCacheRd, billableWebSearch,
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
	sessionCost *money.Money, err error,
) {
	costRow := r
	if r.costSource == db.CopilotReportedCostSource && r.cost != nil {
		v := money.Money{Microdollars: *r.cost}
		sessionCost = &v
		costRow.cost = nil
		pricing.RecordUnattributedReported()
	}
	if costRow.cost != nil {
		kept := db.ReportedCostForPricing(
			sql.NullInt64{Int64: *costRow.cost, Valid: true}, costRow.costStatus,
			func() bool {
				_, lookup := pricing.ResolveAt(
					costRow.model, duckUsageLookupModel(costRow.model, costRow.pricingTS),
					duckUsagePricingTimestamp(costRow.pricingTS))
				return lookup.OK
			})
		if !kept.Valid {
			costRow.cost = nil
		}
	}
	_, cost, priced, contributes, err = duckActivityReportRowStatus(costRow, pricing)
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
