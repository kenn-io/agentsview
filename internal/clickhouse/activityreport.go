package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"

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
	return q.RangeStart.UTC().Format(time.RFC3339Nano),
		q.RangeEnd.UTC().Format(time.RFC3339Nano)
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

	candidateWhere, candidateArgs := clickActivityReportCandidateWhere(
		f, rangeStartUTC, rangeEndUTC)
	sessions, ids, err := s.activityReportSessions(ctx, candidateWhere, candidateArgs)
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}
	// Send the already selected IDs as native data instead of repeating discovery.
	table, err := ext.NewTable("activity_candidate_ids", ext.Column("id", "String"))
	if err != nil {
		return activity.CandidateArtifacts{}, fmt.Errorf("creating activity candidate table: %w", err)
	}
	for _, id := range ids {
		if err := table.Append(id); err != nil {
			return activity.CandidateArtifacts{}, fmt.Errorf("adding activity candidate: %w", err)
		}
	}
	ctx = chdriver.Context(ctx, chdriver.WithExternalTable(table))
	candidates := chSessionSet{body: "SELECT id FROM activity_candidate_ids"}
	clickReportProgress(onProgress, activity.Progress{
		Phase: activity.ProgressLoadingUsage, SessionsTotal: len(sessions),
	})

	usage, pricing, err := s.activityReportUsage(
		ctx, candidates, ids, rangeStartUTC, rangeEndUTC, q)
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}

	rowsProcessed := int64(0)
	source := s.activityReportCandidateSource(candidates, ids, q)
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
	ctx context.Context, where string, args []any,
) ([]activity.SessionMeta, []string, error) {
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
			OR (s.id, s.push_version) IN (
				SELECT session_id, push_version FROM terminal_event_snapshots
				WHERE last_terminal_at >= ` + chTimestampSQL + `))
		AND COALESCE(s.started_at, s.created_at) < ` + chTimestampSQL
	return where, append(args, rangeStartUTC, rangeStartUTC, rangeEndUTC)
}

// activityReportCandidateSource streams interval candidates for the sessions
// selected by `candidates`. The set is evaluated inside each statement; `ids`
// is the session list the caller already loaded, and candidates for any
// session outside it are dropped so the stream matches the metadata the
// aggregator was given even if a push lands between the two queries.
func (s *Store) activityReportCandidateSource(
	candidates chSessionSet, ids []string, q activity.Query,
) activity.CandidateSource {
	return func(
		ctx context.Context,
		yield func(activity.IntervalCandidate) error,
	) error {
		if len(ids) == 0 {
			return nil
		}
		paired, terminal, err := s.activityReportPairs(ctx, candidates, q)
		if err != nil {
			return err
		}
		messageSource := func(ctx context.Context, yield func(activity.IntervalCandidate) error) error {
			for _, c := range paired {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := yield(c); err != nil {
					return err
				}
			}
			return nil
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
	return s.activityReportCandidateSource(chSessionSetFromIDs(ids), ids, q)
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
	// Load every chunk before selecting survivors: the cross-session
	// snapshot and dedup passes below need the complete row set, the same
	// way the SQLite and PostgreSQL stores chunk this load.
	var rowsAcc []clickSessionUsageOrderedRow
	err = chQueryChunked(ids, func(chunk []string) error {
		inList, inArgs := chInPlaceholders(chunk)
		query := clickUsageNormalizedQuery(
			chUsageStoredMessageEligibility+" AND s.id IN "+inList,
			chUsageEventEligibility+" AND s.id IN "+inList,
		)
		queryArgs := append([]any{}, inArgs...)
		queryArgs = append(queryArgs, inArgs...)
		chunkRows, err := s.scanActivityUsageRows(ctx, query, queryArgs)
		if err != nil {
			return err
		}
		rowsAcc = append(rowsAcc, chunkRows...)
		return nil
	})
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
	canonicalTokenCoverageBySession, err := activity.CanonicalSessionTokenCoverageContext(ctx, snapshotRows)
	if err != nil {
		return nil, err
	}
	snapshotMask, snapshotAttribution, snapshotWebSearchRequests := activity.ClaudeSnapshotSurvivorSelection(snapshotRows)
	seen := make(map[string]struct{})
	deduplicatedOutputTokens := make(map[string]int)
	discardedContributingSessions := make(map[string]struct{})
	out := make([]activity.UsageRow, 0, len(rowsAcc))
	for i, o := range rowsAcc {
		if !snapshotMask[i] {
			deduplicatedOutputTokens[o.scan.sessionID] += snapshotRows[i].OutputTokens
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
		cost, costSource, priced, contributes, sessionCost, priceErr := clickActivityUsageCost(r, rateResolver)
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

// activityReportUsage loads the usage rows of the candidate sessions plus the
// cross-session Claude snapshot peers needed to pick complete snapshots, all
// in one statement. Candidate sessions and their snapshot keys are relations
// inside the query, so neither the session count nor the key count changes
// the statement size. `ids` is the candidate list already loaded by the
// caller and only limits which survivors are attributed to the report.
func (s *Store) activityReportUsage(
	ctx context.Context,
	candidates chSessionSet,
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

	query, args := clickActivityReportUsageQuery(candidates, lowerBound, upperBound)
	rowsAcc, err := s.scanActivityUsageRows(ctx, query, args)
	if err != nil {
		return nil, nil, err
	}

	// Keep the wide scanned rows in place while ordering their indexes.
	order := make([]int, len(rowsAcc))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := &rowsAcc[order[i]], &rowsAcc[order[j]]
		if a.validTS && b.validTS && !a.ts.Equal(b.ts) {
			return a.ts.Before(b.ts)
		}
		if a.scan.sessionID != b.scan.sessionID {
			return a.scan.sessionID < b.scan.sessionID
		}
		return a.ordinal < b.ordinal
	})
	baseRows := make([]activity.UsageRow, len(rowsAcc))
	for i, index := range order {
		o := &rowsAcc[index]
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
	mask, attribution, webSearchRequests := activity.UsageSurvivorSelectionForSessions(
		q.RangeStart, q.RangeEnd, q.EffectiveEnd, baseRows, ids,
	)
	out = make([]activity.UsageRow, 0, len(rowsAcc))
	for i, index := range order {
		o := &rowsAcc[index]
		if !mask[i] {
			continue
		}
		costRow := o.scan
		costRow.webSearchRequests = webSearchRequests[i]
		cost, costSource, priced, contributes, sessionCost, priceErr := clickActivityUsageCost(costRow, rateResolver)
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

// clickActivityReportUsageQuery reads candidate usage and any peers needed
// for complete-snapshot selection. Key discovery may include obsolete keys:
// a peer-only group cannot survive attribution to the candidate sessions.
func clickActivityReportUsageQuery(
	candidates chSessionSet, lowerBound, upperBound string,
) (string, []any) {
	messageBound := " AND COALESCE(m.timestamp, s.started_at) >= " + chTimestampSQL +
		" AND COALESCE(m.timestamp, s.started_at) <= " + chTimestampSQL
	eventBound := " AND COALESCE(ue.occurred_at, s.started_at) >= " + chTimestampSQL +
		" AND COALESCE(ue.occurred_at, s.started_at) <= " + chTimestampSQL
	timestampBound := "(m.timestamp IS NULL OR (m.timestamp >= " + chTimestampSQL +
		" AND m.timestamp <= " + chTimestampSQL + "))"
	const candidateIn = "s.id IN (SELECT id FROM candidate_sessions)"
	ctes := `candidate_sessions AS (
			SELECT id FROM (` + candidates.body + `)
		), candidate_snapshot_keys AS (
			SELECT DISTINCT m.claude_message_id AS claude_message_id,
				m.claude_request_id AS claude_request_id
			FROM usage_messages m
			WHERE m.session_id IN (SELECT id FROM candidate_sessions)
				AND m.claude_message_id != '' AND m.claude_request_id != ''
				AND ` + timestampBound + `
			SETTINGS final = 0
		), `
	query := clickUsageNormalizedQueryWith(ctes,
		timestampBound+" AND "+chUsageStoredMessageEligibility+`
			AND (m.session_id IN (SELECT id FROM candidate_sessions)
				OR (m.claude_message_id, m.claude_request_id) IN (
					SELECT claude_message_id, claude_request_id
					FROM candidate_snapshot_keys))`+messageBound,
		chUsageEventEligibility+" AND "+candidateIn+eventBound,
	)
	args := slices.Clone(candidates.args)
	for range 4 {
		args = append(args, lowerBound, upperBound)
	}
	// Exact index filtering also reads overlapping newer parts before FINAL.
	// A timestamp correction must not resurrect an older matching version.
	return query + " SETTINGS optimize_move_to_prewhere_if_final = 0, use_skip_indexes_if_final_exact_mode = 1", args
}

// A push writes a session's messages before it publishes the session row, and
// an interrupted push may never publish it. Accept stored usage at or above the
// published version so that window shows the newer rows instead of no usage.
// Rows a shorter republished session left behind stay below it and are skipped.
const chUsageMessageCurrent = "m.push_version >= s.push_version"

// chUsageStoredMessageEligibility is chUsageMessageEligibility for reads of
// usage_messages, which stores a presence flag instead of the raw JSON.
const chUsageStoredMessageEligibility = `
			m.usage_present != 0
			AND m.model != ''
			AND m.model != '<synthetic>'
			AND s.deleted_at IS NULL`

func clickUsageNormalizedQuery(messageWhere, eventWhere string) string {
	return clickUsageNormalizedQueryWith("", messageWhere, eventWhere)
}

// clickUsageNormalizedQueryWith prepends extra common table expressions
// (each terminated by a comma) ahead of usage_raw.
func clickUsageNormalizedQueryWith(ctes, messageWhere, eventWhere string) string {
	maxTok := db.MaxPlausibleTokens
	clamp := func(expr string) string {
		return fmt.Sprintf("least(greatest(%s, toInt64(0)), toInt64(%d))", expr, maxTok)
	}
	msgInput := clamp("usage_input")
	msgOutput := clamp("usage_output")
	msgCacheCr := clamp("usage_cache_create")
	msgCacheCr1h := clamp("usage_cache_create_1h")
	msgCacheRd := clamp("usage_cache_read")
	msgReasoning := clamp("usage_reasoning")
	msgWeb := "greatest(usage_web, toInt64(0))"
	return fmt.Sprintf(`
		WITH %[15]susage_raw AS (
			SELECT m.session_id AS session_id,
				CAST(m.ordinal AS Nullable(Int64)) AS message_ordinal,
				'message' AS source,
				COALESCE(m.timestamp, s.started_at) AS ts,
				m.timestamp AS pricing_ts,
				m.model AS model, m.provider_id AS provider_id,
				m.usage_input AS usage_input,
				m.usage_output AS usage_output,
				m.usage_cache_create AS usage_cache_create,
				m.usage_cache_create_1h AS usage_cache_create_1h,
				m.usage_cache_read AS usage_cache_read,
				m.usage_reasoning AS usage_reasoning,
				m.usage_web AS usage_web,
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
			FROM usage_messages m
			JOIN sessions s ON s.id = m.session_id
			WHERE %[16]s AND %[1]s
			UNION ALL
			SELECT ue.session_id AS session_id,
				ue.message_ordinal AS message_ordinal,
				ue.source AS source,
				COALESCE(ue.occurred_at, s.started_at) AS ts,
				ue.occurred_at AS pricing_ts,
				ue.model AS model, ue.provider_id AS provider_id,
				toInt64(0) AS usage_input,
				toInt64(0) AS usage_output,
				toInt64(0) AS usage_cache_create,
				toInt64(0) AS usage_cache_create_1h,
				toInt64(0) AS usage_cache_read,
				toInt64(0) AS usage_reasoning,
				toInt64(0) AS usage_web,
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
		ctes, chUsageMessageCurrent,
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
	// Start non-nil: the caller indexes the result through a sorted index
	// slice, and NilAway cannot see that an empty result yields no indexes.
	rowsAcc := make([]clickSessionUsageOrderedRow, 0)
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
	sessionCost *money.Money, err error,
) {
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
	if r.cost.Valid {
		pricedModel, lookup := pricing.ResolveAt(
			r.model, canonicalModel, chUsagePricingTimestamp(r.pricingTS),
		)
		pricing.RecordResolvedReported(r.model, pricedModel, lookup)
		return money.Money{Microdollars: r.cost.Int64}, true, true, nil
	}
	if !activity.UsageDataContributes(
		false, r.inputTok, r.outputTok, r.reasoningTok,
		r.cacheCr, r.cacheRd, r.webSearchRequests,
	) {
		return money.Money{}, true, false, nil
	}
	pricedModel, lookup, err := pricing.ResolveBilledAt(
		r.providerID, r.model, canonicalModel, chUsagePricingTimestamp(r.pricingTS))
	if err != nil {
		return money.Money{}, false, false, err
	}
	if !lookup.OK {
		pricing.RecordResolvedComputed(r.model, pricedModel, lookup)
		fee, feeErr := export.WebSearchFee(r.webSearchRequests)
		if feeErr != nil {
			return money.Money{}, false, false, feeErr
		}
		return fee, false, true, nil
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
