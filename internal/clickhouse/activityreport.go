package clickhouse

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/ext"
	chnative "github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/readbase"
	"go.kenn.io/agentsview/internal/usagefacts"
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
	return activity.ReportWithSessions(artifacts), nil
}

type activityReportBackend struct {
	*Store
	candidates    chSessionSet
	rangeStartUTC string
	rangeEndUTC   string
	pairing       *activityReportPairing
}

// ActivityReportSecondRead lets usage run beside the sweep. Pairing already
// reads in its own goroutine and the sweep reads nothing, so overlapping them
// takes no connection that the serial order would not.
func (b activityReportBackend) ActivityReportSecondRead() (func(), bool) {
	return func() {}, true
}

func (b activityReportBackend) ActivityReportUsage(
	ctx context.Context, ids []string, q activity.Query,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	return b.activityReportUsage(ctx, b.candidates, ids, b.rangeStartUTC, b.rangeEndUTC, q)
}

// ActivityReportCandidateSource is the candidate stream the report's runs
// stand for. Reports sweep the runs; see ActivityReportRunSource.
func (b activityReportBackend) ActivityReportCandidateSource(ids []string, q activity.Query) activity.CandidateSource {
	return b.activityCandidateSource(ids, q)
}

func (b activityReportBackend) ActivityReportRunSource([]string, activity.Query) activity.RunSource {
	return b.pairing.runSource()
}

func (b activityReportBackend) ActivityReportMessageCounts(
	ctx context.Context, ids []string, q activity.Query, artifacts *activity.CandidateArtifacts,
) error {
	return b.pairing.countMessages(ctx, ids, q, artifacts)
}

func (s *Store) BuildActivityReportArtifacts(
	ctx context.Context,
	f db.AnalyticsFilter,
	q activity.Query,
	onProgress activity.ProgressFunc,
) (activity.CandidateArtifacts, error) {
	activity.ReportProgress(onProgress, activity.Progress{Phase: activity.ProgressLoadingSessions})
	ctx, err := s.withPartsSnapshot(ctx)
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}
	f.IncludeSubagents = true
	f.IncludeForks = true
	rangeStartUTC, rangeEndUTC := activityReportRangeBoundsUTC(q)
	// An ended range whose kept report was checked against these exact
	// parts needs no further read.
	var selection, fingerprint string
	// Only the reports of ended days a client opens are kept on disk. Every
	// open refreshes the file's modification time, and a sweep removes the
	// files no client opened for activityReportUnopened.
	onDisk := !q.Partial && kept.DayPreset(q, time.Now())
	if !q.Partial {
		selection = kept.Selection(f, q)
		if fingerprint, err = s.partsFingerprint(ctx); err != nil {
			return activity.CandidateArtifacts{}, err
		}
		if kept, _, ok := s.checkedActivityReport(selection, fingerprint, onDisk); ok {
			activity.ReportProgress(onProgress, kept.Done)
			return kept.Artifacts, nil
		}
	}

	candidateWhere, candidateArgs := clickActivityReportCandidateWhere(
		f, rangeStartUTC, rangeEndUTC)
	// A range that has ended does not depend on the time of the request, so
	// its report is kept per the rows it reads; a range in progress moves its
	// effective end with every request and is always built. The key is taken
	// before any read the report depends on, so a push that lands after it
	// makes the next request miss.
	var memoKey string
	if !q.Partial {
		memoKey, err = s.endedActivityReportKey(ctx, candidateWhere, candidateArgs, rangeStartUTC, rangeEndUTC)
		if err != nil {
			return activity.CandidateArtifacts{}, err
		}
		if report, ok := s.reports.Get(selection, memoKey, onDisk); ok {
			s.markActivityReportChecked(selection, fingerprint, memoKey)
			activity.ReportProgress(onProgress, report.Done)
			return report.Artifacts, nil
		}
	}
	// A kept report answers its range until a push changes the listing's
	// parts, so its listing would never be read again.
	sessions, ids, versions, err := s.activityReportSessions(ctx, candidateWhere, candidateArgs, memoKey == "")
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
	activity.ReportProgress(onProgress, activity.Progress{
		Phase: activity.ProgressLoadingUsage, SessionsTotal: len(sessions),
	})

	// Usage selection and interval pairing both depend only on the candidate
	// set, so pair while usage loads instead of serializing the two stages.
	pairCtx, cancelPairs := context.WithCancel(ctx)
	defer cancelPairs()
	pairs := s.startActivityReportPairs(pairCtx, ids, versions, q)
	backend := activityReportBackend{
		Store: s, candidates: candidates,
		rangeStartUTC: rangeStartUTC, rangeEndUTC: rangeEndUTC, pairing: pairs,
	}
	artifacts, done, err := activity.BuildReportArtifacts(
		ctx, backend, sessions, ids, q, onProgress, "aggregating clickhouse activity report")
	if err != nil {
		return activity.CandidateArtifacts{}, err
	}

	if memoKey != "" {
		s.reports.Put(selection, memoKey, onDisk, kept.Report{Artifacts: artifacts, Done: done})
		s.markActivityReportChecked(selection, fingerprint, memoKey)
	}
	return artifacts, nil
}

// markActivityReportChecked records the key an ended range's kept report
// was kept under; its memo version is the parts it was last checked against.
func (s *Store) markActivityReportChecked(selection, fingerprint, key string) {
	s.activityChecks.Put(selection, fingerprint, []string{key})
}

// checkedActivityReport returns the kept report for a selection checked
// against exactly these parts. The parts name every row the report reads,
// so the report's key cannot have changed.
func (s *Store) checkedActivityReport(selection, fingerprint string, onDisk bool) (kept.Report, string, bool) {
	check, ok := s.activityChecks.Get(selection, fingerprint)
	if !ok {
		return kept.Report{}, "", false
	}
	report, ok := s.reports.Get(selection, check[0], onDisk)
	if !ok {
		return kept.Report{}, "", false
	}
	return report, check[0], true
}

// endedActivityReportKey identifies everything the report of an ended
// range reads, or is empty when the usage comes from the raw rows. The
// candidate sessions name their pairing inputs by push version; the usage
// rows in the range are named by the snapshots they were prepared from,
// whichever session they belong to, so a push that touches no session with
// usage in the range leaves the key, and the kept report, in place.
func (s *Store) endedActivityReportKey(
	ctx context.Context, candidateWhere string, candidateArgs []any, lowerBound, upperBound string,
) (string, error) {
	// The candidate sessions are named by their push versions, which name
	// the messages and tool events their pairing reads. Two independent
	// hash sums and the count identify the set without listing it. The read
	// depends on nothing below, so it runs while they do.
	var candidates activityCandidateDigest
	candidatesRead := make(chan error, 1)
	go func() {
		err := s.queryRowContext(ctx, `SELECT `+chActivityCandidateDigestSQL+`
			FROM sessions s WHERE `+candidateWhere, candidateArgs...).Scan(&candidates.hash, &candidates.count)
		if err != nil {
			err = fmt.Errorf("reading activity candidate sessions: %w", err)
		}
		candidatesRead <- err
	}()
	key, err := s.endedActivityReportRowsKey(ctx, lowerBound, upperBound)
	if err := errors.Join(err, <-candidatesRead); err != nil || key == "" {
		return "", err
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%d", key, candidates.hash, candidates.count))
	return hex.EncodeToString(sum[:]), nil
}

// endedActivityReportRowsKey identifies everything but the candidate
// sessions that an ended range's report reads; see endedActivityReportKey.
func (s *Store) endedActivityReportRowsKey(
	ctx context.Context, lowerBound, upperBound string,
) (string, error) {
	state, err := s.preparedUsageState(ctx)
	if err != nil || !state.ready {
		return "", err
	}
	pricing, err := s.pricingSnapshot(ctx)
	if err != nil {
		return "", err
	}
	identity, err := s.tablePartsFingerprint(ctx, []string{"source_project_identity_observations", "source_archives"})
	if err != nil {
		return "", err
	}
	// The raw hosts' rows come from the message and event tables, which
	// the prepared digest below does not cover.
	raw := ""
	if len(state.raw) > 0 {
		if raw, err = s.tablePartsFingerprint(ctx, chRawHostUsageTables); err != nil {
			return "", err
		}
	}
	stored, err := s.readActivityPrepared(ctx, state, lowerBound, upperBound)
	if err != nil {
		return "", err
	}
	lower, err := time.Parse(time.RFC3339Nano, lowerBound)
	if err != nil {
		return "", fmt.Errorf("parsing activity range start: %w", err)
	}
	upper, err := time.Parse(time.RFC3339Nano, upperBound)
	if err != nil {
		return "", fmt.Errorf("parsing activity range end: %w", err)
	}
	var delta []string
	for _, row := range state.deltaRows {
		ts, ok := chDeltaRowTime(row[2])
		if ok && !ts.Before(lower) && !ts.After(upper) {
			delta = append(delta, fmt.Sprintf("%s@%v", row[0], row[24]))
		}
	}
	slices.Sort(delta)
	delta = slices.Compact(delta)
	// The filter and range are not named here: the key is only ever compared
	// under the selection's slot, which names them.
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%s|%s|%s|%s|%s|%d|%q",
		chPreparedUsageComment(), state.pricingDigest, pricing.digest, pricing.catalog.digest, identity, raw,
		stored.hash, stored.count, delta))
	return hex.EncodeToString(sum[:]), nil
}

// chDeltaRowTime reads a prepared delta row's timestamp.
func chDeltaRowTime(value any) (time.Time, bool) {
	switch t := value.(type) {
	case time.Time:
		return t, true
	case *time.Time:
		if t != nil {
			return *t, true
		}
	}
	return time.Time{}, false
}

// countMessages adds the user and assistant message counts to artifacts.
// The derived sessions keep the times of the counted messages, so the
// counts need no read of their own.
func (pairing *activityReportPairing) countMessages(
	ctx context.Context, ids []string, q activity.Query, artifacts *activity.CandidateArtifacts,
) error {
	if err := pairing.wait(ctx); err != nil {
		return err
	}
	counts := activity.NewMessageAccumulator(q, artifacts)
	from := func(micros []int64) []int64 {
		i, _ := slices.BinarySearchFunc(micros, q.RangeStart, func(us int64, start time.Time) int {
			return time.UnixMicro(us).Compare(start)
		})
		return micros[i:]
	}
	for _, id := range ids {
		session := pairing.sessions[id]
		counts.AddSorted(id, "user", from(session.users))
		counts.AddSorted(id, "assistant", from(session.assistants))
	}
	return nil
}

// chSessionIDsDigest names a session set independent of its order.
func chSessionIDsDigest(ids []string) string {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])
}

// activitySessionListing is one candidate listing kept per parts and predicate.
type activitySessionListing struct {
	sessions []activity.SessionMeta
	ids      []string
	versions map[string]uint64
}

// activityReportSessions lists the candidate sessions with the push version
// each was read at, which names the messages and tool events it carries.
// The listing reads sessions and terminal event snapshots only, so it is
// kept per their parts and the predicate.
func (s *Store) activityReportSessions(
	ctx context.Context, where string, args []any, keep bool,
) ([]activity.SessionMeta, []string, map[string]uint64, error) {
	fingerprint, err := s.tablePartsFingerprint(ctx, []string{"sessions", "terminal_event_snapshots"})
	if err != nil {
		return nil, nil, nil, err
	}
	memoKey := fmt.Sprintf("%s|%#v", where, args)
	if cached, ok := s.activitySessionListings.Get(memoKey, fingerprint); ok && len(cached) == 1 {
		listing := cached[0]
		return slices.Clone(listing.sessions), slices.Clone(listing.ids), maps.Clone(listing.versions), nil
	}
	s.activitySessionQueries.Add(1)
	query := `SELECT
		s.id,
		COALESCE(NULLIF(s.display_name, ''), NULLIF(s.session_name, ''), NULLIF(s.project, ''), s.id) AS display_name,
		s.project,
		s.agent,
		s.machine,
		s.started_at,
		s.ended_at,
		s.is_automated AS is_automated,
		s.relationship_type = 'subagent' AS is_subagent,
		s.push_version
	FROM sessions s
	WHERE ` + where

	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, nil, fmt.Errorf(
			"querying clickhouse activity report sessions: %w", err)
	}
	defer rows.Close()

	var sessions []activity.SessionMeta
	var ids []string
	versions := map[string]uint64{}
	for rows.Next() {
		var m activity.SessionMeta
		var startedAt, endedAt any
		var version uint64
		if err := rows.Scan(
			&m.SessionID, &m.Title, &m.Project, &m.Agent,
			&m.Machine, &startedAt, &endedAt, &m.IsAutomated, &m.IsSubagent, &version,
		); err != nil {
			return nil, nil, nil, fmt.Errorf(
				"scanning clickhouse activity report session: %w", err)
		}
		m.StartedAt = formatDBTime(startedAt)
		m.EndedAt = formatDBTime(endedAt)
		sessions = append(sessions, m)
		ids = append(ids, m.SessionID)
		versions[m.SessionID] = version
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf(
			"iterating clickhouse activity report sessions: %w", err)
	}
	if keep {
		s.activitySessionListings.Put(memoKey, fingerprint, []activitySessionListing{{
			sessions: slices.Clone(sessions), ids: slices.Clone(ids), versions: maps.Clone(versions),
		}})
	}
	return sessions, ids, versions, nil
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

// chActivityCandidateDigestSQL selects the digest that identifies a set of
// candidate sessions by their push versions: two independent hash sums,
// then the count.
const chActivityCandidateDigestSQL = "sum(sipHash64(s.id, s.push_version)) || ':' || " +
	"sum(cityHash64(s.id, s.push_version)), count()"

type activityCandidateDigest struct {
	hash  string
	count uint64
}

// readActivityPrepared reads the digest of the prepared snapshots the
// range's stored usage rows come from, whichever session they belong to.
func (s *Store) readActivityPrepared(
	ctx context.Context, state preparedUsageState, lowerBound, upperBound string,
) (activityCandidateDigest, error) {
	query := `SELECT toString(sum(sipHash64(session_id, snapshot_revision))), count()
		FROM (SELECT DISTINCT session_id, snapshot_revision FROM prepared_usage
		WHERE ` + chPreparedUsageKeyColumn + ` >= ` + chTimestampSQL + ` AND ` + chPreparedUsageKeyColumn + ` <= ` + chTimestampSQL + `
		AND ts >= ` + chTimestampSQL + ` AND ts <= ` + chTimestampSQL
	if replaced := state.replaced(); len(replaced) > 0 {
		table, err := usageSessionListTable("usage_replaced_sessions", replaced)
		if err != nil {
			return activityCandidateDigest{}, err
		}
		ctx = chdriver.Context(ctx, chdriver.WithExternalTable(table))
		query += " AND session_id NOT IN (SELECT id FROM usage_replaced_sessions)"
	}
	var digest activityCandidateDigest
	if err := s.queryRowContext(ctx, query+") SETTINGS final=0",
		lowerBound, upperBound, lowerBound, upperBound).Scan(&digest.hash, &digest.count); err != nil {
		return activityCandidateDigest{}, fmt.Errorf("reading activity usage snapshots: %w", err)
	}
	return digest, nil
}

// activityReportPairing holds a report's derived sessions and their runs
// clipped to the range, started ahead of their consumer.
type activityReportPairing struct {
	done     chan struct{}
	sessions map[string]activitySessionDerived
	runs     []activity.Run
	err      error
}

// startActivityReportPairs derives the candidate sessions' activity in
// the background. Runs come only from the sessions in ids, the list the
// caller already loaded, so the stream matches the metadata the aggregator
// was given even if a push lands between the two queries.
func (s *Store) startActivityReportPairs(
	ctx context.Context, ids []string, versions map[string]uint64, q activity.Query,
) *activityReportPairing {
	pairing := &activityReportPairing{done: make(chan struct{})}
	if len(ids) == 0 {
		close(pairing.done)
		return pairing
	}
	go func() {
		defer close(pairing.done)
		pairing.sessions, pairing.err = s.activityReportDerived(ctx, ids, versions, q.GapCapSeconds)
		if pairing.err != nil {
			return
		}
		pairing.runs = clipActivityRuns(pairing.sessions, ids, q)
	}()
	return pairing
}

// clipActivityRuns clips the sessions' runs to the report range and orders
// them by start.
func clipActivityRuns(sessions map[string]activitySessionDerived, ids []string, q activity.Query) []activity.Run {
	n := 0
	for _, session := range sessions {
		n += session.runs.Runs()
	}
	runs := make([]activity.Run, 0, n)
	for _, id := range ids {
		runs = sessions[id].runs.AppendClipped(runs, id, q.RangeStart, q.EffectiveEnd)
	}
	activity.SortRuns(runs)
	return runs
}

func (pairing *activityReportPairing) wait(ctx context.Context) error {
	select {
	case <-pairing.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return pairing.err
}

// runSource streams the runs once they are derived.
func (pairing *activityReportPairing) runSource() activity.RunSource {
	return func(ctx context.Context, yield func(activity.Run) error) error {
		if err := pairing.wait(ctx); err != nil {
			return err
		}
		for _, run := range pairing.runs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := yield(run); err != nil {
				return err
			}
		}
		return nil
	}
}

// ActivityReportCandidateSource exposes the backend's mechanical pairing
// stream for cross-backend contract tests. Activity semantics remain in the
// shared aggregator.
func (s *Store) ActivityReportCandidateSource(
	ids []string, q activity.Query,
) activity.CandidateSource {
	return s.activityCandidateSource(ids, q)
}

// activityCandidateSource reads and pairs the sessions in ids and streams
// their candidates in start order.
func (s *Store) activityCandidateSource(ids []string, q activity.Query) activity.CandidateSource {
	return func(ctx context.Context, yield func(activity.IntervalCandidate) error) error {
		if len(ids) == 0 {
			return nil
		}
		inputs, err := s.readActivitySessionInputs(ctx, ids)
		if err != nil {
			return err
		}
		lower := q.RangeStart.Add(-time.Duration(q.GapCapSeconds) * time.Second)
		paired, terminal, err := s.pairActivitySessions(ctx, inputs, ids, lower, q.EffectiveEnd, q)
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
	// recordedCost is a harness-recorded cost the SQL left out of cost;
	// clickActivityUsagePrice decides whether it prices the row.
	recordedCost sql.NullInt64
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
	err = db.QueryChunkedSize(ids, readbase.MaxSQLVars, func(chunk []string) error {
		inList, inArgs := db.InPlaceholders(chunk)
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
	for i, o := range rowsAcc {
		snapshotRows[i] = activity.UsageRow{
			Model:               o.scan.model,
			SessionID:           o.scan.sessionID,
			Timestamp:           o.scan.ts,
			MessageOrdinal:      db.UsageRowMessageOrdinal(o.scan.messageOrdinal),
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
			o.scan.cost.Valid, o.scan.inputTok, o.scan.outputTok,
			o.scan.reasoningTok, o.scan.cacheCr, o.scan.cacheRd,
			o.scan.webSearchRequests)
	}
	return activity.SelectSessionUsageRows(ctx, snapshotRows, func(i int, webSearches int) (activity.UsagePrice, error) {
		r := rowsAcc[i].scan
		r.webSearchRequests = webSearches
		cost, costSource, priced, contributes, sessionCost, priceErr := clickActivityUsageCost(r, rateResolver)
		if priceErr != nil {
			return activity.UsagePrice{}, priceErr
		}
		var recorded *money.Money
		if r.recordedCost.Valid {
			recorded = &money.Money{Microdollars: r.recordedCost.Int64}
		}
		return activity.UsagePrice{
			Cost: cost, CostSource: costSource, SessionCost: sessionCost,
			RecordedCost: recorded, Priced: priced, Contributes: contributes,
		}, nil
	})
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
	snapshot, err := s.pricingSnapshot(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("loading clickhouse pricing: %w", err)
	}
	rateResolver := export.NewPricingResolverWithDigest(snapshot.rows, snapshot.digest)
	out := []activity.UsageRow{}
	if len(ids) > 0 {
		if out, err = s.selectActivityReportUsage(ctx, candidates, ids, lowerBound, upperBound, q, rateResolver, snapshot.digest); err != nil {
			return nil, nil, err
		}
	}
	block, err := rateResolver.BuildBlock()
	if err != nil {
		return nil, nil, fmt.Errorf("building pricing block: %w", err)
	}
	return out, &block, nil
}

func (s *Store) selectActivityReportUsage(
	ctx context.Context,
	candidates chSessionSet,
	ids []string,
	lowerBound, upperBound string,
	q activity.Query,
	rateResolver *export.PricingResolver,
	pricingDigest string,
) ([]activity.UsageRow, error) {
	state, err := s.preparedUsageState(ctx)
	if err != nil {
		return nil, err
	}
	// The survivors are selected, priced, and summed in ClickHouse, so serve
	// receives totals instead of every row.
	return s.keptActivityUsageSums(ctx, activityUsageSourceFor(state, lowerBound, upperBound),
		candidates, ids, lowerBound, upperBound, q, rateResolver, pricingDigest)
}

// chPreparedActivityRangeSQL bounds prepared rows to a range. The sort
// column agrees with ts for every stored timestamp and is the epoch
// otherwise, so bounding both reads only the range's granules while keeping
// the null-timestamp exclusion of the ts predicate.
func chPreparedActivityRangeSQL(lowerBound, upperBound string) (string, []any) {
	return chPreparedUsageKeyColumn + " >= " + chTimestampSQL + " AND " + chPreparedUsageKeyColumn + " <= " + chTimestampSQL +
			" AND ts >= " + chTimestampSQL + " AND ts <= " + chTimestampSQL,
		[]any{lowerBound, upperBound, lowerBound, upperBound}
}

// A push writes a session's messages before it publishes the session row, and
// an interrupted push may never publish it. Accept stored usage at or above the
// published content version so that window shows the newer rows instead of no
// usage. Rows a shorter republished session left behind stay below it and are
// skipped. A push that keeps a session's messages republishes its row with the
// messages' version as content_version.
const chUsageMessageCurrent = "m.push_version >= s.content_version"

// chStoredMessageUsable renders the condition a stored message meets when
// its usage counts, from the expressions of its usage_present and model
// fields. Row filters pass a message's columns; the rows-behind count and the
// snapshot prepares_usage column pass the fields of usage_facts elements.
func chStoredMessageUsable(present, model string) string {
	return present + ` != 0
			AND ` + model + ` != ''
			AND ` + model + ` != '<synthetic>'`
}

// chUsageStoredMessageEligibility is the condition a usage_messages row meets
// when its usage counts. It reads the presence flag the view stores instead of
// the raw JSON. Like the messages rows it mirrors, it takes no version check: a
// push deletes the ordinals a session no longer has from both tables in the
// same step.
var chUsageStoredMessageEligibility = `
			` + chStoredMessageUsable("m.usage_present", "m.model") + `
			AND s.deleted_at IS NULL`

func clickUsageNormalizedQuery(messageWhere, eventWhere string) string {
	return clickUsageNormalizedQueryWith("", messageWhere, eventWhere)
}

// clickUsageNormalizedQueryWith prepends extra common table expressions
// (each terminated by a comma) ahead of usage_raw.
func clickUsageNormalizedQueryWith(ctes, messageWhere, eventWhere string) string {
	return clickUsageNormalizedQueryFrom(ctes, messageWhere, eventWhere,
		"usage_messages m JOIN sessions s ON s.id = m.session_id", chStoredMessageRecordedCost,
		"usage_events ue JOIN sessions s ON s.id = ue.session_id", chUsageMessageCurrent, "")
}

// chStoredMessageRecordedCost is a usage_messages row's recorded cost.
const chStoredMessageRecordedCost = "m." + usageRecordedCostColumn

// chSnapshotMessageFrom pairs each snapshot message fact with its recorded
// cost; both arrays map usage_messages, so they have the same length.
const chSnapshotMessageFrom = "usage_session_snapshots s ARRAY JOIN s.usage_facts AS m, s." +
	usageRecordedCostsColumn + " AS m_recorded_cost"

// chSnapshotMessageRecordedCost is the recorded cost chSnapshotMessageFrom
// pairs with a fact.
const chSnapshotMessageRecordedCost = "m_recorded_cost"

// clickUsageNormalizedQueryFrom renders the normalized usage rows read from
// the message and event sources. A non-empty revision expression is carried
// on every row as snapshot_revision, naming the session snapshot the row was
// derived from.
//
// A harness-recorded event cost (usagefacts.CostStatusRecorded) is evidence,
// not a price: it is moved out of cost_microdollars into
// recorded_cost_microdollars, so price keys and the SQL sums treat the row as
// computed, and Go decides per row whether the recorded cost prices it
// (db.ReportedCostForPricing). chUsageRawSQL applies the same split. A
// message's recorded cost comes from messageRecordedCost, an expression over
// messageFrom, and its cost_microdollars stays NULL.
func clickUsageNormalizedQueryFrom(ctes, messageWhere, eventWhere, messageFrom, messageRecordedCost, eventFrom, currentVersion, revision string) string {
	revisionColumn, revisionSelect := "", ""
	if revision != "" {
		revisionColumn = ",\n\t\t\t\t" + revision + " AS snapshot_revision"
		revisionSelect = ", snapshot_revision"
	}
	return fmt.Sprintf(`
		WITH %[3]susage_raw AS (
			SELECT s.id AS session_id,
				m.ordinal AS message_ordinal,
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
				'' AS usage_dedup_key,
				0 AS input_tokens, 0 AS output_tokens,
				0 AS cache_create, 0 AS cache_read,
				0 AS reasoning_tokens,
				NULL AS cost_microdollars,
				'' AS cost_source,
				%[8]s AS recorded_cost_microdollars,
				COALESCE(m.timestamp, s.started_at) AS ts_raw,
				s.started_at AS started_at_raw%[7]s
			FROM %[5]s
			WHERE %[4]s AND %[1]s
			UNION ALL
			SELECT s.id AS session_id,
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
				0 AS usage_web,
				s.agent AS agent,
				'' AS claude_message_id,
				'' AS claude_request_id,
				'' AS source_uuid,
				if(ue.dedup_key != '',
					concat(s.id, ':', ue.source, ':', ue.dedup_key),
					concat(s.id, ':', ue.source, ':id:', ue.id)) AS usage_dedup_key,
				ue.input_tokens AS input_tokens,
				ue.output_tokens AS output_tokens,
				ue.cache_creation_input_tokens AS cache_create,
				ue.cache_read_input_tokens AS cache_read,
				ue.reasoning_tokens AS reasoning_tokens,
				if(ue.cost_status = 'recorded' AND ue.cost_source != 'copilot-reported', NULL, ue.cost_microdollars) AS cost_microdollars,
				ue.cost_source AS cost_source,
				if(ue.cost_status = 'recorded' AND ue.cost_source != 'copilot-reported', ue.cost_microdollars, NULL) AS recorded_cost_microdollars,
				COALESCE(ue.occurred_at, s.started_at) AS ts_raw,
				s.started_at AS started_at_raw%[7]s
			FROM %[6]s
			WHERE %[2]s
		)`,
		messageWhere, eventWhere, ctes, currentVersion, messageFrom, eventFrom, revisionColumn,
		messageRecordedCost,
	) + " SELECT " + clickUsageNormalizedColumns() + revisionSelect + " FROM usage_raw"
}

func clickUsageNormalizedColumns() string {
	msgInput := chClampTokens("usage_input")
	msgOutput := chClampTokens("usage_output")
	msgCacheCr := chClampTokens("usage_cache_create")
	msgCacheCr1h := chClampTokens("usage_cache_create_1h")
	msgCacheRd := chClampTokens("usage_cache_read")
	msgReasoning := chClampTokens("usage_reasoning")
	msgWeb := "greatest(usage_web, 0)"
	return fmt.Sprintf(`session_id, message_ordinal, ts, pricing_ts, source, model,
			provider_id, agent, claude_message_id, claude_request_id, source_uuid,
			usage_dedup_key,
			CASE
				WHEN source = 'message' THEN %[1]s
				WHEN source = 'session' THEN greatest(input_tokens, 0)
				ELSE %[6]s
			END AS input_tokens_norm,
			CASE
				WHEN source = 'message' THEN %[2]s
				WHEN source = 'session' THEN greatest(output_tokens, 0)
				ELSE %[7]s
			END AS output_tokens_norm,
			CASE
				WHEN source = 'message' THEN %[3]s
				WHEN source = 'session' THEN greatest(cache_create, 0)
				ELSE %[8]s
			END AS cache_create_norm,
			CASE
				WHEN source = 'message' THEN %[4]s
				ELSE 0
			END AS cache_create_1h_norm,
			CASE
				WHEN source = 'message' THEN %[5]s
				WHEN source = 'session' THEN greatest(cache_read, 0)
				ELSE %[9]s
			END AS cache_read_norm,
			CASE
				WHEN source = 'message' THEN %[10]s
				WHEN source = 'session' THEN greatest(reasoning_tokens, 0)
				ELSE %[11]s
			END AS reasoning_tokens_norm,
			CASE
				WHEN source = 'message' THEN %[12]s
				ELSE 0
			END AS web_search_requests_norm,
			cost_microdollars, cost_source, recorded_cost_microdollars`,
		msgInput, msgOutput, msgCacheCr, msgCacheCr1h, msgCacheRd,
		chClampTokens("input_tokens"), chClampTokens("output_tokens"), chClampTokens("cache_create"),
		chClampTokens("cache_read"), msgReasoning, chClampTokens("reasoning_tokens"), msgWeb,
	)
}

// scanActivityUsageRows runs query on the native connection and returns
// its rows, in the columns of chPreparedUsageColumns.
func (s *Store) scanActivityUsageRows(
	ctx context.Context, query string, args []any,
) ([]clickSessionUsageOrderedRow, error) {
	rows, err := s.bulk.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse activity usage: %w", err)
	}
	defer rows.Close()
	var out []clickSessionUsageOrderedRow
	for rows.Next() {
		var r clickSessionUsageOrderedRow
		if err := scanActivityUsageRow(rows, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse activity usage: %w", err)
	}
	return out, nil
}

// scanActivityUsageRow scans the current row, in the columns of
// chPreparedUsageColumns, into out. The destinations match the column
// types exactly, which the native API requires.
func scanActivityUsageRow(rows chnative.Rows, out *clickSessionUsageOrderedRow) error {
	var r clickActivityReportUsageRow
	var messageOrdinal, cost, recorded *int64
	var ts, pricingTS *time.Time
	var input, output, cacheCr, cacheCr1h, cacheRd, reasoning, webSearch int64
	if err := rows.Scan(
		&r.sessionID, &messageOrdinal, &ts, &pricingTS, &r.source, &r.model,
		&r.providerID, &r.agent, &r.claudeMessageID, &r.claudeRequestID, &r.sourceUUID,
		&r.usageDedupKey,
		&input, &output, &cacheCr, &cacheCr1h, &cacheRd,
		&reasoning, &webSearch, &cost, &r.costSource, &recorded,
	); err != nil {
		return fmt.Errorf("scanning clickhouse activity usage: %w", err)
	}
	r.inputTok, r.outputTok, r.cacheCr, r.cacheCr1h = int(input), int(output), int(cacheCr), int(cacheCr1h)
	r.cacheRd, r.reasoningTok, r.webSearchRequests = int(cacheRd), int(reasoning), int(webSearch)
	if cost != nil {
		r.cost = sql.NullInt64{Int64: *cost, Valid: true}
	}
	if recorded != nil {
		r.recordedCost = sql.NullInt64{Int64: *recorded, Valid: true}
	}
	*out = clickSessionUsageOrderedRow{ordinal: -1}
	if messageOrdinal != nil {
		out.ordinal = *messageOrdinal
		r.messageOrdinal = sql.NullInt64{Int64: out.ordinal, Valid: true}
	}
	if ts != nil {
		out.ts, out.validTS = ts.UTC(), true
		r.ts = out.ts.Format(time.RFC3339Nano)
	}
	switch {
	case pricingTS == nil:
	case out.validTS && pricingTS.Equal(out.ts):
		// Most rows are priced at their own timestamp; share its text.
		r.pricingTS = r.ts
	default:
		r.pricingTS = pricingTS.UTC().Format(time.RFC3339Nano)
	}
	out.scan = r
	return nil
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
	price, record, err := clickActivityUsagePrice(r, pricing)
	if err != nil {
		return money.Money{}, "", false, false, nil, err
	}
	record.apply(pricing, 1)
	return price.cost, price.costSource, price.priced, price.contributes, price.sessionCost, nil
}

// clickActivityUsagePriced is a usage row's price.
type clickActivityUsagePriced struct {
	cost        money.Money
	costSource  export.CostSource
	sessionCost *money.Money
	priced      bool
	contributes bool
}

// clickActivityUsagePrice prices a usage row and returns what pricing it
// records on the resolver, without recording it, so a kept price can record
// it again on a later request's resolver.
func clickActivityUsagePrice(
	r clickActivityReportUsageRow, pricing *export.PricingResolver,
) (clickActivityUsagePriced, usagePricingRecord, error) {
	var price clickActivityUsagePriced
	costRow := r
	unattributed := false
	if r.costSource == db.CopilotReportedCostSource && r.cost.Valid {
		v := money.Money{Microdollars: r.cost.Int64}
		price.sessionCost = &v
		costRow.cost = sql.NullInt64{}
		unattributed = true
	}
	if !costRow.cost.Valid {
		costRow.cost = db.ReportedCostForPricing(costRow.recordedCost,
			usagefacts.CostStatusRecorded,
			chCatalogPricesAt(pricing, costRow.model, costRow.pricingTS))
	}
	cost, priced, contributes, record, err := clickActivityReportRowPrice(costRow, pricing)
	if err != nil {
		return clickActivityUsagePriced{}, usagePricingRecord{}, err
	}
	record.unattributed = unattributed
	price.cost, price.priced, price.contributes = cost, priced, contributes
	price.costSource = export.CostSourceComputed
	if costRow.cost.Valid {
		price.costSource = export.CostSourceReported
	}
	return price, record, nil
}

func clickActivityReportRowStatus(
	r clickActivityReportUsageRow, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, err error) {
	cost, priced, contributes, record, err := clickActivityReportRowPrice(r, pricing)
	if err != nil {
		return money.Money{}, false, false, err
	}
	record.apply(pricing, 1)
	return cost, priced, contributes, nil
}

func clickActivityReportRowPrice(
	r clickActivityReportUsageRow, pricing *export.PricingResolver,
) (cost money.Money, priced, contributes bool, record usagePricingRecord, err error) {
	canonicalModel := chUsageLookupModel(r.model, r.pricingTS)
	if r.cost.Valid {
		pricedModel, lookup := pricing.ResolveAt(
			r.model, canonicalModel, chUsagePricingTimestamp(r.pricingTS),
		)
		record = usagePricingRecord{kind: usagePricingReported, reportedModel: r.model, pricedModel: pricedModel, lookup: lookup}
		return money.Money{Microdollars: r.cost.Int64}, true, true, record, nil
	}
	if !activity.UsageDataContributes(
		false, r.inputTok, r.outputTok, r.reasoningTok,
		r.cacheCr, r.cacheRd, r.webSearchRequests,
	) {
		return money.Money{}, true, false, record, nil
	}
	pricedModel, lookup, err := pricing.ResolveBilledAt(
		r.providerID, r.model, canonicalModel, chUsagePricingTimestamp(r.pricingTS))
	if err != nil {
		return money.Money{}, false, false, record, err
	}
	record = usagePricingRecord{reportedModel: r.model, pricedModel: pricedModel, lookup: lookup}
	if !lookup.OK {
		record.kind = usagePricingUnresolved
		fee, feeErr := export.WebSearchFee(r.webSearchRequests)
		if feeErr != nil {
			return money.Money{}, false, false, usagePricingRecord{}, feeErr
		}
		return fee, false, true, record, nil
	}
	requestScoped := db.UsageSourceIsRequestScoped(r.source) || r.messageOrdinal.Valid
	cost, err = lookup.Rates.CostForTokensScoped(
		requestScoped,
		r.inputTok, r.outputTok, r.reasoningTok, r.cacheCr, r.cacheCr1h, r.cacheRd)
	if err != nil {
		return money.Money{}, false, false, usagePricingRecord{},
			fmt.Errorf("pricing clickhouse activity usage for model %q: %w", r.model, err)
	}
	cost, err = export.AddWebSearchFee(cost, r.webSearchRequests)
	if err != nil {
		return money.Money{}, false, false, usagePricingRecord{},
			fmt.Errorf("pricing clickhouse activity usage for model %q: %w", r.model, err)
	}
	record.kind = usagePricingAggregate
	if requestScoped {
		record.kind = usagePricingRequest
		record.band, record.hasBand = lookup.Rates.RequestBand(r.inputTok, r.cacheCr, r.cacheRd)
	}
	return cost, true, true, record, nil
}

// usagePricingKind is which resolver record a priced usage row makes.
type usagePricingKind uint8

const (
	usagePricingNone usagePricingKind = iota
	usagePricingReported
	usagePricingUnresolved
	usagePricingRequest
	usagePricingAggregate
)

// usagePricingRecord is what pricing one usage row records on the resolver
// whose pricing block the report carries.
type usagePricingRecord struct {
	kind                       usagePricingKind
	reportedModel, pricedModel string
	lookup                     export.PricingLookup
	// band is the request band's input threshold; set only with hasBand.
	band    int
	hasBand bool
	// unattributed marks a session total no model can be credited with.
	unattributed bool
}

// apply records n rows that each made this record. The request and
// aggregate counts add up, and every other record is a flag, so recording
// n rows at once records what recording each of them would.
func (rec usagePricingRecord) apply(pricing *export.PricingResolver, n int) {
	if n == 0 {
		return
	}
	if rec.unattributed {
		pricing.RecordUnattributedReported()
	}
	switch rec.kind {
	case usagePricingNone:
	case usagePricingReported:
		pricing.RecordResolvedReported(rec.reportedModel, rec.pricedModel, rec.lookup)
	case usagePricingUnresolved:
		pricing.RecordResolvedComputed(rec.reportedModel, rec.pricedModel, rec.lookup)
	case usagePricingRequest:
		var band *int
		if rec.hasBand {
			band = &rec.band
		}
		pricing.RecordResolvedCounts(rec.reportedModel, rec.pricedModel, rec.lookup, 0, n, 0, band)
	case usagePricingAggregate:
		pricing.RecordResolvedCounts(rec.reportedModel, rec.pricedModel, rec.lookup, 0, 0, n, nil)
	}
}
