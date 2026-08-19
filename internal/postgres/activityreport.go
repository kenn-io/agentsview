package postgres

import (
	"context"
	"database/sql"
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
// DuckDB backends so the candidate-session predicate selects exactly the
// sessions whose window intersects the range, with no padding slop.
// PostgreSQL compares parsed instants (the bounds are cast to
// timestamptz), so it keeps the zone suffix, unlike SQLite's zone-less
// TEXT comparison.
func activityReportRangeBoundsUTC(q activity.Query) (string, string) {
	return q.RangeStart.UTC().Format(time.RFC3339),
		q.RangeEnd.UTC().Format(time.RFC3339)
}

// GetActivityReport assembles a concurrency- and usage-oriented report
// for the resolved range `q`, reading from the PostgreSQL store. It
// mirrors the SQLite (*DB).GetActivityReport: sessions and activity come from
// the filtered candidate set. Usage loads candidate rows plus only the
// cross-session Claude peers needed for complete-snapshot selection.
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
	artifacts.Report.BySession = artifacts.Sessions
	artifacts.Report.SessionsTotal = len(artifacts.Sessions)
	return artifacts.Report, nil
}

func (s *Store) BuildActivityReportArtifacts(
	ctx context.Context,
	f db.AnalyticsFilter,
	q activity.Query,
	onProgress activity.ProgressFunc,
) (activity.CandidateArtifacts, error) {
	pgReportProgress(onProgress, activity.Progress{Phase: activity.ProgressLoadingSessions})
	f.IncludeSubagents = true
	f.IncludeForks = true
	rangeStartUTC, rangeEndUTC := activityReportRangeBoundsUTC(q)
	lowerBound := paddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
	upperBound := paddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)

	sessions, ids, err := s.activityReportSessions(
		ctx, f, rangeStartUTC, rangeEndUTC)
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}
	pgReportProgress(onProgress, activity.Progress{
		Phase: activity.ProgressLoadingUsage, SessionsTotal: len(sessions),
	})

	usage, pricing, err := s.activityReportUsage(ctx, ids, lowerBound, upperBound, q)
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}

	rowsProcessed := int64(0)
	source := s.activityReportCandidateSource(ids, q)
	artifacts, err := activity.BuildCandidateArtifactsFromSourceWithSurvivorUsage(ctx, activity.Params{
		RangeStart:    q.RangeStart,
		RangeEnd:      q.RangeEnd,
		Loc:           q.Loc,
		EffectiveEnd:  q.EffectiveEnd,
		Partial:       q.Partial,
		GapCapSeconds: q.GapCapSeconds,
		Bucket:        q.Bucket,
	}, sessions, func(
		ctx context.Context, yield func(activity.IntervalCandidate) error,
	) error {
		pgReportProgress(onProgress, activity.Progress{
			Phase: activity.ProgressScanningActivity, SessionsTotal: len(sessions),
		})
		return source(ctx, func(candidate activity.IntervalCandidate) error {
			rowsProcessed++
			pgReportProgress(onProgress, activity.Progress{
				Phase:         activity.ProgressScanningActivity,
				SessionsTotal: len(sessions), RowsProcessed: rowsProcessed,
			})
			return yield(candidate)
		})
	}, usage)
	if err != nil {
		return activity.CandidateArtifacts{}, fmt.Errorf("aggregating pg activity report: %w", err)
	}
	pgReportProgress(onProgress, activity.Progress{
		Phase: activity.ProgressFinalizing, SessionsTotal: len(sessions),
		SessionsProcessed: len(sessions), RowsProcessed: rowsProcessed,
	})
	artifacts.Report.SchemaVersion = export.ActivityReportSchemaVersion
	artifacts.Report.Pricing = pricing
	projects, err := s.BuildProjectIdentityMap(ctx,
		activityReportProjectLabels(sessions))
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}
	artifacts.Report.BySession = artifacts.Sessions
	activity.SanitizeProjectLabels(&artifacts.Report, projects)
	artifacts.Sessions = artifacts.Report.BySession
	artifacts.Report.BySession = []activity.SessionRow{}
	artifacts.Report.Projects = export.ProjectMapForWire(projects)
	pgReportProgress(onProgress, activity.Progress{
		Phase: activity.ProgressDone, SessionsTotal: len(sessions),
		SessionsProcessed: len(sessions), RowsProcessed: rowsProcessed,
	})
	return artifacts, nil
}

func pgReportProgress(callback activity.ProgressFunc, progress activity.Progress) {
	if callback != nil {
		callback(progress)
	}
}

// GetSessionUsageRows returns the backend-priced usage rows for the supplied
// sessions, with the same cross-session deduplication as activity reports.
type pgSessionUsageOrderedRow struct {
	scan    pgUsageScanRow
	tsText  string
	ordinal int64
}

func (s *Store) GetSessionUsageRows(
	ctx context.Context, ids []string,
) (*activity.SessionUsageRows, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	pricing, err := s.loadPricingMap(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading pg pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)
	sessionOrder := make(map[string]int, len(ids))
	for i, id := range ids {
		sessionOrder[id] = i
	}
	var rowsAcc []pgSessionUsageOrderedRow
	err = pgQueryChunked(ids, func(chunk []string) error {
		pb := &paramBuilder{}
		ph := pgInPlaceholders(chunk, pb)
		query := pgUsageRowSelect() + " AND u.session_id IN " + ph
		rows, queryErr := s.pg.QueryContext(ctx, query, pb.args...)
		if queryErr != nil {
			return fmt.Errorf("querying pg session usage rows: %w", queryErr)
		}
		defer rows.Close()
		for rows.Next() {
			r, scanErr := scanPGUsageRow(rows)
			if scanErr != nil {
				return fmt.Errorf(
					"scanning pg session usage rows: %w", scanErr)
			}
			ordinal := int64(-1)
			if r.messageOrdinal.Valid {
				ordinal = r.messageOrdinal.Int64
			}
			rowsAcc = append(rowsAcc, pgSessionUsageOrderedRow{
				scan:    r,
				tsText:  startedAtString(r.ts),
				ordinal: ordinal,
			})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rowsAcc, func(i, j int) bool {
		return pgSessionUsageRowLess(rowsAcc[i], rowsAcc[j], sessionOrder)
	})
	snapshotRows := make([]activity.UsageRow, len(rowsAcc))
	rowContributes := make([]bool, len(rowsAcc))
	rawOutputTokensBySession := make(map[string]int)
	for i, o := range rowsAcc {
		inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok :=
			pgDailyUsageRowTokens(
				pgDailyUsageScanRow{
					messageOrdinal:           o.scan.messageOrdinal,
					usageSource:              o.scan.usageSource,
					tokenJSON:                o.scan.tokenJSON,
					inputTokens:              o.scan.inputTokens,
					outputTokens:             o.scan.outputTokens,
					cacheCreationInputTokens: o.scan.cacheCreationInputTokens,
					cacheReadInputTokens:     o.scan.cacheReadInputTokens,
					reasoningTokens:          o.scan.reasoningTokens,
				},
			)
		snapshotRows[i] = activity.UsageRow{
			SessionID:      o.scan.sessionID,
			Timestamp:      o.tsText,
			MessageOrdinal: o.ordinal,
			OutputTokens:   outputTok,
			WebSearchRequests: pgUsageRowWebSearchRequests(
				o.scan.usageSource, o.scan.tokenJSON),
			ClaudeMessageID: o.scan.claudeMessageID,
			ClaudeRequestID: o.scan.claudeRequestID,
		}
		rowContributes[i] = activity.UsageDataContributes(
			o.scan.cost.Valid, inputTok, outputTok, reasoningTok,
			cacheCrTok, cacheRdTok,
			pgUsageRowWebSearchRequests(o.scan.usageSource, o.scan.tokenJSON))
		rawOutputTokensBySession[o.scan.sessionID] += outputTok
	}
	snapshotMask, snapshotAttribution, snapshotWebSearchRequests :=
		activity.ClaudeSnapshotSurvivorSelection(snapshotRows)
	seen := make(map[pgUsageDedupToken]struct{})
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
		inputTok, outputTok, cacheCrTok, cacheRdTok, _ :=
			pgDailyUsageRowTokens(
				pgDailyUsageScanRow{
					messageOrdinal:           r.messageOrdinal,
					usageSource:              r.usageSource,
					tokenJSON:                r.tokenJSON,
					inputTokens:              r.inputTokens,
					outputTokens:             r.outputTokens,
					cacheCreationInputTokens: r.cacheCreationInputTokens,
					cacheReadInputTokens:     r.cacheReadInputTokens,
					reasoningTokens:          r.reasoningTokens,
				},
			)
		attributionSessionID := snapshotAttribution[i]
		if attributionSessionID != r.sessionID {
			deduplicatedOutputTokens[r.sessionID] += outputTok
			if rowContributes[i] {
				discardedContributingSessions[r.sessionID] = struct{}{}
			}
		}
		if key, ok := pgUsageDedupTokenForRow(
			r.usageSource, r.agent, r.claudeMessageID,
			r.claudeRequestID, r.sourceUUID, r.usageDedupKey,
		); ok {
			if _, dup := seen[key]; dup {
				deduplicatedOutputTokens[r.sessionID] += outputTok
				if rowContributes[i] {
					discardedContributingSessions[r.sessionID] = struct{}{}
				}
				continue
			}
			seen[key] = struct{}{}
		}
		costRow := r
		var sessionCost *money.Money
		if r.costSource == db.CopilotReportedCostSource && r.cost.Valid {
			v := money.Money{Microdollars: r.cost.Int64}
			sessionCost = &v
			costRow.cost = sql.NullInt64{}
			rateResolver.RecordUnattributedReported()
		}
		cost, priced, contributes, priceErr :=
			pgSessionRowCostWithWebSearchRequests(
				costRow, snapshotWebSearchRequests[i], rateResolver)
		if priceErr != nil {
			return nil, priceErr
		}
		costSource := export.CostSourceComputed
		if costRow.cost.Valid {
			costSource = export.CostSourceReported
		}
		out = append(out, activity.UsageRow{
			SessionID:       attributionSessionID,
			SourceSessionID: r.sessionID,
			Model:           r.model,
			Timestamp:       o.tsText,
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

			UsageSource:         r.usageSource,
			MessageOrdinal:      pgUsageRowMessageOrdinal(r.messageOrdinal),
			InputTokens:         inputTok,
			CacheCreationTokens: cacheCrTok,
			CacheReadTokens:     cacheRdTok,
			WebSearchRequests:   snapshotWebSearchRequests[i],
		})
	}
	return &activity.SessionUsageRows{
		Rows:                          out,
		RawOutputTokensBySession:      rawOutputTokensBySession,
		DeduplicatedOutputTokens:      deduplicatedOutputTokens,
		DiscardedContributingSessions: discardedContributingSessions,
	}, nil
}

// pgNullInt64Pointer converts a nullable message ordinal into the pointer
// shape SessionUsageBreakdownEntry uses.
func pgNullInt64Pointer(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	out := int(v.Int64)
	return &out
}

// pgUsageRowMessageOrdinal renders a nullable message ordinal in
// activity.UsageRow's COALESCE(message_ordinal, -1) convention.
func pgUsageRowMessageOrdinal(v sql.NullInt64) int64 {
	if !v.Valid {
		return -1
	}
	return v.Int64
}

func pgSessionUsageRowLess(
	a, b pgSessionUsageOrderedRow,
	sessionOrder map[string]int,
) bool {
	if a.scan.ts.Valid && b.scan.ts.Valid {
		if !a.scan.ts.Time.Equal(b.scan.ts.Time) {
			return a.scan.ts.Time.Before(b.scan.ts.Time)
		}
	} else if a.scan.ts.Valid != b.scan.ts.Valid {
		return a.scan.ts.Valid
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
	if a.scan.usageDedupKey != b.scan.usageDedupKey {
		return a.scan.usageDedupKey < b.scan.usageDedupKey
	}
	return !a.scan.ts.Valid && a.tsText < b.tsText
}

func activityReportProjectLabels(sessions []activity.SessionMeta) []string {
	set := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		set[session.Project] = struct{}{}
	}
	return sortedStringSetKeys(set)
}

// activityReportSessions returns the candidate sessions whose window
// overlaps the exact range [rangeStartUTC, rangeEndUTC), plus their
// IDs. The ID set defines the scope for the activity and usage fetches.
// Titles intentionally exclude first_message because activity reports cross
// the summary export boundary.
//
// The effective-end fallback for a session with no ended_at uses its
// latest message timestamp before started_at, so a still-open session
// that began before the range but has messages inside it is not dropped,
// matching SQLite and DuckDB. COALESCE short-circuits, so the correlated
// MAX subquery runs only for the rare sessions missing an ended_at.
// A terminal tool event can outlive ended_at metadata, so it independently
// keeps the session eligible when it reaches the report range.
func (s *Store) activityReportSessions(
	ctx context.Context, f db.AnalyticsFilter, rangeStartUTC, rangeEndUTC string,
) ([]activity.SessionMeta, []string, error) {
	pb := &paramBuilder{}
	where := buildAnalyticsWhereWithDate(f, "", pb, false, "s.id")
	lower := pb.add(rangeStartUTC)
	upper := pb.add(rangeEndUTC)

	// Each Title candidate is NULLIF'd independently (not a nested
	// COALESCE-then-NULLIF) so an empty display_name cannot mask a real
	// session_name.
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
		AND (COALESCE(s.ended_at,
				(SELECT MAX(m.timestamp) FROM messages m
					WHERE m.session_id = s.id AND m.timestamp IS NOT NULL),
				s.started_at, s.created_at) >= ` +
		lower + `::timestamptz
			OR EXISTS (
				SELECT 1 FROM tool_result_events tre
				WHERE tre.session_id = s.id
					AND tre.source = 'tool_execution'
					AND tre.status IN ('completed', 'errored')
					AND tre.timestamp >= ` + lower + `::timestamptz
			))
		AND COALESCE(s.started_at, s.created_at) < ` +
		upper + `::timestamptz`

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"querying activity report sessions: %w", err)
	}
	defer rows.Close()

	var sessions []activity.SessionMeta
	var ids []string
	for rows.Next() {
		var m activity.SessionMeta
		var startedAt, endedAt sql.NullTime
		if err := rows.Scan(
			&m.SessionID, &m.Title, &m.Project, &m.Agent,
			&m.Machine, &startedAt, &endedAt, &m.IsAutomated,
		); err != nil {
			return nil, nil, fmt.Errorf(
				"scanning activity report session: %w", err)
		}
		m.StartedAt = startedAtString(startedAt)
		m.EndedAt = startedAtString(endedAt)
		sessions = append(sessions, m)
		ids = append(ids, m.SessionID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf(
			"iterating activity report sessions: %w", err)
	}
	return sessions, ids, nil
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
			WHERE tre.session_id = ANY($1)
				AND tre.source = 'tool_execution'
				AND tre.status IN ('completed', 'errored')
				AND tre.timestamp IS NOT NULL
				AND tre.timestamp >= $2
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
			AND candidate.start_timestamp < $3
		ORDER BY candidate.start_timestamp, candidate.session_id,
			candidate.start_ordinal, candidate.call_index, candidate.event_index`
		scanCandidate := func(
			row interface{ Scan(dest ...any) error },
		) (activity.IntervalCandidate, error) {
			var candidate activity.IntervalCandidate
			if err := row.Scan(
				&candidate.SessionID, &candidate.StartOrdinal,
				&candidate.EndOrdinal, &candidate.Start, &candidate.End,
				&candidate.ClosingRole, &candidate.ClosingModel,
				&candidate.PriorModel,
			); err != nil {
				return candidate, fmt.Errorf(
					"scanning pg activity report candidate: %w", err)
			}
			candidate.Start = candidate.Start.UTC()
			candidate.End = candidate.End.UTC()
			return candidate, nil
		}

		terminalRows, err := s.pg.QueryContext(
			ctx, terminalQuery, ids, lower, q.EffectiveEnd,
		)
		if err != nil {
			return fmt.Errorf("querying pg activity report terminal candidates: %w", err)
		}
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
			query := `SELECT
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
						AND prior.timestamp > (
							SELECT prior_previous.timestamp
							FROM messages prior_previous
							WHERE prior_previous.session_id = prior.session_id
								AND prior_previous.ordinal < prior.ordinal
								AND prior_previous.timestamp IS NOT NULL
							ORDER BY prior_previous.ordinal DESC
							LIMIT 1
						)
					ORDER BY prior.ordinal DESC
					LIMIT 1
				), 'unknown')
			FROM messages m
			JOIN messages successor
				ON successor.session_id = m.session_id
				AND successor.ordinal = (
					SELECT next.ordinal
					FROM messages next
					WHERE next.session_id = m.session_id
						AND next.ordinal > m.ordinal
						AND next.timestamp IS NOT NULL
					ORDER BY next.ordinal
					LIMIT 1
				)
			WHERE m.session_id = ANY($1)
				AND m.timestamp IS NOT NULL
				AND m.timestamp >= $2
				AND m.timestamp < $3
			ORDER BY m.timestamp, m.session_id, m.ordinal`
			rows, queryErr := s.pg.QueryContext(
				ctx, query, ids, lower, q.EffectiveEnd,
			)
			if queryErr != nil {
				return fmt.Errorf(
					"querying pg activity report candidates: %w", queryErr)
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

// ActivityReportCandidateSource exposes the backend's mechanical pairing
// stream for cross-backend contract tests. Activity semantics remain in the
// shared aggregator.
func (s *Store) ActivityReportCandidateSource(
	ids []string, q activity.Query,
) activity.CandidateSource {
	return s.activityReportCandidateSource(ids, q)
}

// activityReportUsage selects complete snapshots across the padded range,
// then keeps rows attributed to the candidate sessions. Per-row cost is
// computed after selection so filtered rows do not affect pricing metadata.
func (s *Store) activityReportUsage(
	ctx context.Context, ids []string, lowerBound, upperBound string, q activity.Query,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	out := []activity.UsageRow{}

	pricing, err := s.loadPricingMap(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("loading pg pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)
	if len(ids) == 0 {
		block, err := rateResolver.BuildBlock()
		if err != nil {
			return nil, nil, fmt.Errorf("building pricing block: %w", err)
		}
		return out, &block, nil
	}

	type ordered struct {
		row     activity.UsageRow
		scan    pgDailyUsageScanRow
		ts      time.Time
		ordinal int64
	}
	var rowsAcc []ordered
	loadRows := func(
		rowsSQL string,
		pb *paramBuilder,
		skipSessionIDs map[string]struct{},
	) error {
		lower := pb.add(lowerBound)
		upper := pb.add(upperBound)
		query := pgDailyUsageRowSelectFromRows(rowsSQL) + `
			AND u.ts >= ` + lower + `::timestamptz
			AND u.ts <= ` + upper + `::timestamptz`
		rows, queryErr := s.pg.QueryContext(ctx, query, pb.args...)
		if queryErr != nil {
			return fmt.Errorf("querying activity report usage: %w", queryErr)
		}
		defer rows.Close()
		for rows.Next() {
			r, scanErr := scanPGDailyUsageRow(rows)
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
			rowsAcc = append(rowsAcc, ordered{
				ts:      r.ts.Time,
				ordinal: ord,
				scan:    r,
				row: activity.UsageRow{
					SessionID:       r.sessionID,
					Model:           r.model,
					Timestamp:       startedAtString(r.ts),
					Agent:           r.agent,
					ClaudeMessageID: r.claudeMessageID,
					ClaudeRequestID: r.claudeRequestID,
					SourceUUID:      r.sourceUUID,
					UsageDedupKey:   r.usageDedupKey,
				},
			})
		}
		return rows.Err()
	}

	err = pgQueryChunked(ids, func(chunk []string) error {
		pb := &paramBuilder{}
		ph := pgInPlaceholders(chunk, pb)
		rowsSQL := pgDailyUsageRowsSQLWithWhere(
			pgUsageMessageEligibility+" AND m.session_id IN "+ph,
			pgUsageEventEligibility+" AND ue.session_id IN "+ph)
		return loadRows(rowsSQL, pb, nil)
	})
	if err != nil {
		return nil, nil, err
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
	const peerChunk = (maxPGVars - 2) / 2
	for i := 0; i < len(keys); i += peerChunk {
		end := min(i+peerChunk, len(keys))
		pb := &paramBuilder{}
		pairs := make([]string, 0, end-i)
		for _, key := range keys[i:end] {
			pairs = append(pairs,
				"("+pb.add(key.messageID)+", "+pb.add(key.requestID)+")")
		}
		rowsSQL := pgDailyUsageRowsSQLWithWhere(
			pgUsageMessageEligibility+` AND EXISTS (
				SELECT 1
				FROM (VALUES `+strings.Join(pairs, ", ")+`) AS peer_keys(message_id, request_id)
				WHERE peer_keys.message_id = m.claude_message_id
				  AND peer_keys.request_id = m.claude_request_id
			)`,
			pgUsageEventEligibility+" AND FALSE")
		if err := loadRows(rowsSQL, pb, candidateIDs); err != nil {
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
		row := o.row
		_, row.OutputTokens, _, _, _ = pgDailyUsageRowTokens(o.scan)
		row.WebSearchRequests = pgUsageRowWebSearchRequests(
			o.scan.usageSource, o.scan.tokenJSON)
		baseRows[i] = row
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
		_, outputTok, _, _, _ := pgDailyUsageRowTokens(o.scan)
		costRow := o.scan
		var sessionCost *money.Money
		if o.scan.costSource == db.CopilotReportedCostSource && o.scan.cost.Valid {
			v := money.Money{Microdollars: o.scan.cost.Int64}
			sessionCost = &v
			costRow.cost = sql.NullInt64{}
			rateResolver.RecordUnattributedReported()
		}
		cost, priced, contributes, priceErr :=
			pgActivityReportRowStatusWithWebSearchRequests(
				costRow, webSearchRequests[i], rateResolver)
		if priceErr != nil {
			return nil, nil, priceErr
		}
		costSource := export.CostSourceComputed
		if costRow.cost.Valid {
			costSource = export.CostSourceReported
		}
		row := o.row
		row.SessionID = attribution[i]
		row.OutputTokens = outputTok
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

func pgActivityReportRowStatus(
	r pgDailyUsageScanRow, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	return pgActivityReportRowStatusWithWebSearchRequests(
		r, pgDailyUsageRowWebSearchRequests(r), pricing)
}

func pgActivityReportRowStatusWithWebSearchRequests(
	r pgDailyUsageScanRow, webSearches int, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	pricedModel, lookup := pricing.Resolve(
		r.model, pgUsageLookupModel(r.model, r.ts))
	inTok, outTok, crTok, rdTok, reasoningTok := pgDailyUsageRowTokens(r)
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
	requestScoped := pgUsageRowIsRequestScoped(r.usageSource, r.messageOrdinal)
	cost, err = lookup.Rates.CostForTokensScoped(
		requestScoped,
		inTok, outTok, reasoningTok, crTok, rdTok)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing pg activity usage for model %q: %w", r.model, err)
	}
	cost, err = export.AddWebSearchFee(cost, webSearches)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing pg activity usage for model %q: %w", r.model, err)
	}
	pgRecordComputedUsagePricing(
		pricing, r.model, pricedModel, lookup,
		requestScoped, inTok, crTok, rdTok)
	return cost, true, true, nil
}
