package db

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
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
// for the resolved range `q`. Sessions and activity are fetched from the
// filtered candidate set. Usage loads candidate rows plus only the
// cross-session Claude peers needed for complete-snapshot selection, keeping
// the resulting streams consistent without materializing the whole window.
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
	artifacts, err := db.BuildActivityReportArtifacts(ctx, f, q, nil)
	if err != nil {
		return activity.Report{}, err
	}
	artifacts.Report.BySession = artifacts.Sessions
	artifacts.Report.SessionsTotal = len(artifacts.Sessions)
	return artifacts.Report, nil
}

func (db *DB) BuildActivityReportArtifacts(
	ctx context.Context,
	f AnalyticsFilter,
	q activity.Query,
	onProgress activity.ProgressFunc,
) (activity.CandidateArtifacts, error) {
	reportProgress(onProgress, activity.Progress{Phase: activity.ProgressLoadingSessions})
	f.IncludeSubagents = true
	f.IncludeForks = true
	rangeStartUTC, rangeEndUTC := activityReportRangeBoundsUTC(q)
	lowerBound := paddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
	upperBound := paddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)

	sessions, ids, err := db.activityReportSessions(
		ctx, f, rangeStartUTC, rangeEndUTC)
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}
	reportProgress(onProgress, activity.Progress{
		Phase: activity.ProgressLoadingUsage, SessionsTotal: len(sessions),
	})

	usage, pricing, err := db.activityReportUsage(ctx, ids, lowerBound, upperBound, q)
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}

	rowsProcessed := int64(0)
	source := db.activityReportCandidateSource(ids, q)
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
		reportProgress(onProgress, activity.Progress{
			Phase: activity.ProgressScanningActivity, SessionsTotal: len(sessions),
		})
		return source(ctx, func(candidate activity.IntervalCandidate) error {
			rowsProcessed++
			reportProgress(onProgress, activity.Progress{
				Phase:         activity.ProgressScanningActivity,
				SessionsTotal: len(sessions), RowsProcessed: rowsProcessed,
			})
			return yield(candidate)
		})
	}, usage)
	if err != nil {
		return activity.CandidateArtifacts{}, fmt.Errorf("aggregating activity report: %w", err)
	}
	reportProgress(onProgress, activity.Progress{
		Phase: activity.ProgressFinalizing, SessionsTotal: len(sessions),
		SessionsProcessed: len(sessions), RowsProcessed: rowsProcessed,
	})
	artifacts.Report.SchemaVersion = export.ActivityReportSchemaVersion
	artifacts.Report.Pricing = pricing
	projects, err := db.BuildProjectIdentityMap(ctx,
		activityReportProjectLabels(sessions))
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}
	artifacts.Report.BySession = artifacts.Sessions
	activity.SanitizeProjectLabels(&artifacts.Report, projects)
	artifacts.Sessions = artifacts.Report.BySession
	artifacts.Report.BySession = []activity.SessionRow{}
	artifacts.Report.Projects = export.ProjectMapForWire(projects)
	reportProgress(onProgress, activity.Progress{
		Phase: activity.ProgressDone, SessionsTotal: len(sessions),
		SessionsProcessed: len(sessions), RowsProcessed: rowsProcessed,
	})
	return artifacts, nil
}

func reportProgress(callback activity.ProgressFunc, progress activity.Progress) {
	if callback != nil {
		callback(progress)
	}
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
) (*activity.SessionUsageRows, error) {
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
	snapshotRows := make([]activity.UsageRow, len(rowsAcc))
	rowContributes := make([]bool, len(rowsAcc))
	rawOutputTokensBySession := make(map[string]int)
	for i, o := range rowsAcc {
		inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok :=
			sqliteSessionUsageRowTokens(o.scan)
		snapshotRows[i] = activity.UsageRow{
			SessionID:      o.scan.sessionID,
			Timestamp:      o.scan.ts,
			MessageOrdinal: o.ordinal,
			OutputTokens:   outputTok,
			WebSearchRequests: usageRowWebSearchRequests(
				o.scan.usageSource, o.scan.tokenJSON),
			ClaudeMessageID: o.scan.claudeMessageID,
			ClaudeRequestID: o.scan.claudeRequestID,
		}
		rowContributes[i] = activity.UsageDataContributes(
			o.scan.cost.Valid, inputTok, outputTok, reasoningTok,
			cacheCrTok, cacheRdTok,
			usageRowWebSearchRequests(o.scan.usageSource, o.scan.tokenJSON))
		rawOutputTokensBySession[o.scan.sessionID] += outputTok
	}
	snapshotMask, snapshotAttribution, snapshotWebSearchRequests :=
		activity.ClaudeSnapshotSurvivorSelection(snapshotRows)
	seen := make(map[usageDedupToken]struct{})
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
			sqliteSessionUsageRowTokens(r)
		attributionSessionID := snapshotAttribution[i]
		if attributionSessionID != r.sessionID {
			deduplicatedOutputTokens[r.sessionID] += outputTok
			if rowContributes[i] {
				discardedContributingSessions[r.sessionID] = struct{}{}
			}
		}
		if key, ok := usageDedupTokenForRow(
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
		if r.costSource == CopilotReportedCostSource && r.cost.Valid {
			v := money.Money{Microdollars: r.cost.Int64}
			sessionCost = &v
			costRow.cost = sql.NullInt64{}
			rateResolver.RecordUnattributedReported()
		}
		cost, priced, contributes, priceErr :=
			sessionRowCostWithWebSearchRequests(
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

			UsageSource:         r.usageSource,
			MessageOrdinal:      usageRowMessageOrdinal(r.messageOrdinal),
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

// nullInt64Pointer converts a nullable message ordinal into the pointer
// shape SessionUsageBreakdownEntry and activity.UsageRow use.
func nullInt64Pointer(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	out := int(v.Int64)
	return &out
}

// usageRowMessageOrdinal renders a nullable message ordinal in
// activity.UsageRow's COALESCE(message_ordinal, -1) convention.
func usageRowMessageOrdinal(v sql.NullInt64) int64 {
	if !v.Valid {
		return -1
	}
	return v.Int64
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
	return !a.validTS && a.scan.ts < b.scan.ts
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
// A terminal tool event can outlive ended_at metadata, so it independently
// keeps the session eligible when it reaches the report range.
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
	args = append(args, rangeStartUTC, rangeStartUTC, rangeEndUTC)

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
		AND (COALESCE(NULLIF(s.ended_at, ''),
				(SELECT MAX(m.timestamp) FROM messages m
					WHERE m.session_id = s.id AND m.timestamp != ''),
				NULLIF(s.started_at, ''), s.created_at) >= ?
			OR EXISTS (
				SELECT 1 FROM tool_result_events tre
				WHERE tre.session_id = s.id
					AND tre.source = 'tool_execution'
					AND tre.status IN ('completed', 'errored')
					AND tre.timestamp IS NOT NULL
					AND tre.timestamp != ''
					AND agentsview_timestamp_unix_micro(tre.timestamp) IS NOT NULL
					AND tre.timestamp >= ?
			))
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

const activityReportTerminalCandidatesSQL = `WITH
	session_ids AS (
		SELECT value AS session_id FROM json_each(?)
	),
	terminal_events AS (
		SELECT tre.session_id, tre.tool_call_message_ordinal AS ordinal,
			tre.call_index, tre.event_index, tre.timestamp
		FROM tool_result_events tre
		JOIN session_ids ids ON ids.session_id = tre.session_id
		WHERE tre.source = 'tool_execution'
			AND tre.status IN ('completed', 'errored')
			AND tre.timestamp IS NOT NULL
			AND tre.timestamp != ''
			AND agentsview_timestamp_unix_micro(tre.timestamp) IS NOT NULL
			AND agentsview_timestamp_unix_micro(tre.timestamp) >= ?
	),
	terminal_sessions AS (
		SELECT DISTINCT session_id FROM terminal_events
	),
	ordered_terminal AS (
		SELECT te.*,
			LEAD(te.ordinal) OVER terminal_order AS next_terminal_ordinal,
			LEAD(te.timestamp) OVER terminal_order AS next_terminal_timestamp
		FROM terminal_events te
		WINDOW terminal_order AS (
			PARTITION BY te.session_id
			ORDER BY agentsview_timestamp_unix_micro(te.timestamp),
				te.call_index, te.event_index
		)
	),
	terminal_with_message AS (
		SELECT ot.*, next_message.ordinal AS next_message_ordinal,
			next_message.timestamp AS next_message_timestamp,
			next_message.role AS next_message_role,
			next_message.model AS next_message_model
		FROM ordered_terminal ot
		LEFT JOIN messages next_message ON next_message.id = (
			SELECT next.id
			FROM messages next INDEXED BY idx_messages_velocity
			WHERE next.session_id = ot.session_id
				AND next.ordinal > ot.ordinal
				AND next.timestamp IS NOT NULL
				AND next.timestamp != ''
				AND agentsview_timestamp_unix_micro(next.timestamp) >
					agentsview_timestamp_unix_micro(ot.timestamp)
			ORDER BY next.ordinal
			LIMIT 1
		)
	),
	last_messages AS (
		SELECT m.session_id, m.ordinal, m.timestamp
		FROM terminal_sessions ts
		JOIN messages m ON m.id = (
			SELECT latest.id
			FROM messages latest INDEXED BY idx_messages_velocity
			WHERE latest.session_id = ts.session_id
				AND latest.timestamp IS NOT NULL
				AND latest.timestamp != ''
				AND agentsview_timestamp_unix_micro(latest.timestamp) IS NOT NULL
			ORDER BY latest.ordinal DESC
			LIMIT 1
		)
	),
	first_tail_events AS (
		SELECT lm.session_id, lm.ordinal, lm.timestamp,
			te.call_index, te.event_index, te.timestamp AS terminal_timestamp,
			ROW_NUMBER() OVER (
				PARTITION BY lm.session_id
				ORDER BY agentsview_timestamp_unix_micro(te.timestamp),
					te.call_index, te.event_index
			) AS row_num
		FROM last_messages lm
		JOIN terminal_events te ON te.session_id = lm.session_id
		WHERE agentsview_timestamp_unix_micro(te.timestamp) >
				agentsview_timestamp_unix_micro(lm.timestamp)
	),
	candidates AS (
		SELECT twm.session_id, twm.ordinal AS start_ordinal,
			CASE
				WHEN twm.next_terminal_timestamp IS NOT NULL AND
					(twm.next_message_timestamp IS NULL OR
					 agentsview_timestamp_unix_micro(twm.next_terminal_timestamp) <
					 agentsview_timestamp_unix_micro(twm.next_message_timestamp))
				THEN twm.next_terminal_ordinal
				ELSE twm.next_message_ordinal
			END AS end_ordinal,
			twm.timestamp AS start_timestamp,
			CASE
				WHEN twm.next_terminal_timestamp IS NOT NULL AND
					(twm.next_message_timestamp IS NULL OR
					 agentsview_timestamp_unix_micro(twm.next_terminal_timestamp) <
					 agentsview_timestamp_unix_micro(twm.next_message_timestamp))
				THEN twm.next_terminal_timestamp
				ELSE twm.next_message_timestamp
			END AS end_timestamp,
			CASE
				WHEN twm.next_terminal_timestamp IS NOT NULL AND
					(twm.next_message_timestamp IS NULL OR
					 agentsview_timestamp_unix_micro(twm.next_terminal_timestamp) <
					 agentsview_timestamp_unix_micro(twm.next_message_timestamp))
				THEN 'tool'
				ELSE twm.next_message_role
			END AS closing_role,
			CASE
				WHEN twm.next_terminal_timestamp IS NOT NULL AND
					(twm.next_message_timestamp IS NULL OR
					 agentsview_timestamp_unix_micro(twm.next_terminal_timestamp) <
					 agentsview_timestamp_unix_micro(twm.next_message_timestamp))
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
SELECT candidate.session_id, candidate.start_ordinal, candidate.end_ordinal,
	candidate.start_timestamp, candidate.end_timestamp,
	candidate.closing_role, candidate.closing_model,
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
	AND agentsview_timestamp_unix_micro(candidate.start_timestamp) < ?
ORDER BY agentsview_timestamp_unix_micro(candidate.start_timestamp),
	candidate.session_id, candidate.start_ordinal,
	candidate.call_index, candidate.event_index`

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
		scanCandidate := func(
			row interface{ Scan(dest ...any) error },
		) (activity.IntervalCandidate, error) {
			var candidate activity.IntervalCandidate
			var start, end string
			if err := row.Scan(
				&candidate.SessionID, &candidate.StartOrdinal,
				&candidate.EndOrdinal, &start, &end,
				&candidate.ClosingRole, &candidate.ClosingModel,
				&candidate.PriorModel,
			); err != nil {
				return candidate, fmt.Errorf(
					"scanning activity report candidate: %w", err)
			}
			candidate.Start, err = time.Parse(time.RFC3339Nano, start)
			if err != nil {
				return candidate, fmt.Errorf(
					"parsing activity candidate start: %w", err)
			}
			candidate.End, err = time.Parse(time.RFC3339Nano, end)
			if err != nil {
				return candidate, fmt.Errorf(
					"parsing activity candidate end: %w", err)
			}
			candidate.Start = candidate.Start.UTC()
			candidate.End = candidate.End.UTC()
			return candidate, nil
		}

		terminalRows, err := db.getReader().QueryContext(
			ctx, activityReportTerminalCandidatesSQL,
			string(encodedIDs), lower, upper,
		)
		if err != nil {
			return fmt.Errorf("querying activity report terminal candidates: %w", err)
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
			rows, queryErr := db.getReader().QueryContext(
				ctx, activityReportCandidatesSQL,
				string(encodedIDs), paddedLower, paddedUpper, lower, upper,
			)
			if queryErr != nil {
				return fmt.Errorf(
					"querying activity report candidates: %w", queryErr)
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
func (db *DB) ActivityReportCandidateSource(
	ids []string, q activity.Query,
) activity.CandidateSource {
	return db.activityReportCandidateSource(ids, q)
}

// activityReportUsage selects complete snapshots across the padded range,
// then keeps rows attributed to the candidate sessions. Rows are ordered on
// parsed instants so mixed RFC3339 representations remain chronological.
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
		ctx, source, ids, lowerBound, upperBound, false,
	)
	if err != nil {
		return nil, nil, err
	}
	sortActivityReportUsageCandidates(candidates)
	baseRows := make([]activity.UsageRow, len(candidates))
	for i, candidate := range candidates {
		row := candidate.row
		_, row.OutputTokens, _, _, _ = dailyUsageRowTokens(candidate.scan)
		row.WebSearchRequests = usageRowWebSearchRequests(
			candidate.scan.usageSource, candidate.scan.tokenJSON)
		baseRows[i] = row
	}
	mask, attribution, webSearchRequests :=
		activity.UsageSurvivorSelectionForSessions(
			q.RangeStart, q.RangeEnd, q.EffectiveEnd, baseRows, ids,
		)
	return materializeActivityReportUsageCandidates(
		candidates, mask, attribution, webSearchRequests, rateResolver,
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
	validTS bool
	ordinal int64
}

func (db *DB) loadActivityReportUsageCandidatesFrom(
	ctx context.Context,
	source sessionExportQuerier,
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
func (db *DB) activityReportUsageCandidatesFrom(
	ctx context.Context,
	source sessionExportQuerier,
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
			return nil, nil, priceErr
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
	return sqliteActivityReportRowStatusWithWebSearchRequests(
		r, dailyUsageRowWebSearchRequests(r), pricing)
}

func sqliteActivityReportRowStatusWithWebSearchRequests(
	r dailyUsageScanRow, webSearches int, pricing *export.PricingResolver,
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
	requestScoped := usageRowIsRequestScoped(r.usageSource, r.messageOrdinal)
	cost, err = lookup.Rates.CostForTokensScoped(
		requestScoped,
		inTok, outTok, reasoningTok, crTok, rdTok)
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
