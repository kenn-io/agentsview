package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/usagefacts"
	"golang.org/x/sync/errgroup"
)

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
	return activity.ReportWithSessions(artifacts), nil
}

type activityReportBackend struct {
	*Store
	lowerBound, upperBound string
	read                   map[string]struct{}
	pricing                []export.EffectivePricingRow
	pricingDigest          string
	inputs                 *pgActivityInputs
	counted                *[]pgActivityCountedMessage
}

func (b activityReportBackend) ActivityReportSecondRead() (func(), bool) {
	return func() {}, b.supportsConcurrentReads()
}

func (b activityReportBackend) ActivityReportUsage(
	ctx context.Context, ids []string, q activity.Query,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	return b.activityReportUsage(ctx, ids, b.lowerBound, b.upperBound, q, b.read, b.pricing, b.pricingDigest, b.inputs)
}

func (b activityReportBackend) ActivityReportCandidateSource(ids []string, q activity.Query) activity.CandidateSource {
	return b.activityReportCandidateSourceWithCounts(ids, q, b.counted)
}

// activityReportRunBackend sweeps the runs kept in inputs.
type activityReportRunBackend struct {
	activityReportBackend
}

func (b activityReportRunBackend) ActivityReportRunSource(ids []string, q activity.Query) activity.RunSource {
	return func(ctx context.Context, yield func(activity.Run) error) error {
		runs, err := b.activityReportRuns(ctx, ids, q, b.inputs)
		if err != nil {
			return err
		}
		// The consumer checks for cancellation before each run.
		for _, run := range runs {
			if err := yield(run); err != nil {
				return err
			}
		}
		return nil
	}
}

// ActivityReportMessageCounts counts the messages the kept sessions hold,
// before the effective end at PostgreSQL's microsecond precision.
func (b activityReportRunBackend) ActivityReportMessageCounts(
	ctx context.Context, ids []string, q activity.Query, artifacts *activity.CandidateArtifacts,
) error {
	counts := activity.NewMessageAccumulator(q, artifacts)
	upper := q.EffectiveEnd.UnixMicro()
	before := func(micros []int64) []int64 {
		i, _ := slices.BinarySearch(micros, upper)
		return micros[:i]
	}
	for _, id := range ids {
		session := b.inputs.sessions[id]
		counts.AddSorted(id, "user", before(session.users))
		counts.AddSorted(id, "assistant", before(session.assistants))
	}
	return ctx.Err()
}

func (b activityReportBackend) ActivityReportMessageCounts(
	ctx context.Context, ids []string, q activity.Query, artifacts *activity.CandidateArtifacts,
) error {
	counts := activity.NewMessageAccumulator(q, artifacts)
	for _, message := range *b.counted {
		if err := ctx.Err(); err != nil {
			return err
		}
		role := "assistant"
		if message.user {
			role = "user"
		}
		counts.Add(message.sessionID, role, time.UnixMicro(message.timestamp).UTC())
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
	rangeStartUTC, rangeEndUTC := db.ActivityReportInstantBoundsUTC(q)
	lowerBound := db.PaddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
	upperBound := db.PaddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)
	pricing, err := s.loadPricingMap(ctx)
	if err != nil {
		return activity.CandidateArtifacts{}, fmt.Errorf("loading pg pricing: %w", err)
	}
	pricingDigest, err := export.EffectivePricingDigest(pricing)
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}

	var selection, version string
	var revision int64
	if !q.Partial && !s.hostedRelations && !db.TerminationUsesTime(f.Termination) {
		version, err = s.activityKeptVersion(ctx, pricingDigest)
		if err != nil {
			return activity.CandidateArtifacts{}, err
		}
		if version != "" {
			selection = kept.Selection(f, q)
			var report kept.Report
			var hit bool
			report, revision, hit, err = s.keptActivityReport(ctx, selection, version, f,
				rangeStartUTC, rangeEndUTC, lowerBound, upperBound)
			if err != nil {
				return activity.CandidateArtifacts{}, err
			}
			if hit {
				activity.ReportProgress(onProgress, report.Done)
				return report.Artifacts, nil
			}
			if revision == 0 {
				selection = ""
			}
		}
	}

	sessions, ids, err := s.activityReportSessions(
		ctx, f, rangeStartUTC, rangeEndUTC, nil)
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}
	activity.ReportProgress(onProgress, activity.Progress{
		Phase: activity.ProgressLoadingUsage, SessionsTotal: len(sessions),
	})

	var read map[string]struct{}
	if selection != "" {
		read = make(map[string]struct{}, len(ids))
	}
	var inputs *pgActivityInputs
	var inputVersion string
	if q.Partial && !s.hostedRelations && s.readTx == nil {
		inputVersion, err = s.activityKeptVersion(ctx, "")
		if err != nil {
			return activity.CandidateArtifacts{}, err
		}
		if inputVersion != "" {
			inputs, err = s.activityReportInputs(ctx, inputVersion, lowerBound, upperBound)
			if err != nil {
				return activity.CandidateArtifacts{}, err
			}
		}
	}
	var counted []pgActivityCountedMessage
	backend := activityReportBackend{
		counted:       &counted,
		read:          read,
		inputs:        inputs,
		pricing:       pricing,
		pricingDigest: pricingDigest,
		Store:         s,
		lowerBound:    lowerBound, upperBound: upperBound,
	}
	var reportBackend activity.ReportBackend = backend
	if inputs != nil {
		reportBackend = activityReportRunBackend{backend}
	}
	artifacts, done, err := activity.BuildReportArtifacts(
		ctx, reportBackend, sessions, ids, q, onProgress, "aggregating pg activity report")
	if err == nil && ctx.Err() == nil && inputs != nil {
		for id := range inputs.sessions {
			if !inputs.candidates[id] {
				delete(inputs.sessions, id)
			}
		}
		s.runtimeState().activityInputs.Put(lowerBound+"|"+upperBound, inputVersion, []pgActivityInputs{*inputs})
	}
	if err == nil && selection != "" {
		candidates := slices.Clip(slices.Clone(ids))
		slices.Sort(candidates)
		for _, id := range candidates {
			delete(read, id)
		}
		s.runtimeState().activityKept.Put(selection, version, []pgActivityKept{{
			report: kept.Report{Artifacts: artifacts, Done: done}, revision: revision,
			candidates: candidates, peers: slices.Clip(slices.Sorted(maps.Keys(read))),
			labels: activity.ProjectLabels(sessions),
		}})
	}
	return artifacts, err
}

// pgActivityCountedMessage lives only until this report finishes aggregation.
type pgActivityCountedMessage struct {
	sessionID string
	timestamp int64
	user      bool
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
	for i, o := range rowsAcc {
		inputTok, outputTok, cacheCrTok, cacheRdTok, reasoningTok := pgDailyUsageRowTokens(
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
			Model:               o.scan.model,
			SessionID:           o.scan.sessionID,
			Timestamp:           o.tsText,
			MessageOrdinal:      o.ordinal,
			UsageSource:         o.scan.usageSource,
			InputTokens:         inputTok,
			OutputTokens:        outputTok,
			CacheCreationTokens: cacheCrTok,
			CacheReadTokens:     cacheRdTok,
			WebSearchRequests: pgUsageRowWebSearchRequests(
				o.scan.usageSource, o.scan.tokenJSON),
			Agent:           o.scan.agent,
			ProviderID:      o.scan.providerID,
			ClaudeMessageID: o.scan.claudeMessageID,
			ClaudeRequestID: o.scan.claudeRequestID,
			SourceUUID:      o.scan.sourceUUID,
			UsageDedupKey:   o.scan.usageDedupKey,
		}
		snapshotRows[i].Contributes = activity.UsageDataContributes(
			o.scan.cost.Valid, inputTok, outputTok, reasoningTok,
			cacheCrTok, cacheRdTok,
			pgUsageRowWebSearchRequests(o.scan.usageSource, o.scan.tokenJSON))
	}
	return activity.SelectSessionUsageRows(ctx, snapshotRows, func(i int, webSearches int) (activity.UsagePrice, error) {
		o := rowsAcc[i]
		r := o.scan
		costRow := r
		var sessionCost *money.Money
		if r.costSource == db.CopilotReportedCostSource && r.cost.Valid {
			v := money.Money{Microdollars: r.cost.Int64}
			sessionCost = &v
			costRow.cost = sql.NullInt64{}
			rateResolver.RecordUnattributedReported()
		}
		costRow.cost = db.ReportedCostForPricing(costRow.cost, costRow.costStatus,
			pgCatalogPricesAt(rateResolver, costRow.model, costRow.pricingTS))
		cost, priced, contributes, priceErr := pgSessionRowCostWithWebSearchRequests(
			costRow, webSearches, rateResolver)
		if priceErr != nil {
			return activity.UsagePrice{}, priceErr
		}
		costSource := export.CostSourceComputed
		if costRow.cost.Valid {
			costSource = export.CostSourceReported
		}
		recorded, _ := db.RecordedRowCost(r.cost, r.costStatus, costRow.cost)
		return activity.UsagePrice{
			Cost: cost, CostSource: costSource, SessionCost: sessionCost,
			RecordedCost: recorded, Priced: priced, Contributes: contributes,
		}, nil
	})
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
	only []string,
) ([]activity.SessionMeta, []string, error) {
	pb := &paramBuilder{}
	where := buildAnalyticsWhereWithDate(f, "", pb, false, "s.id", s.sessionDialect())
	lower := pb.add(rangeStartUTC)
	upper := pb.add(rangeEndUTC)
	if only != nil {
		where += " AND s.id = ANY(" + pb.add(only) + ")"
	}

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
		s.is_automated,
		s.relationship_type = 'subagent' AS is_subagent
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
			&m.Machine, &startedAt, &endedAt, &m.IsAutomated, &m.IsSubagent,
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
	return s.activityReportCandidateSourceWithCounts(ids, q, nil)
}

// activityReportCandidateSourceWithCounts also appends to counted, when set,
// each counted message from the range start to the effective end.
func (s *Store) activityReportCandidateSourceWithCounts(ids []string, q activity.Query, counted *[]pgActivityCountedMessage) activity.CandidateSource {
	return func(
		ctx context.Context,
		yield func(activity.IntervalCandidate) error,
	) error {
		if len(ids) == 0 {
			return nil
		}
		terminal, pairs, read, err := s.readActivityCandidates(ctx, ids, q, counted != nil)
		if err != nil {
			return err
		}
		if counted != nil {
			*counted = append(*counted, read...)
		}
		sort.Slice(pairs, func(i, j int) bool {
			return activity.CandidateLess(pairs[i], pairs[j])
		})
		messageSource := func(
			ctx context.Context,
			yield func(activity.IntervalCandidate) error,
		) error {
			for _, candidate := range pairs {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := yield(candidate); err != nil {
					return err
				}
			}
			return nil
		}
		return activity.MergeCandidateSlice(terminal, messageSource)(ctx, yield)
	}
}

// readActivityCandidates reads the sessions' terminal tool candidates, in
// candidate stream order, beside their message pairs, which come from
// slices of the sessions read at once on the pool and in order on a read
// snapshot. Each session's pairs sit together in the result. With
// withCounts, it also returns each counted message from the range start to
// the effective end, in no set order.
func (s *Store) readActivityCandidates(
	ctx context.Context, ids []string, q activity.Query, withCounts bool,
) (terminal, pairs []activity.IntervalCandidate, counted []pgActivityCountedMessage, err error) {
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

	readTerminal := func(ctx context.Context) ([]activity.IntervalCandidate, error) {
		terminalRows, err := s.pg.QueryContext(
			ctx, terminalQuery, pgx.QueryExecModeDescribeExec, ids, lower, q.EffectiveEnd,
		)
		if err != nil {
			return nil, fmt.Errorf("querying pg activity report terminal candidates: %w", err)
		}
		defer terminalRows.Close()
		var terminal []activity.IntervalCandidate
		for terminalRows.Next() {
			candidate, scanErr := scanCandidate(terminalRows)
			if scanErr != nil {
				return nil, scanErr
			}
			terminal = append(terminal, candidate)
		}
		if err := terminalRows.Err(); err != nil {
			return nil, err
		}
		return terminal, terminalRows.Close()
	}
	n := max(1, min(activityMessageSlices, (len(ids)+activityMessageSliceSessions-1)/activityMessageSliceSessions))
	parts := make([][]string, n)
	for i, id := range ids {
		parts[i%n] = append(parts[i%n], id)
	}
	slicePairs := make([][]activity.IntervalCandidate, n)
	sliceCounted := make([][]pgActivityCountedMessage, n)
	g, readCtx := errgroup.WithContext(ctx)
	if !s.supportsConcurrentReads() {
		// A read snapshot shares one transaction, which runs one statement
		// at a time, so its reads run in order.
		g.SetLimit(1)
	}
	g.Go(func() (err error) {
		terminal, err = readTerminal(readCtx)
		return err
	})
	for i, part := range parts {
		g.Go(func() (err error) {
			slicePairs[i], sliceCounted[i], err = s.readActivityMessagePairs(readCtx, part, q, withCounts)
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, nil, nil, err
	}
	return terminal, slices.Concat(slicePairs...), slices.Concat(sliceCounted...), nil
}

// ActivityReportCandidateSource exposes the backend's mechanical pairing
// stream for cross-backend contract tests. Activity semantics remain in the
// shared aggregator.
func (s *Store) ActivityReportCandidateSource(
	ids []string, q activity.Query,
) activity.CandidateSource {
	return s.activityReportCandidateSource(ids, q)
}

func activityReportClaudePeerPredicate(sessions, lower, upper string) string {
	return pgUsageMessageEligibility + `
		AND (m.claude_message_id, m.claude_request_id) IN (
			SELECT m.claude_message_id, m.claude_request_id
			FROM messages m JOIN sessions s ON s.id = m.session_id
			WHERE ` + pgUsageMessageEligibility + `
				AND m.session_id = ANY(` + sessions + `)
				AND m.claude_message_id != ''
				AND COALESCE(m.timestamp, s.started_at) >= ` + lower + `::timestamptz
				AND COALESCE(m.timestamp, s.started_at) <= ` + upper + `::timestamptz
		)`
}

// activityReportUsage selects complete snapshots across the padded range,
// then keeps rows attributed to the candidate sessions. Per-row cost is
// computed after selection so filtered rows do not affect pricing metadata.
func (s *Store) activityReportUsage(
	ctx context.Context, ids []string, lowerBound, upperBound string, q activity.Query,
	read map[string]struct{}, pricing []export.EffectivePricingRow, pricingDigest string, inputs *pgActivityInputs,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	out := []activity.UsageRow{}

	raw, err := s.activityUsageInputs(ctx, ids, lowerBound, upperBound, inputs)
	if err != nil {
		return nil, nil, err
	}
	rateResolver := export.NewPricingResolverWithDigest(pricing, pricingDigest)
	if len(ids) == 0 {
		block, err := rateResolver.BuildBlock()
		if err != nil {
			return nil, nil, fmt.Errorf("building pricing block: %w", err)
		}
		return out, &block, nil
	}

	order := make([]int, len(raw))
	for i, r := range raw {
		order[i] = i
		if read != nil {
			read[r.sessionID.Value()] = struct{}{}
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := raw[order[i]], raw[order[j]]
		if a.us != b.us {
			return a.us < b.us
		}
		if a.sessionID != b.sessionID {
			return a.sessionID.Value() < b.sessionID.Value()
		}
		ao, bo := int64(-1), int64(-1)
		if a.ordinalValid {
			ao = int64(a.ordinal)
		}
		if b.ordinalValid {
			bo = int64(b.ordinal)
		}
		return ao < bo
	})
	selection := make([]activity.UsageSelectionRow, len(order))
	for i, index := range order {
		r := raw[index]
		selection[i] = activity.UsageSelectionRow{SessionID: r.sessionID.Value(), Timestamp: time.UnixMicro(r.us).UTC(), TimestampValid: true, OutputTokens: int(r.output), WebSearchRequests: int(r.webSearches), Agent: r.agent.Value(), ClaudeMessageID: r.claudeMessageID, ClaudeRequestID: r.claudeRequestID, SourceUUID: r.sourceUUID, UsageDedupKey: r.usageDedupKey}
	}
	mask, attribution, webSearchRequests, err := activity.SelectUsageSurvivorsForSessions(ctx, q.RangeStart, q.RangeEnd, q.EffectiveEnd, selection, ids)
	if err != nil {
		return nil, nil, err
	}
	out = make([]activity.UsageRow, 0, len(order))
	for i, index := range order {
		r := raw[index] //nolint:nilaway // order contains only indices from raw.
		if !mask[i] {
			continue
		}
		inputTok, outputTok := int(r.input), int(r.output)
		costRow := r
		var sessionCost *money.Money
		if r.copilotReported && r.costValid {
			v := money.Money{Microdollars: r.cost}
			sessionCost = &v
			costRow.costValid = false
			rateResolver.RecordUnattributedReported()
		}
		if costRow.costValid && costRow.costRecorded {
			pricingTS := sql.NullTime{
				Time: time.UnixMicro(costRow.us).UTC(), Valid: !costRow.pricingTimeAbsent,
			}
			costRow.costValid = db.ReportedCostForPricing(
				sql.NullInt64{Int64: costRow.cost, Valid: true},
				usagefacts.CostStatusRecorded,
				pgCatalogPricesAt(rateResolver, costRow.model.Value(), pricingTS)).Valid
		}
		cost, priced, contributes, priceErr := pgActivityReportRowStatusWithWebSearchRequests(
			costRow, webSearchRequests[i], rateResolver)
		if priceErr != nil {
			return nil, nil, priceErr
		}
		costSource := export.CostSourceComputed
		if costRow.costValid {
			costSource = export.CostSourceReported
		}
		row := activity.UsageRow{SessionID: attribution[i], Model: r.model.Value(), Timestamp: FormatISO8601(time.UnixMicro(r.us).UTC()), Agent: r.agent.Value(), ProviderID: r.providerID.Value(), ClaudeMessageID: r.claudeMessageID, ClaudeRequestID: r.claudeRequestID, SourceUUID: r.sourceUUID, UsageDedupKey: r.usageDedupKey}
		row.InputTokens = inputTok
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

func pgActivityReportRowStatusWithWebSearchRequests(
	r pgActivityUsageRow, webSearches int, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	pricingTS := sql.NullTime{Time: time.UnixMicro(r.us).UTC(), Valid: !r.pricingTimeAbsent}
	pricedModel, lookup := pricing.ResolveAt(
		r.model.Value(), pgUsageLookupModel(r.model.Value(), pricingTS),
		pgUsagePricingTimestamp(pricingTS),
	)
	inTok, outTok, crTok, rdTok, reasoningTok := int(r.input), int(r.output), int(r.cacheCreate), int(r.cacheRead), int(r.reasoning)
	cr1hTok := int(r.cacheCreate1h)
	if r.costValid {
		pricing.RecordResolvedReported(r.model.Value(), pricedModel, lookup)
		return money.Money{Microdollars: r.cost}, true, true, nil
	}
	if inTok == 0 && outTok == 0 && reasoningTok == 0 &&
		crTok == 0 && rdTok == 0 && webSearches == 0 {
		return money.Money{}, true, false, nil
	}
	if !lookup.OK {
		pricing.RecordResolvedComputed(r.model.Value(), pricedModel, lookup)
		fee, feeErr := export.WebSearchFee(webSearches)
		if feeErr != nil {
			return money.Money{}, false, false, feeErr
		}
		return fee, false, true, nil
	}
	pricedModel, lookup, err = pricing.ResolveBilledAt(
		r.providerID.Value(), r.model.Value(), pgUsageLookupModel(r.model.Value(), pricingTS),
		pgUsagePricingTimestamp(pricingTS))
	if err != nil {
		return money.Money{}, false, false, err
	}
	requestScoped := db.UsageRowIsRequestScoped(r.usageSource.Value(), sql.NullInt64{Int64: int64(r.ordinal), Valid: r.ordinalValid})
	cost, err = lookup.Rates.CostForTokensScoped(
		requestScoped,
		inTok, outTok, reasoningTok, crTok, cr1hTok, rdTok)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing pg activity usage for model %q: %w", r.model.Value(), err)
	}
	cost, err = export.AddWebSearchFee(cost, webSearches)
	if err != nil {
		return money.Money{}, false, false,
			fmt.Errorf("pricing pg activity usage for model %q: %w", r.model.Value(), err)
	}
	db.RecordComputedUsagePricing(
		pricing, r.model.Value(), pricedModel, lookup,
		requestScoped, inTok, crTok, rdTok)
	return cost, true, true, nil
}
