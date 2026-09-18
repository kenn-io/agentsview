package clickhouse

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

var (
	_ db.ActivityReportArtifactStore = (*Store)(nil)
	_ db.ActivityReportProbeStore    = (*Store)(nil)
	_ db.ActivityReportTokenStore    = (*Store)(nil)
)

// activityReportRangeBoundsUTC returns the exact [start, end) UTC bounds
// of the resolved range `q` as RFC3339 strings. ClickHouse compares parsed
// instants, so the zone suffix stays, matching DuckDB and PostgreSQL.
func activityReportRangeBoundsUTC(q activity.Query) (string, string) {
	return q.RangeStart.UTC().Format(time.RFC3339),
		q.RangeEnd.UTC().Format(time.RFC3339)
}

// GetActivityReport assembles a concurrency- and usage-oriented report
// for the resolved range `q`, reading from the ClickHouse store. It mirrors
// the SQLite, PostgreSQL, and DuckDB backends: sessions and activity come
// from the filtered candidate set. Usage loads candidate rows plus only the
// cross-session Claude peers needed for complete-snapshot selection.
//
// Subagent and fork sessions are always counted so the cost totals match
// GetDailyUsage, which never filters by relationship_type.
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
	clickReportProgress(onProgress, activity.Progress{Phase: activity.ProgressLoadingSessions})
	f.IncludeSubagents = true
	f.IncludeForks = true
	rangeStartUTC, rangeEndUTC := activityReportRangeBoundsUTC(q)
	lowerBound := chUsagePaddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
	upperBound := chUsagePaddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)

	sessions, ids, err := s.activityReportSessions(
		ctx, f, rangeStartUTC, rangeEndUTC)
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}
	clickReportProgress(onProgress, activity.Progress{
		Phase: activity.ProgressLoadingUsage, SessionsTotal: len(sessions),
	})

	usage, pricing, err := s.activityReportUsage(
		ctx, ids, lowerBound, upperBound, q)
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
		clickReportProgress(onProgress, activity.Progress{
			Phase: activity.ProgressScanningActivity, SessionsTotal: len(sessions),
		})
		return source(ctx, func(candidate activity.IntervalCandidate) error {
			rowsProcessed++
			clickReportProgress(onProgress, activity.Progress{
				Phase:         activity.ProgressScanningActivity,
				SessionsTotal: len(sessions), RowsProcessed: rowsProcessed,
			})
			return yield(candidate)
		})
	}, usage)
	if err != nil {
		return activity.CandidateArtifacts{}, fmt.Errorf("aggregating clickhouse activity report: %w", err)
	}
	clickReportProgress(onProgress, activity.Progress{
		Phase: activity.ProgressFinalizing, SessionsTotal: len(sessions),
		SessionsProcessed: len(sessions), RowsProcessed: rowsProcessed,
	})
	artifacts.Report.SchemaVersion = export.ActivityReportSchemaVersion
	artifacts.Report.Pricing = pricing
	projects, err := s.BuildProjectIdentityMap(ctx, activityReportProjectLabels(sessions))
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}
	artifacts.Report.BySession = artifacts.Sessions
	activity.SanitizeProjectLabels(&artifacts.Report, projects)
	artifacts.Sessions = artifacts.Report.BySession
	artifacts.Report.BySession = []activity.SessionRow{}
	artifacts.Report.Projects = export.ProjectMapForWire(projects)
	clickReportProgress(onProgress, activity.Progress{
		Phase: activity.ProgressDone, SessionsTotal: len(sessions),
		SessionsProcessed: len(sessions), RowsProcessed: rowsProcessed,
	})
	return artifacts, nil
}

func clickReportProgress(callback activity.ProgressFunc, progress activity.Progress) {
	if callback != nil {
		callback(progress)
	}
}

func activityReportProjectLabels(sessions []activity.SessionMeta) []string {
	set := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		set[session.Project] = true
	}
	return sortedBoolKeys(set)
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *Store) activityReportSessions(
	ctx context.Context, f db.AnalyticsFilter, rangeStartUTC, rangeEndUTC string,
) ([]activity.SessionMeta, []string, error) {
	where, args := clickActivityReportCandidateWhere(
		f, rangeStartUTC, rangeEndUTC)

	query := `SELECT
		s.id,
		COALESCE(NULLIF(s.display_name, ''), NULLIF(s.session_name, ''), NULLIF(s.project, ''), s.id) AS display_name,
		s.project,
		s.agent,
		s.machine,
		s.started_at,
		s.ended_at,
		s.is_automated AS is_automated,
		s.relationship_type = 'subagent' AS is_subagent
	FROM sessions s
	WHERE ` + where

	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"querying clickhouse activity report sessions: %w", err)
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
				"scanning clickhouse activity report session: %w", err)
		}
		m.StartedAt = formatDBTime(startedAt)
		m.EndedAt = formatDBTime(endedAt)
		sessions = append(sessions, m)
		ids = append(ids, m.SessionID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf(
			"iterating clickhouse activity report sessions: %w", err)
	}
	return sessions, ids, nil
}

func clickActivityReportCandidateWhere(
	f db.AnalyticsFilter, rangeStartUTC, rangeEndUTC string,
) (string, []any) {
	where, args := chBuildAnalyticsWhere(
		f, "COALESCE(s.started_at, s.created_at)", "s.", false, false)
	// last_message_at is the push-time stand-in for the correlated MAX
	// subquery other backends use; ClickHouse does not evaluate those.
	where += `
		AND (COALESCE(s.ended_at, s.last_message_at, s.started_at, s.created_at) >= ` + chTimestampSQL + `
			OR EXISTS (
				SELECT 1 FROM tool_result_events tre
				WHERE tre.session_id = s.id
					AND tre.source = 'tool_execution'
					AND tre.status IN ('completed', 'errored')
					AND tre.timestamp >= ` + chTimestampSQL + `
			))
		AND COALESCE(s.started_at, s.created_at) < ` + chTimestampSQL
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
		inList, inArgs := chInPlaceholders(ids)
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
					"scanning clickhouse activity report candidate: %w", err)
			}
			startText, endText := formatDBTime(start), formatDBTime(end)
			var err error
			candidate.Start, err = time.Parse(time.RFC3339Nano, startText)
			if err != nil {
				return candidate, fmt.Errorf(
					"parsing clickhouse activity candidate start: %w", err)
			}
			candidate.End, err = time.Parse(time.RFC3339Nano, endText)
			if err != nil {
				return candidate, fmt.Errorf(
					"parsing clickhouse activity candidate end: %w", err)
			}
			candidate.Start = candidate.Start.UTC()
			candidate.End = candidate.End.UTC()
			return candidate, nil
		}

		terminalQuery := `
		WITH terminal_events AS (
			SELECT tre.session_id, tre.tool_call_message_ordinal AS ordinal,
				tre.call_index, tre.event_index, tre.timestamp
			FROM tool_result_events tre
			WHERE tre.session_id IN ` + inList + `
				AND tre.source = 'tool_execution'
				AND tre.status IN ('completed', 'errored')
				AND tre.timestamp IS NOT NULL
				AND tre.timestamp >= ` + chTimestampSQL + `
		), ordered_terminal AS (
			SELECT te.*,
				leadInFrame(CAST(te.ordinal AS Nullable(Int64)), 1, CAST(NULL AS Nullable(Int64))) OVER terminal_order AS next_terminal_ordinal,
				leadInFrame(CAST(te.timestamp AS Nullable(DateTime64(6, 'UTC'))), 1, CAST(NULL AS Nullable(DateTime64(6, 'UTC')))) OVER terminal_order AS next_terminal_timestamp
			FROM terminal_events te
			WINDOW terminal_order AS (
				PARTITION BY te.session_id
				ORDER BY te.timestamp, te.call_index, te.event_index
				ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING
			)
		), next_after_terminal AS (
			SELECT te.session_id AS session_id,
				te.ordinal AS terminal_ordinal,
				te.call_index AS call_index,
				te.event_index AS event_index,
				argMin(m.ordinal, m.ordinal) AS next_message_ordinal,
				argMin(m.timestamp, m.ordinal) AS next_message_timestamp,
				argMin(m.role, m.ordinal) AS next_message_role,
				argMin(m.model, m.ordinal) AS next_message_model
			FROM terminal_events te
			INNER JOIN messages m
				ON m.session_id = te.session_id
				AND m.ordinal > te.ordinal
				AND m.timestamp IS NOT NULL
				AND m.timestamp > te.timestamp
			GROUP BY te.session_id, te.ordinal, te.call_index, te.event_index
		), terminal_with_message AS (
			SELECT ot.*,
				nm.next_message_ordinal,
				nm.next_message_timestamp,
				nm.next_message_role,
				nm.next_message_model
			FROM ordered_terminal ot
			LEFT JOIN next_after_terminal nm
				ON nm.session_id = ot.session_id
				AND nm.terminal_ordinal = ot.ordinal
				AND nm.call_index = ot.call_index
				AND nm.event_index = ot.event_index
		), last_messages AS (
			SELECT session_id,
				argMax(ordinal, ordinal) AS last_ordinal,
				argMax(timestamp, ordinal) AS last_timestamp
			FROM messages
			WHERE session_id IN (SELECT DISTINCT session_id FROM terminal_events)
				AND timestamp IS NOT NULL
			GROUP BY session_id
		), first_tail_events AS (
			SELECT lm.session_id, lm.last_ordinal AS ordinal, lm.last_timestamp AS timestamp,
				te.call_index, te.event_index, te.timestamp AS terminal_timestamp,
				row_number() OVER (
					PARTITION BY lm.session_id
					ORDER BY te.timestamp, te.call_index, te.event_index
				) AS row_num
			FROM last_messages lm
			JOIN terminal_events te ON te.session_id = lm.session_id
			WHERE te.timestamp > lm.last_timestamp
		), candidates AS (
			SELECT twm.session_id, twm.ordinal AS start_ordinal,
				if(
					twm.next_terminal_timestamp IS NOT NULL AND
						(twm.next_message_timestamp IS NULL OR
						 twm.next_terminal_timestamp < twm.next_message_timestamp),
					twm.next_terminal_ordinal,
					twm.next_message_ordinal
				) AS end_ordinal,
				twm.timestamp AS start_timestamp,
				if(
					twm.next_terminal_timestamp IS NOT NULL AND
						(twm.next_message_timestamp IS NULL OR
						 twm.next_terminal_timestamp < twm.next_message_timestamp),
					twm.next_terminal_timestamp,
					twm.next_message_timestamp
				) AS end_timestamp,
				if(
					twm.next_terminal_timestamp IS NOT NULL AND
						(twm.next_message_timestamp IS NULL OR
						 twm.next_terminal_timestamp < twm.next_message_timestamp),
					'tool',
					twm.next_message_role
				) AS closing_role,
				if(
					twm.next_terminal_timestamp IS NOT NULL AND
						(twm.next_message_timestamp IS NULL OR
						 twm.next_terminal_timestamp < twm.next_message_timestamp),
					'',
					twm.next_message_model
				) AS closing_model,
				twm.call_index, twm.event_index
			FROM terminal_with_message twm

			UNION ALL

			SELECT fte.session_id, fte.ordinal, fte.ordinal,
				fte.timestamp, fte.terminal_timestamp, 'tool', '',
				fte.call_index, fte.event_index
			FROM first_tail_events fte
			WHERE fte.row_num = 1
		), assistant_models AS (
			SELECT session_id, ordinal, model
			FROM messages
			WHERE session_id IN ` + inList + `
				AND role = 'assistant'
				AND model != ''
		)
		SELECT candidate.session_id, candidate.start_ordinal,
			candidate.end_ordinal, candidate.start_timestamp,
			candidate.end_timestamp, candidate.closing_role,
			candidate.closing_model,
			ifNull(nullIf(am.model, ''), 'unknown')
		FROM candidates candidate
		ASOF LEFT JOIN assistant_models am
			ON candidate.session_id = am.session_id
			AND candidate.start_ordinal >= am.ordinal
		WHERE candidate.end_timestamp IS NOT NULL
			AND candidate.start_timestamp < ` + chTimestampSQL + `
		ORDER BY candidate.start_timestamp, candidate.session_id,
			candidate.start_ordinal, candidate.call_index, candidate.event_index`
		terminalArgs := append([]any{}, inArgs...)
		terminalArgs = append(terminalArgs, lower.UTC().Format(time.RFC3339Nano))
		terminalArgs = append(terminalArgs, inArgs...)
		terminalArgs = append(terminalArgs, q.EffectiveEnd.UTC().Format(time.RFC3339Nano))

		terminalRows, err := s.queryContext(ctx, terminalQuery, terminalArgs...)
		if err != nil {
			return fmt.Errorf("querying clickhouse activity report terminal candidates: %w", err)
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
			query := `
			WITH stamped AS (
				SELECT session_id, ordinal, timestamp, role, model,
					leadInFrame(CAST(ordinal AS Nullable(Int64)), 1, CAST(NULL AS Nullable(Int64))) OVER w AS successor_ordinal,
					leadInFrame(CAST(timestamp AS Nullable(DateTime64(6, 'UTC'))), 1, CAST(NULL AS Nullable(DateTime64(6, 'UTC')))) OVER w AS successor_timestamp,
					leadInFrame(CAST(role AS Nullable(String)), 1, CAST(NULL AS Nullable(String))) OVER w AS successor_role,
					leadInFrame(CAST(model AS Nullable(String)), 1, CAST(NULL AS Nullable(String))) OVER w AS successor_model,
					lagInFrame(CAST(timestamp AS Nullable(DateTime64(6, 'UTC'))), 1, CAST(NULL AS Nullable(DateTime64(6, 'UTC')))) OVER w AS prev_timestamp
				FROM messages
				WHERE session_id IN ` + inList + `
					AND timestamp IS NOT NULL
				WINDOW w AS (
					PARTITION BY session_id ORDER BY ordinal
					ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING
				)
			), prior_models AS (
				SELECT session_id, ordinal, model
				FROM stamped
				WHERE role = 'assistant'
					AND model != ''
					AND prev_timestamp IS NOT NULL
					AND timestamp > prev_timestamp
			)
			SELECT m.session_id, m.ordinal, m.successor_ordinal,
				m.timestamp, m.successor_timestamp,
				m.successor_role, m.successor_model,
				ifNull(nullIf(pm.model, ''), 'unknown')
			FROM stamped m
			ASOF LEFT JOIN prior_models pm
				ON m.session_id = pm.session_id AND m.ordinal >= pm.ordinal
			WHERE m.successor_ordinal IS NOT NULL
				AND m.timestamp >= ` + chTimestampSQL + `
				AND m.timestamp < ` + chTimestampSQL + `
			ORDER BY m.timestamp, m.session_id, m.ordinal`
			args := append([]any{}, inArgs...)
			args = append(args,
				lower.UTC().Format(time.RFC3339Nano),
				q.EffectiveEnd.UTC().Format(time.RFC3339Nano),
			)
			rows, queryErr := s.queryContext(ctx, query, args...)
			if queryErr != nil {
				return fmt.Errorf(
					"querying clickhouse activity report candidates: %w", queryErr)
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

type clickActivityReportUsageRow struct {
	sessionID         string
	source            string
	model             string
	providerID        string
	ts                string
	pricingTS         string
	messageOrdinal    sql.NullInt64
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
	cost              sql.NullInt64
	costSource        string
}

type clickSessionUsageOrderedRow struct {
	scan    clickActivityReportUsageRow
	ts      time.Time
	validTS bool
	ordinal int64
}

type clickSnapshotKey struct {
	messageID string
	requestID string
}

func (s *Store) GetSessionUsageRows(
	ctx context.Context, ids []string,
) (*activity.SessionUsageRows, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rateResolver, err := s.loadPricingResolver(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading clickhouse pricing: %w", err)
	}
	sessionOrder := make(map[string]int, len(ids))
	for i, id := range ids {
		sessionOrder[id] = i
	}
	inList, inArgs := chInPlaceholders(ids)
	query := clickUsageNormalizedQuery(
		chUsageMessageEligibility+" AND s.id IN "+inList,
		chUsageEventEligibility+" AND s.id IN "+inList,
	)
	queryArgs := append([]any{}, inArgs...)
	queryArgs = append(queryArgs, inArgs...)
	rowsAcc, err := s.scanActivityUsageRows(ctx, query, queryArgs)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rowsAcc, func(i, j int) bool {
		return clickSessionUsageRowLess(rowsAcc[i], rowsAcc[j], sessionOrder)
	})
	snapshotRows := make([]activity.UsageRow, len(rowsAcc))
	rowContributes := make([]bool, len(rowsAcc))
	rawOutputTokensBySession := make(map[string]int)
	for i, o := range rowsAcc {
		snapshotRows[i] = activity.UsageRow{
			SessionID:           o.scan.sessionID,
			Timestamp:           o.scan.ts,
			MessageOrdinal:      clickUsageOrdinalOrNeg(o.scan.messageOrdinal),
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
		rowContributes[i] = activity.UsageDataContributes(
			o.scan.cost.Valid, o.scan.inputTok, o.scan.outputTok,
			o.scan.reasoningTok, o.scan.cacheCr, o.scan.cacheRd,
			o.scan.webSearchRequests)
		rawOutputTokensBySession[o.scan.sessionID] += o.scan.outputTok
	}
	canonicalTokenCoverageBySession, err :=
		activity.CanonicalSessionTokenCoverageContext(ctx, snapshotRows)
	if err != nil {
		return nil, err
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
		if key, ok := clickSessionUsageDedupKey(r); ok {
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
			clickActivityUsageCost(r, rateResolver)
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
			ProviderID:      r.providerID,
			ClaudeMessageID: r.claudeMessageID,
			ClaudeRequestID: r.claudeRequestID,
			SourceUUID:      r.sourceUUID,
			UsageDedupKey:   r.usageDedupKey,

			UsageSource:         r.source,
			MessageOrdinal:      clickUsageOrdinalOrNeg(r.messageOrdinal),
			InputTokens:         r.inputTok,
			CacheCreationTokens: r.cacheCr,
			CacheReadTokens:     r.cacheRd,
			WebSearchRequests:   r.webSearchRequests,
		})
	}
	return &activity.SessionUsageRows{
		Rows:                            out,
		RawOutputTokensBySession:        rawOutputTokensBySession,
		DeduplicatedOutputTokens:        deduplicatedOutputTokens,
		DiscardedContributingSessions:   discardedContributingSessions,
		CanonicalTokenCoverageBySession: canonicalTokenCoverageBySession,
	}, nil
}

func (s *Store) activityReportUsage(
	ctx context.Context,
	ids []string,
	lowerBound, upperBound string,
	q activity.Query,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	out := []activity.UsageRow{}
	rateResolver, err := s.loadPricingResolver(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("loading clickhouse pricing: %w", err)
	}
	if len(ids) == 0 {
		block, err := rateResolver.BuildBlock()
		if err != nil {
			return nil, nil, fmt.Errorf("building pricing block: %w", err)
		}
		return out, &block, nil
	}

	inList, inArgs := chInPlaceholders(ids)
	messageBound := " AND COALESCE(m.timestamp, s.started_at) >= " + chTimestampSQL +
		" AND COALESCE(m.timestamp, s.started_at) <= " + chTimestampSQL
	eventBound := " AND COALESCE(ue.occurred_at, s.started_at) >= " + chTimestampSQL +
		" AND COALESCE(ue.occurred_at, s.started_at) <= " + chTimestampSQL
	query := clickUsageNormalizedQuery(
		chUsageMessageEligibility+" AND s.id IN "+inList+messageBound,
		chUsageEventEligibility+" AND s.id IN "+inList+eventBound,
	)
	args := append([]any{}, inArgs...)
	args = append(args, lowerBound, upperBound)
	args = append(args, inArgs...)
	args = append(args, lowerBound, upperBound)
	rowsAcc, err := s.scanActivityUsageRows(ctx, query, args)
	if err != nil {
		return nil, nil, err
	}

	type snapshotKey = clickSnapshotKey
	keySet := make(map[snapshotKey]struct{})
	for _, candidate := range rowsAcc {
		if candidate.scan.claudeMessageID == "" || candidate.scan.claudeRequestID == "" {
			continue
		}
		keySet[snapshotKey{
			messageID: candidate.scan.claudeMessageID,
			requestID: candidate.scan.claudeRequestID,
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
		pairSQL, pairArgs := clickPairPlaceholders(keys)
		peerQuery := clickUsageNormalizedQuery(
			chUsageMessageEligibility+`
				AND (m.claude_message_id, m.claude_request_id) IN `+pairSQL+`
				AND s.id NOT IN `+inList+messageBound,
			chUsageEventEligibility+" AND false",
		)
		peerArgs := append([]any{}, pairArgs...)
		peerArgs = append(peerArgs, inArgs...)
		peerArgs = append(peerArgs, lowerBound, upperBound)
		peerRows, peerErr := s.scanActivityUsageRows(ctx, peerQuery, peerArgs)
		if peerErr != nil {
			return nil, nil, peerErr
		}
		for _, row := range peerRows {
			if _, skip := candidateIDs[row.scan.sessionID]; skip {
				continue
			}
			rowsAcc = append(rowsAcc, row)
		}
	}

	sort.SliceStable(rowsAcc, func(i, j int) bool {
		a, b := rowsAcc[i], rowsAcc[j]
		if a.validTS && b.validTS && !a.ts.Equal(b.ts) {
			return a.ts.Before(b.ts)
		}
		if a.scan.sessionID != b.scan.sessionID {
			return a.scan.sessionID < b.scan.sessionID
		}
		return a.ordinal < b.ordinal
	})
	baseRows := make([]activity.UsageRow, len(rowsAcc))
	for i, o := range rowsAcc {
		baseRows[i] = activity.UsageRow{
			SessionID:         o.scan.sessionID,
			Model:             o.scan.model,
			Timestamp:         o.scan.ts,
			InputTokens:       o.scan.inputTok,
			OutputTokens:      o.scan.outputTok,
			WebSearchRequests: o.scan.webSearchRequests,
			Agent:             o.scan.agent,
			ClaudeMessageID:   o.scan.claudeMessageID,
			ClaudeRequestID:   o.scan.claudeRequestID,
			SourceUUID:        o.scan.sourceUUID,
			UsageDedupKey:     o.scan.usageDedupKey,
		}
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
			clickActivityUsageCost(costRow, rateResolver)
		if priceErr != nil {
			return nil, nil, priceErr
		}
		out = append(out, activity.UsageRow{
			SessionID:         attribution[i],
			Model:             o.scan.model,
			Timestamp:         o.scan.ts,
			InputTokens:       o.scan.inputTok,
			OutputTokens:      o.scan.outputTok,
			WebSearchRequests: webSearchRequests[i],
			Cost:              cost,
			CostSource:        costSource,
			SessionCost:       sessionCost,
			Priced:            priced,
			Contributes:       contributes,
			Agent:             o.scan.agent,
			ClaudeMessageID:   o.scan.claudeMessageID,
			ClaudeRequestID:   o.scan.claudeRequestID,
			SourceUUID:        o.scan.sourceUUID,
			UsageDedupKey:     o.scan.usageDedupKey,
		})
	}
	block, err := rateResolver.BuildBlock()
	if err != nil {
		return nil, nil, fmt.Errorf("building pricing block: %w", err)
	}
	return out, &block, nil
}

func clickUsageNormalizedQuery(messageWhere, eventWhere string) string {
	maxTok := db.MaxPlausibleTokens
	clamp := func(expr string) string {
		return fmt.Sprintf("least(greatest(%s, toInt64(0)), toInt64(%d))", expr, maxTok)
	}
	msgInput := clamp("JSONExtractInt(token_json, 'input_tokens')")
	msgOutput := clamp("JSONExtractInt(token_json, 'output_tokens')")
	msgCacheCr := clamp("JSONExtractInt(token_json, 'cache_creation_input_tokens')")
	msgCacheCr1h := clamp("JSONExtractInt(token_json, 'cache_creation', 'ephemeral_1h_input_tokens')")
	msgCacheRd := clamp("JSONExtractInt(token_json, 'cache_read_input_tokens')")
	msgReasoning := clamp("JSONExtractInt(token_json, 'reasoning_tokens')")
	msgWeb := "greatest(JSONExtractInt(token_json, 'server_tool_use', 'web_search_requests'), toInt64(0))"
	return fmt.Sprintf(`
		WITH usage_raw AS (
			SELECT m.session_id AS session_id,
				CAST(m.ordinal AS Nullable(Int64)) AS message_ordinal,
				'message' AS source,
				COALESCE(m.timestamp, s.started_at) AS ts,
				m.timestamp AS pricing_ts,
				m.model AS model, m.provider_id AS provider_id,
				m.token_usage AS token_json,
				s.agent AS agent,
				m.claude_message_id AS claude_message_id,
				m.claude_request_id AS claude_request_id,
				m.source_uuid AS source_uuid,
				CAST('' AS String) AS usage_dedup_key,
				toInt64(0) AS input_tokens, toInt64(0) AS output_tokens,
				toInt64(0) AS cache_create, toInt64(0) AS cache_read,
				toInt64(0) AS reasoning_tokens,
				CAST(NULL AS Nullable(Int64)) AS cost_microdollars,
				CAST('' AS String) AS cost_source,
				COALESCE(m.timestamp, s.started_at) AS ts_raw,
				s.started_at AS started_at_raw
			FROM messages m
			JOIN sessions s ON s.id = m.session_id
			WHERE %[1]s
			UNION ALL
			SELECT ue.session_id AS session_id,
				ue.message_ordinal AS message_ordinal,
				ue.source AS source,
				COALESCE(ue.occurred_at, s.started_at) AS ts,
				ue.occurred_at AS pricing_ts,
				ue.model AS model, ue.provider_id AS provider_id,
				CAST('' AS String) AS token_json,
				s.agent AS agent,
				CAST('' AS String) AS claude_message_id,
				CAST('' AS String) AS claude_request_id,
				CAST('' AS String) AS source_uuid,
				if(ue.dedup_key != '',
					concat(ue.session_id, ':', ue.source, ':', ue.dedup_key),
					concat(ue.session_id, ':', ue.source, ':id:', toString(ue.id))) AS usage_dedup_key,
				toInt64(ue.input_tokens) AS input_tokens,
				toInt64(ue.output_tokens) AS output_tokens,
				toInt64(ue.cache_creation_input_tokens) AS cache_create,
				toInt64(ue.cache_read_input_tokens) AS cache_read,
				toInt64(ue.reasoning_tokens) AS reasoning_tokens,
				ue.cost_microdollars AS cost_microdollars,
				ue.cost_source AS cost_source,
				COALESCE(ue.occurred_at, s.started_at) AS ts_raw,
				s.started_at AS started_at_raw
			FROM usage_events ue
			JOIN sessions s ON s.id = ue.session_id
			WHERE %[2]s
		)
		SELECT session_id, message_ordinal, ts, pricing_ts, source, model,
			provider_id, agent, claude_message_id, claude_request_id, source_uuid,
			usage_dedup_key,
			toInt64(CASE
				WHEN source = 'message' THEN %[3]s
				WHEN source = 'session' THEN greatest(input_tokens, toInt64(0))
				ELSE %[8]s
			END) AS input_tokens_norm,
			toInt64(CASE
				WHEN source = 'message' THEN %[4]s
				WHEN source = 'session' THEN greatest(output_tokens, toInt64(0))
				ELSE %[9]s
			END) AS output_tokens_norm,
			toInt64(CASE
				WHEN source = 'message' THEN %[5]s
				WHEN source = 'session' THEN greatest(cache_create, toInt64(0))
				ELSE %[10]s
			END) AS cache_create_norm,
			toInt64(CASE
				WHEN source = 'message' THEN %[6]s
				ELSE toInt64(0)
			END) AS cache_create_1h_norm,
			toInt64(CASE
				WHEN source = 'message' THEN %[7]s
				WHEN source = 'session' THEN greatest(cache_read, toInt64(0))
				ELSE %[11]s
			END) AS cache_read_norm,
			toInt64(CASE
				WHEN source = 'message' THEN %[12]s
				WHEN source = 'session' THEN greatest(reasoning_tokens, toInt64(0))
				ELSE %[13]s
			END) AS reasoning_tokens_norm,
			toInt64(CASE
				WHEN source = 'message' THEN %[14]s
				ELSE toInt64(0)
			END) AS web_search_requests_norm,
			cost_microdollars, cost_source
		FROM usage_raw`,
		messageWhere, eventWhere,
		msgInput, msgOutput, msgCacheCr, msgCacheCr1h, msgCacheRd,
		clamp("input_tokens"), clamp("output_tokens"), clamp("cache_create"),
		clamp("cache_read"), msgReasoning, clamp("reasoning_tokens"), msgWeb,
	)
}

func (s *Store) scanActivityUsageRows(
	ctx context.Context, query string, args []any,
) ([]clickSessionUsageOrderedRow, error) {
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse activity usage: %w", err)
	}
	defer rows.Close()
	var rowsAcc []clickSessionUsageOrderedRow
	for rows.Next() {
		var r clickActivityReportUsageRow
		var ts, pricingTS any
		if err := rows.Scan(
			&r.sessionID, &r.messageOrdinal, &ts, &pricingTS, &r.source, &r.model,
			&r.providerID, &r.agent, &r.claudeMessageID, &r.claudeRequestID, &r.sourceUUID,
			&r.usageDedupKey,
			&r.inputTok, &r.outputTok, &r.cacheCr, &r.cacheCr1h, &r.cacheRd,
			&r.reasoningTok, &r.webSearchRequests, &r.cost, &r.costSource,
		); err != nil {
			return nil, fmt.Errorf("scanning clickhouse activity usage: %w", err)
		}
		r.ts = formatDBTime(ts)
		r.pricingTS = formatDBTime(pricingTS)
		ordinal := int64(-1)
		if r.messageOrdinal.Valid {
			ordinal = r.messageOrdinal.Int64
		}
		parsedTS, ok := parseAnalyticsTime(r.ts)
		rowsAcc = append(rowsAcc, clickSessionUsageOrderedRow{
			scan:    r,
			ts:      parsedTS,
			validTS: ok,
			ordinal: ordinal,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse activity usage: %w", err)
	}
	return rowsAcc, nil
}

func clickUsageOrdinalOrNeg(v sql.NullInt64) int64 {
	if !v.Valid {
		return -1
	}
	return v.Int64
}

func clickSessionUsageDedupKey(r clickActivityReportUsageRow) (string, bool) {
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

func clickSessionUsageRowLess(
	a, b clickSessionUsageOrderedRow,
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

func clickActivityUsageCost(
	r clickActivityReportUsageRow, pricing *export.PricingResolver,
) (cost money.Money, costSource export.CostSource, priced, contributes bool,
	sessionCost *money.Money, err error) {
	costRow := r
	if r.costSource == db.CopilotReportedCostSource && r.cost.Valid {
		v := money.Money{Microdollars: r.cost.Int64}
		sessionCost = &v
		costRow.cost = sql.NullInt64{}
		pricing.RecordUnattributedReported()
	}
	cost, priced, contributes, err = clickActivityReportRowStatus(costRow, pricing)
	costSource = export.CostSourceComputed
	if costRow.cost.Valid {
		costSource = export.CostSourceReported
	}
	return
}

func clickActivityReportRowStatus(
	r clickActivityReportUsageRow, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	canonicalModel := chUsageLookupModel(r.model, r.pricingTS)
	pricedModel, lookup := pricing.ResolveAt(
		r.model, canonicalModel, chUsagePricingTimestamp(r.pricingTS),
	)
	if r.cost.Valid {
		pricing.RecordResolvedReported(r.model, pricedModel, lookup)
		return money.Money{Microdollars: r.cost.Int64}, true, true, nil
	}
	if !activity.UsageDataContributes(
		false, r.inputTok, r.outputTok, r.reasoningTok,
		r.cacheCr, r.cacheRd, r.webSearchRequests,
	) {
		return money.Money{}, true, false, nil
	}
	if !lookup.OK {
		pricing.RecordResolvedComputed(r.model, pricedModel, lookup)
		fee, feeErr := export.WebSearchFee(r.webSearchRequests)
		if feeErr != nil {
			return money.Money{}, false, false, feeErr
		}
		return fee, false, true, nil
	}
	pricedModel, lookup, err = pricing.ResolveBilledAt(
		r.providerID, r.model, canonicalModel, chUsagePricingTimestamp(r.pricingTS))
	if err != nil {
		return money.Money{}, false, false, err
	}
	requestScoped := db.UsageSourceIsRequestScoped(r.source) || r.messageOrdinal.Valid
	cost, err = lookup.Rates.CostForTokensScoped(
		requestScoped,
		r.inputTok, r.outputTok, r.reasoningTok, r.cacheCr, r.cacheCr1h, r.cacheRd)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing clickhouse activity usage for model %q: %w", r.model, err)
	}
	cost, err = export.AddWebSearchFee(cost, r.webSearchRequests)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing clickhouse activity usage for model %q: %w", r.model, err)
	}
	if requestScoped {
		pricing.RecordResolvedComputedRequest(
			r.model, pricedModel, lookup,
			r.inputTok, r.cacheCr, r.cacheRd)
	} else {
		pricing.RecordResolvedComputedAggregate(r.model, pricedModel, lookup)
	}
	return cost, true, true, nil
}

func clickPairPlaceholders(keys []clickSnapshotKey) (string, []any) {
	parts := make([]string, len(keys))
	args := make([]any, 0, len(keys)*2)
	for i, key := range keys {
		parts[i] = "(?, ?)"
		args = append(args, key.messageID, key.requestID)
	}
	return "(" + strings.Join(parts, ", ") + ")", args
}
