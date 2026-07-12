package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
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
// mirrors the SQLite (*DB).GetActivityReport: three fetches scoped to the
// SAME candidate session-ID set so the concurrency timeline, sessions
// table, and usage totals stay mutually consistent (no orphan usage
// rows), then the in-memory streams are handed to activity.Aggregate.
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
	lowerBound := paddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
	upperBound := paddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)

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
	activity.SanitizeProjectLabels(&report, projects)
	report.Projects = export.ProjectMapForWire(projects)
	return report, nil
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
		AND COALESCE(s.ended_at,
			(SELECT MAX(m.timestamp) FROM messages m
				WHERE m.session_id = s.id AND m.timestamp IS NOT NULL),
			s.started_at, s.created_at) >= ` +
		lower + `::timestamptz
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

// activityReportActivity returns every timestamped message for the
// candidate sessions, ordered for the aggregator's per-session
// interval walk.
func (s *Store) activityReportActivity(
	ctx context.Context, ids []string,
) ([]activity.ActivityEvent, error) {
	var out []activity.ActivityEvent
	if len(ids) == 0 {
		return out, nil
	}
	err := pgQueryChunked(ids, func(chunk []string) error {
		pb := &paramBuilder{}
		ph := pgInPlaceholders(chunk, pb)
		query := `SELECT session_id, ordinal, role, timestamp, model
		FROM messages
		WHERE session_id IN ` + ph + `
			AND timestamp IS NOT NULL
		ORDER BY session_id, ordinal`

		rows, err := s.pg.QueryContext(ctx, query, pb.args...)
		if err != nil {
			return fmt.Errorf(
				"querying activity report activity: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var e activity.ActivityEvent
			var ts sql.NullTime
			if err := rows.Scan(
				&e.SessionID, &e.Ordinal, &e.Role, &ts, &e.Model,
			); err != nil {
				return fmt.Errorf(
					"scanning activity report activity: %w", err)
			}
			if !ts.Valid {
				continue
			}
			e.Timestamp = FormatISO8601(ts.Time)
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
// requires for its first-seen-wins dedup.
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

	// Accumulate the dedup sort keys (ts, session_id, ordinal) alongside
	// each mapped row so we can impose one global order across all chunks.
	// The same (claude_message_id, claude_request_id) can recur in
	// different sessions (resumed/forked) and thus different chunks, so
	// per-chunk ordering is not enough for the aggregator's first-seen dedup.
	type ordered struct {
		row     activity.UsageRow
		scan    pgDailyUsageScanRow
		ts      time.Time
		ordinal int64
	}
	var rowsAcc []ordered

	err = pgQueryChunked(ids, func(chunk []string) error {
		pb := &paramBuilder{}
		messagePH := pgInPlaceholders(chunk, pb)
		eventPH := pgInPlaceholders(chunk, pb)
		// Apply the same eligibility filters as GetDailyUsage so empty
		// token_usage, empty, and synthetic models are excluded from the
		// daily totals and dedup, keeping parity with the Usage dashboard.
		rowsSQL := pgDailyUsageRowsSQLWithWhere(
			pgUsageMessageEligibility+" AND m.session_id IN "+messagePH,
			pgUsageEventEligibility+" AND ue.session_id IN "+eventPH)
		lower := pb.add(lowerBound)
		upper := pb.add(upperBound)
		query := pgDailyUsageRowSelectFromRows(rowsSQL) + `
			AND u.ts >= ` + lower + `::timestamptz
			AND u.ts <= ` + upper + `::timestamptz`

		rows, err := s.pg.QueryContext(ctx, query, pb.args...)
		if err != nil {
			return fmt.Errorf("querying activity report usage: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			r, scanErr := scanPGDailyUsageRow(rows)
			if scanErr != nil {
				return fmt.Errorf(
					"scanning activity report usage: %w", scanErr)
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
		_, outputTok, _, _, cost, _ := pgDailyUsageAmounts(o.scan, rateResolver)
		row := o.row
		row.OutputTokens = outputTok
		row.Cost = cost
		out = append(out, row)
	}
	block, err := rateResolver.BuildBlock()
	if err != nil {
		return nil, nil, fmt.Errorf("building pricing block: %w", err)
	}
	return out, &block, nil
}
