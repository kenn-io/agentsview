package clickhouse

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
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

func (b activityReportBackend) ActivityReportUsage(
	ctx context.Context, ids []string, q activity.Query,
) ([]activity.UsageRow, *export.PricingBlock, error) {
	return b.activityReportUsage(ctx, b.candidates, ids, b.rangeStartUTC, b.rangeEndUTC, q)
}

func (b activityReportBackend) ActivityReportCandidateSource([]string, activity.Query) activity.CandidateSource {
	return b.pairing.candidateSource()
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
	pairs := s.startActivityReportPairs(pairCtx, candidates, ids, versions, q)
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
			delta = append(delta, fmt.Sprintf("%s@%v", row[0], row[23]))
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
// The pairing inputs hold every message of the candidate sessions, so the
// counts need no read of their own.
func (pairing *activityReportPairing) countMessages(
	ctx context.Context, ids []string, q activity.Query, artifacts *activity.CandidateArtifacts,
) error {
	select {
	case <-pairing.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if pairing.err != nil {
		return pairing.err
	}
	counts := activity.NewMessageAccumulator(q, artifacts)
	for _, id := range ids {
		for _, m := range pairing.inputs[id].messages {
			if m.counted && m.stamped() && !m.at().Before(q.RangeStart) {
				counts.Add(id, m.role.Value(), m.at())
			}
		}
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
const chActivityCandidateDigestSQL = "toString(sum(sipHash64(s.id, s.push_version))) || ':' || " +
	"toString(sum(cityHash64(s.id, s.push_version))), count()"

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

// activityReportPairing holds pairing started ahead of its consumer.
type activityReportPairing struct {
	done     chan struct{}
	paired   []activity.IntervalCandidate
	terminal []activity.IntervalCandidate
	inputs   map[string]activitySessionInputs
	err      error
}

// startActivityReportPairs runs activityReportPairs in the background for
// the sessions selected by `candidates`. The set is evaluated inside each
// statement; `ids` is the session list the caller already loaded, and
// candidates for any session outside it are dropped so the stream matches
// the metadata the aggregator was given even if a push lands between the
// two queries.
func (s *Store) startActivityReportPairs(
	ctx context.Context, candidates chSessionSet, ids []string, versions map[string]uint64, q activity.Query,
) *activityReportPairing {
	pairing := &activityReportPairing{done: make(chan struct{})}
	if len(ids) == 0 {
		close(pairing.done)
		return pairing
	}
	go func() {
		defer close(pairing.done)
		pairing.paired, pairing.terminal, pairing.inputs, pairing.err = s.activityReportPairs(ctx, candidates, ids, versions, q)
	}()
	return pairing
}

// candidateSource streams the paired intervals once pairing completes.
func (pairing *activityReportPairing) candidateSource() activity.CandidateSource {
	return func(
		ctx context.Context,
		yield func(activity.IntervalCandidate) error,
	) error {
		select {
		case <-pairing.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if pairing.err != nil {
			return pairing.err
		}
		paired, terminal := pairing.paired, pairing.terminal
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
	return func(ctx context.Context, yield func(activity.IntervalCandidate) error) error {
		return s.startActivityReportPairs(ctx, chSessionSetFromIDs(ids), ids, nil, q).candidateSource()(ctx, yield)
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
		return activity.UsagePrice{
			Cost: cost, CostSource: costSource, SessionCost: sessionCost,
			Priced: priced, Contributes: contributes,
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

	state, err := s.preparedUsageState(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Rows are visited in report order: time, then session, then ordinal.
	var ordered iter.Seq[*clickActivityReportUsageRow]
	var count int
	if state.ready && q.Partial {
		// Every push changes a range in progress, so its rows are kept for
		// every session and a push re-reads only the sessions it changed.
		kept, err := s.activityUsageRange(ctx, state, lowerBound, upperBound)
		if err != nil {
			return nil, nil, err
		}
		ordered, count = activityUsageSelection(kept, ids)
	} else {
		// Raw rows depend on the usage source, the candidate set, and the
		// range; the same key between pushes serves the kept rows unchanged.
		// An ended range's report is kept whole once prepared rows back it,
		// and any change to these rows also changes the report's key, so its
		// rows would never be read again and are not kept.
		var memoSlot, memoVersion string
		if !state.ready {
			fingerprint, err := s.partsFingerprint(ctx)
			if err != nil {
				return nil, nil, err
			}
			memoSlot = fmt.Sprintf("%s|%s|%s", lowerBound, upperBound, chSessionIDsDigest(ids))
			memoVersion = fingerprint
		}
		if kept, ok := s.activityUsageRows.Get(memoSlot, memoVersion); ok {
			ordered, count = kept[0].all(), kept[0].count
		} else {
			query, args := clickActivityReportUsageQuery(candidates, lowerBound, upperBound)
			if state.ready {
				query, args = clickPreparedActivityUsageQuery(state, candidates, lowerBound, upperBound)
			}
			readCtx, err := withUsageDeltaTables(ctx, state)
			if err != nil {
				return nil, nil, err
			}
			s.activityUsageQueries.Add(1)
			kept, err := s.readActivityUsageKept(readCtx, query, args)
			if err != nil {
				return nil, nil, err
			}
			if memoSlot != "" {
				s.activityUsageRows.Put(memoSlot, memoVersion, []*activityUsageKept{kept})
			}
			ordered, count = kept.all(), kept.count
		}
	}

	baseRows := make([]activity.UsageRow, 0, count)
	for r := range ordered {
		baseRows = append(baseRows, activity.UsageRow{
			SessionID:         r.sessionID,
			Model:             r.model,
			Timestamp:         r.ts,
			InputTokens:       r.inputTok,
			OutputTokens:      r.outputTok,
			WebSearchRequests: r.webSearchRequests,
			Agent:             r.agent,
			ClaudeMessageID:   r.claudeMessageID,
			ClaudeRequestID:   r.claudeRequestID,
			SourceUUID:        r.sourceUUID,
			UsageDedupKey:     r.usageDedupKey,
		})
	}
	mask, attribution, webSearchRequests, err := activity.UsageSurvivorSelectionForSessions(
		ctx, q.RangeStart, q.RangeEnd, q.EffectiveEnd, baseRows, ids,
	)
	if err != nil {
		return nil, nil, err
	}
	out = make([]activity.UsageRow, 0, count)
	i := -1
	for r := range ordered {
		i++
		if !mask[i] {
			continue
		}
		costRow := *r
		costRow.webSearchRequests = webSearchRequests[i]
		cost, costSource, priced, contributes, sessionCost, priceErr := clickActivityUsageCost(costRow, rateResolver)
		if priceErr != nil {
			return nil, nil, priceErr
		}
		out = append(out, activity.UsageRow{
			SessionID:         attribution[i],
			Model:             costRow.model,
			Timestamp:         costRow.ts,
			InputTokens:       costRow.inputTok,
			OutputTokens:      costRow.outputTok,
			WebSearchRequests: webSearchRequests[i],
			Cost:              cost,
			CostSource:        costSource,
			SessionCost:       sessionCost,
			Priced:            priced,
			Contributes:       contributes,
			Agent:             costRow.agent,
			ClaudeMessageID:   costRow.claudeMessageID,
			ClaudeRequestID:   costRow.claudeRequestID,
			SourceUUID:        costRow.sourceUUID,
			UsageDedupKey:     costRow.usageDedupKey,
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

// chPreparedActivityRangeSQL bounds prepared rows to a range. The sort
// column agrees with ts for every stored timestamp and is the epoch
// otherwise, so bounding both reads only the range's granules while keeping
// the null-timestamp exclusion of the ts predicate.
func chPreparedActivityRangeSQL(lowerBound, upperBound string) (string, []any) {
	return chPreparedUsageKeyColumn + " >= " + chTimestampSQL + " AND " + chPreparedUsageKeyColumn + " <= " + chTimestampSQL +
			" AND ts >= " + chTimestampSQL + " AND ts <= " + chTimestampSQL,
		[]any{lowerBound, upperBound, lowerBound, upperBound}
}

func clickPreparedActivityUsageQuery(
	state preparedUsageState, candidates chSessionSet, lowerBound, upperBound string,
) (string, []any) {
	rangeSQL, rangeArgs := chPreparedActivityRangeSQL(lowerBound, upperBound)
	sourceSQL, sourceArgs := chPreparedUsageSourceSQL(state, rangeSQL, rangeArgs)
	query := `WITH candidate_sessions AS (` + candidates.body + `), prepared_rows AS (` + sourceSQL + `),
		candidate_keys AS (
		SELECT DISTINCT claude_message_id,claude_request_id FROM prepared_rows
		WHERE session_id IN (SELECT id FROM candidate_sessions)
		AND claude_message_id != '' AND claude_request_id != '')
		SELECT ` + chPreparedUsageColumns + ` FROM prepared_rows
		WHERE session_id IN (SELECT id FROM candidate_sessions)
		OR (source='message' AND (claude_message_id,claude_request_id) IN (SELECT * FROM candidate_keys))`
	args := slices.Clone(candidates.args)
	args = append(args, sourceArgs...)
	return query, args
}

// activityUsageRange holds every session's prepared usage rows in a range,
// in report order, with the snapshot revisions they were read at.
type activityUsageRange struct {
	revisions map[string]string
	rows      *activityUsageKept
}

// activityUsageRangeLimit caps the ranges whose rows are kept. Only ranges
// still in progress keep them: today, this week, this month, and the few
// custom ranges a client has open.
const activityUsageRangeLimit = 8

// activityUsageRereadSessions is how many changed sessions a read of a kept
// range always re-reads instead of reading the whole range.
const activityUsageRereadSessions = 64

// activityUsageRange returns every session's prepared usage rows in the
// range, in report order. The rows are kept with the snapshot revisions
// they were read at. A session's prepared rows are derived from its current
// snapshot alone, so a later read re-reads only the sessions whose snapshot
// revision changed since and merges them in; sessions that are gone drop
// out. When most sessions changed, one read of the range is cheaper.
func (s *Store) activityUsageRange(
	ctx context.Context, state preparedUsageState, lowerBound, upperBound string,
) (*activityUsageKept, error) {
	slot := lowerBound + "|" + upperBound
	kept, ok := s.activityUsageRanges.Get(slot, "")
	incremental := ok && len(kept) > 0
	var prev activityUsageRange
	var changed []string
	if incremental {
		prev = kept[0]
		changed = changedSnapshotSessions(prev.revisions, state.revisions)
		if len(changed) == 0 {
			return prev.rows, nil
		}
		incremental = len(changed) <= max(activityUsageRereadSessions, len(state.revisions)/4)
	}
	where, args := chPreparedActivityRangeSQL(lowerBound, upperBound)
	readCtx, err := withUsageDeltaTables(ctx, state)
	if err != nil {
		return nil, err
	}
	if incremental {
		where += " AND session_id IN (SELECT id FROM activity_usage_changed)"
		table, err := usageSessionListTable("activity_usage_changed", changed)
		if err != nil {
			return nil, err
		}
		readCtx = chdriver.Context(readCtx, chdriver.WithExternalTable(table))
	}
	sourceSQL, sourceArgs := chPreparedUsageSourceSQL(state, where, args)
	s.activityUsageQueries.Add(1)
	rows, err := s.readActivityUsageKept(readCtx, "SELECT "+chPreparedUsageColumns+" FROM ("+sourceSQL+")", sourceArgs)
	if err != nil {
		return nil, err
	}
	if incremental {
		replaced := make(map[string]bool, len(changed))
		for _, id := range changed {
			replaced[id] = true
		}
		rows = mergeActivityUsage(prev.rows, replaced, rows)
	}
	// Only ranges in progress read this memo, so a range that has ended is
	// never read again.
	now := time.Now()
	s.activityUsageRanges.DeleteSlots(func(slot string) bool {
		_, upper, _ := strings.Cut(slot, "|")
		end, err := time.Parse(time.RFC3339Nano, upper)
		return err == nil && end.Before(now)
	})
	s.activityUsageRanges.Put(slot, "", []activityUsageRange{{revisions: state.revisions, rows: rows}})
	return rows, nil
}

// changedSnapshotSessions lists the sessions whose snapshot revision differs
// between before and after, including sessions only one of them has.
func changedSnapshotSessions(before, after map[string]string) []string {
	var changed []string
	for id, revision := range after {
		if prior, ok := before[id]; !ok || prior != revision {
			changed = append(changed, id)
		}
	}
	for id := range before {
		if _, ok := after[id]; !ok {
			changed = append(changed, id)
		}
	}
	return changed
}

// A push writes a session's messages before it publishes the session row, and
// an interrupted push may never publish it. Accept stored usage at or above the
// published version so that window shows the newer rows instead of no usage.
// Rows a shorter republished session left behind stay below it and are skipped.
const chUsageMessageCurrent = "m.push_version >= s.push_version"

// chStoredMessageUsable renders the condition a stored message meets when
// its usage counts, from the expressions of its usage_present and model
// fields. Row filters pass a message's columns; the rows-behind count and the
// snapshot prepares_usage column pass the fields of usage_facts elements.
func chStoredMessageUsable(present, model string) string {
	return present + ` != 0
			AND ` + model + ` != ''
			AND ` + model + ` != '<synthetic>'`
}

// chUsageStoredMessageEligibility is chUsageMessageEligibility for reads of
// usage_messages, which stores a presence flag instead of the raw JSON.
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
		"usage_messages m JOIN sessions s ON s.id = m.session_id",
		"usage_events ue JOIN sessions s ON s.id = ue.session_id", chUsageMessageCurrent, "")
}

// clickUsageNormalizedQueryFrom renders the normalized usage rows read from
// the message and event sources. A non-empty revision expression is carried
// on every row as snapshot_revision, naming the session snapshot the row was
// derived from.
func clickUsageNormalizedQueryFrom(ctes, messageWhere, eventWhere, messageFrom, eventFrom, currentVersion, revision string) string {
	revisionColumn, revisionSelect := "", ""
	if revision != "" {
		revisionColumn = ",\n\t\t\t\t" + revision + " AS snapshot_revision"
		revisionSelect = ", snapshot_revision"
	}
	return fmt.Sprintf(`
		WITH %[3]susage_raw AS (
			SELECT s.id AS session_id,
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
				toInt64(0) AS usage_web,
				s.agent AS agent,
				CAST('' AS String) AS claude_message_id,
				CAST('' AS String) AS claude_request_id,
				CAST('' AS String) AS source_uuid,
				if(ue.dedup_key != '',
					concat(s.id, ':', ue.source, ':', ue.dedup_key),
					concat(s.id, ':', ue.source, ':id:', toString(ue.id))) AS usage_dedup_key,
				toInt64(ue.input_tokens) AS input_tokens,
				toInt64(ue.output_tokens) AS output_tokens,
				toInt64(ue.cache_creation_input_tokens) AS cache_create,
				toInt64(ue.cache_read_input_tokens) AS cache_read,
				toInt64(ue.reasoning_tokens) AS reasoning_tokens,
				ue.cost_microdollars AS cost_microdollars,
				ue.cost_source AS cost_source,
				COALESCE(ue.occurred_at, s.started_at) AS ts_raw,
				s.started_at AS started_at_raw%[7]s
			FROM %[6]s
			WHERE %[2]s
		)`,
		messageWhere, eventWhere, ctes, currentVersion, messageFrom, eventFrom, revisionColumn,
	) + " SELECT " + clickUsageNormalizedColumns() + revisionSelect + " FROM usage_raw"
}

func clickUsageNormalizedColumns() string {
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
	return fmt.Sprintf(`session_id, message_ordinal, ts, pricing_ts, source, model,
			provider_id, agent, claude_message_id, claude_request_id, source_uuid,
			usage_dedup_key,
			toInt64(CASE
				WHEN source = 'message' THEN %[1]s
				WHEN source = 'session' THEN greatest(input_tokens, toInt64(0))
				ELSE %[6]s
			END) AS input_tokens_norm,
			toInt64(CASE
				WHEN source = 'message' THEN %[2]s
				WHEN source = 'session' THEN greatest(output_tokens, toInt64(0))
				ELSE %[7]s
			END) AS output_tokens_norm,
			toInt64(CASE
				WHEN source = 'message' THEN %[3]s
				WHEN source = 'session' THEN greatest(cache_create, toInt64(0))
				ELSE %[8]s
			END) AS cache_create_norm,
			toInt64(CASE
				WHEN source = 'message' THEN %[4]s
				ELSE toInt64(0)
			END) AS cache_create_1h_norm,
			toInt64(CASE
				WHEN source = 'message' THEN %[5]s
				WHEN source = 'session' THEN greatest(cache_read, toInt64(0))
				ELSE %[9]s
			END) AS cache_read_norm,
			toInt64(CASE
				WHEN source = 'message' THEN %[10]s
				WHEN source = 'session' THEN greatest(reasoning_tokens, toInt64(0))
				ELSE %[11]s
			END) AS reasoning_tokens_norm,
			toInt64(CASE
				WHEN source = 'message' THEN %[12]s
				ELSE toInt64(0)
			END) AS web_search_requests_norm,
			cost_microdollars, cost_source`,
		msgInput, msgOutput, msgCacheCr, msgCacheCr1h, msgCacheRd,
		clamp("input_tokens"), clamp("output_tokens"), clamp("cache_create"),
		clamp("cache_read"), msgReasoning, clamp("reasoning_tokens"), msgWeb,
	)
}

func (s *Store) scanActivityUsageRows(
	ctx context.Context, query string, args []any,
) ([]clickSessionUsageOrderedRow, error) {
	rows, err := s.bulk.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse activity usage: %w", err)
	}
	defer rows.Close()
	// Non-nil even when empty: the caller indexes the result through a
	// sorted index slice, and NilAway cannot see that an empty result yields
	// no indexes.
	out := make([]clickSessionUsageOrderedRow, 0)
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
	s.activityUsageRowsRead.Add(int64(len(out)))
	return out, nil
}

// readActivityUsageKept reads the usage rows query selects, in the columns
// of chPreparedUsageColumns, and keeps them in report order. A full-history
// read returns over a million rows, so it runs on the native connection
// (see openBulk) and packs each row as it arrives: a slice of a million
// wide rows is seconds of garbage collection and cache misses to sort.
func (s *Store) readActivityUsageKept(
	ctx context.Context, query string, args []any,
) (*activityUsageKept, error) {
	rows, err := s.bulk.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse activity usage: %w", err)
	}
	defer rows.Close()
	sorter := newKeptUsageSorter()
	var r clickSessionUsageOrderedRow
	for rows.Next() {
		if err := scanActivityUsageRow(rows, &r); err != nil {
			return nil, err
		}
		sorter.add(&r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse activity usage: %w", err)
	}
	kept := sorter.finish()
	s.activityUsageRowsRead.Add(int64(kept.count))
	return kept, nil
}

// scanActivityUsageRow scans the current row, in the columns of
// chPreparedUsageColumns, into out. The destinations match the column
// types exactly, which the native API requires.
func scanActivityUsageRow(rows chnative.Rows, out *clickSessionUsageOrderedRow) error {
	var r clickActivityReportUsageRow
	var messageOrdinal, cost *int64
	var ts, pricingTS *time.Time
	var input, output, cacheCr, cacheCr1h, cacheRd, reasoning, webSearch int64
	if err := rows.Scan(
		&r.sessionID, &messageOrdinal, &ts, &pricingTS, &r.source, &r.model,
		&r.providerID, &r.agent, &r.claudeMessageID, &r.claudeRequestID, &r.sourceUUID,
		&r.usageDedupKey,
		&input, &output, &cacheCr, &cacheCr1h, &cacheRd,
		&reasoning, &webSearch, &cost, &r.costSource,
	); err != nil {
		return fmt.Errorf("scanning clickhouse activity usage: %w", err)
	}
	r.inputTok, r.outputTok, r.cacheCr, r.cacheCr1h = int(input), int(output), int(cacheCr), int(cacheCr1h)
	r.cacheRd, r.reasoningTok, r.webSearchRequests = int(cacheRd), int(reasoning), int(webSearch)
	if cost != nil {
		r.cost = sql.NullInt64{Int64: *cost, Valid: true}
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

// activityUsageKept is one activity usage read kept for reuse. A scanned row
// takes several hundred bytes, most of them string headers, repeated session
// IDs, and zero token counts. Kept rows are encoded in report order as
// varints: repeated strings refer to a dictionary, and every other string is
// stored once in one arena and read back without copying. A row's timestamp
// is also stored as microseconds, a delta from the previous row's, so rows
// merge in report order without parsing text.
type activityUsageKept struct {
	dict  []string
	arena string
	data  []byte
	count int
	// sessions are the dictionary indexes of the sessions with kept rows.
	sessions []uint64
}

const (
	keptOrdinalValid = 1 << iota
	keptCostValid
	keptPricingIsTS
	keptTSValid
)

// keptUsageRefs are the dictionary indexes of a row's session, source,
// model, provider, agent, and cost source.
type keptUsageRefs [6]uint64

// keptUsageRow is one decoded kept row.
type keptUsageRow struct {
	clickActivityReportUsageRow
	refs keptUsageRefs
	// us is the timestamp in microseconds; it is set only when validTS.
	us      int64
	validTS bool
}

// keptUsageEncoder appends rows to a new activityUsageKept.
type keptUsageEncoder struct {
	k   *activityUsageKept
	ids map[string]uint64
	// session marks the dictionary indexes already in k.sessions.
	session []bool
	// last holds each field's previous string and index; most rows repeat
	// the previous row's value, which saves a map lookup.
	last [6]struct {
		s   string
		id  uint64
		set bool
	}
	arena strings.Builder
	data  []byte
	us    int64
}

// newKeptUsageEncoder starts an encoding whose dictionary begins with dict,
// so rows decoded from the kept read that owns dict keep their references.
// arenaSize and rows size the buffers: a growing builder keeps up to twice
// the bytes, and a row takes a few dozen bytes of data.
func newKeptUsageEncoder(dict []string, arenaSize, rows int) *keptUsageEncoder {
	e := &keptUsageEncoder{
		k:       &activityUsageKept{dict: slices.Clone(dict)},
		ids:     make(map[string]uint64, len(dict)),
		session: make([]bool, len(dict)),
		data:    make([]byte, 0, 48*rows),
	}
	for i, v := range dict {
		e.ids[v] = uint64(i)
	}
	e.arena.Grow(arenaSize)
	return e
}

func (e *keptUsageEncoder) ref(field int, v string) uint64 {
	if l := &e.last[field]; l.set && l.s == v {
		return l.id
	}
	id, ok := e.ids[v]
	if !ok {
		id = uint64(len(e.k.dict))
		e.ids[v] = id
		e.k.dict = append(e.k.dict, v)
	}
	e.last[field].s, e.last[field].id, e.last[field].set = v, id, true
	return id
}

// add appends a row and returns its session's dictionary index. us is the
// timestamp in microseconds and is read only when validTS.
func (e *keptUsageEncoder) add(r *clickActivityReportUsageRow, us int64, validTS bool) uint64 {
	refs := keptUsageRefs{
		e.ref(0, r.sessionID), e.ref(1, r.source), e.ref(2, r.model),
		e.ref(3, r.providerID), e.ref(4, r.agent), e.ref(5, r.costSource),
	}
	e.write(r, refs, us, validTS)
	return refs[0]
}

// addKept appends a row decoded from the kept read whose dictionary this
// encoding started with.
func (e *keptUsageEncoder) addKept(r *keptUsageRow) {
	e.write(&r.clickActivityReportUsageRow, r.refs, r.us, r.validTS)
}

func (e *keptUsageEncoder) write(r *clickActivityReportUsageRow, refs keptUsageRefs, us int64, validTS bool) {
	var flags byte
	if r.messageOrdinal.Valid {
		flags |= keptOrdinalValid
	}
	if r.cost.Valid {
		flags |= keptCostValid
	}
	if r.pricingTS == r.ts {
		flags |= keptPricingIsTS
	}
	if validTS {
		flags |= keptTSValid
	}
	e.data = append(e.data, flags)
	data := e.data
	for _, id := range refs {
		data = binary.AppendUvarint(data, id)
	}
	if validTS {
		data = binary.AppendVarint(data, us-e.us)
		e.us = us
	}
	inline := func(v string) {
		data = binary.AppendUvarint(data, uint64(len(v)))
		e.arena.WriteString(v)
	}
	inline(r.ts)
	if flags&keptPricingIsTS == 0 {
		inline(r.pricingTS)
	}
	inline(r.claudeMessageID)
	inline(r.claudeRequestID)
	inline(r.sourceUUID)
	inline(r.usageDedupKey)
	if r.messageOrdinal.Valid {
		data = binary.AppendVarint(data, r.messageOrdinal.Int64)
	}
	for _, v := range [...]int{r.inputTok, r.outputTok, r.cacheCr, r.cacheCr1h, r.cacheRd, r.reasoningTok, r.webSearchRequests} {
		data = binary.AppendVarint(data, int64(v))
	}
	if r.cost.Valid {
		data = binary.AppendVarint(data, r.cost.Int64)
	}
	e.data = data
	if session := refs[0]; session >= uint64(len(e.session)) || !e.session[session] {
		if grow := int(session) + 1 - len(e.session); grow > 0 {
			e.session = append(e.session, make([]bool, grow)...)
		}
		e.session[session] = true
		e.k.sessions = append(e.k.sessions, session)
	}
	e.k.count++
}

func (e *keptUsageEncoder) finish() *activityUsageKept {
	e.k.arena = e.arena.String()
	e.k.data = append([]byte(nil), e.data...)
	return e.k
}

// rows decodes the kept rows in report order. Each row is decoded into the
// same value, so the pointer is valid only until the next row.
func (k *activityUsageKept) rows() iter.Seq[*keptUsageRow] {
	return func(yield func(*keptUsageRow) bool) {
		data, arena := k.data, k.arena
		var r keptUsageRow
		var us int64
		for range k.count {
			data, arena = k.decode(data, arena, us, &r)
			if r.validTS {
				us = r.us
			}
			if !yield(&r) {
				return
			}
		}
	}
}

// decode decodes the row data and arena start with into r and returns what
// follows it. A row stores its timestamp as a delta from prevUS, the
// timestamp of the last earlier row that has one.
func (k *activityUsageKept) decode(data []byte, arena string, prevUS int64, r *keptUsageRow) ([]byte, string) {
	uvarint := func() uint64 {
		v, n := binary.Uvarint(data)
		data = data[n:]
		return v
	}
	varint := func() int64 {
		v, n := binary.Varint(data)
		data = data[n:]
		return v
	}
	str := func() string {
		n := uvarint()
		v := arena[:n]
		arena = arena[n:]
		return v
	}
	flags := data[0]
	data = data[1:]
	*r = keptUsageRow{}
	for i := range r.refs {
		r.refs[i] = uvarint()
	}
	r.sessionID = k.dict[r.refs[0]]
	r.source = k.dict[r.refs[1]]
	r.model = k.dict[r.refs[2]]
	r.providerID = k.dict[r.refs[3]]
	r.agent = k.dict[r.refs[4]]
	r.costSource = k.dict[r.refs[5]]
	if flags&keptTSValid != 0 {
		r.us, r.validTS = prevUS+varint(), true
	}
	r.ts = str()
	r.pricingTS = r.ts
	if flags&keptPricingIsTS == 0 {
		r.pricingTS = str()
	}
	r.claudeMessageID = str()
	r.claudeRequestID = str()
	r.sourceUUID = str()
	r.usageDedupKey = str()
	if flags&keptOrdinalValid != 0 {
		r.messageOrdinal.Int64, r.messageOrdinal.Valid = varint(), true
	}
	r.inputTok = int(varint())
	r.outputTok = int(varint())
	r.cacheCr = int(varint())
	r.cacheCr1h = int(varint())
	r.cacheRd = int(varint())
	r.reasoningTok = int(varint())
	r.webSearchRequests = int(varint())
	if flags&keptCostValid != 0 {
		r.cost.Int64, r.cost.Valid = varint(), true
	}
	return data, arena
}

// all decodes the kept rows in report order. Each row is decoded into the
// same value, so the pointer is valid only until the next row.
func (k *activityUsageKept) all() iter.Seq[*clickActivityReportUsageRow] {
	return func(yield func(*clickActivityReportUsageRow) bool) {
		for r := range k.rows() {
			if !yield(&r.clickActivityReportUsageRow) {
				return
			}
		}
	}
}

// keptUsageKey is where a row sits in an arrival-order encoding and what
// orders it: time, then session, then ordinal, then arrival.
type keptUsageKey struct {
	us, ordinal int64
	// data and arena are the row's offsets in the arrival encoding.
	data, arena int
	session     uint64
	validTS     bool
}

// keptUsageSorter packs rows as they arrive and keeps them in report order:
// time, then session, then ordinal, with rows that tie in arrival order.
// Sorting small keys and copying each packed row once costs a fraction of
// sorting the wide scanned rows.
type keptUsageSorter struct {
	e    *keptUsageEncoder
	keys []keptUsageKey
}

func newKeptUsageSorter() *keptUsageSorter {
	return &keptUsageSorter{e: newKeptUsageEncoder(nil, 0, 0)}
}

func (s *keptUsageSorter) add(r *clickSessionUsageOrderedRow) {
	key := keptUsageKey{
		us: r.ts.UnixMicro(), ordinal: r.ordinal, validTS: r.validTS,
		data: len(s.e.data), arena: s.e.arena.Len(),
	}
	key.session = s.e.add(&r.scan, key.us, r.validTS)
	s.keys = append(s.keys, key)
}

func (s *keptUsageSorter) finish() *activityUsageKept {
	arrival := &activityUsageKept{dict: s.e.k.dict, arena: s.e.arena.String(), data: s.e.data}
	dict := arrival.dict
	slices.SortFunc(s.keys, func(a, b keptUsageKey) int {
		if a.validTS && b.validTS && a.us != b.us {
			return cmp.Compare(a.us, b.us)
		}
		if a.session != b.session {
			if c := strings.Compare(dict[a.session], dict[b.session]); c != 0 {
				return c
			}
		}
		if c := cmp.Compare(a.ordinal, b.ordinal); c != 0 {
			return c
		}
		return cmp.Compare(a.data, b.data)
	})
	e := newKeptUsageEncoder(dict, len(arrival.arena), len(s.keys))
	var r keptUsageRow
	for _, key := range s.keys {
		arrival.decode(arrival.data[key.data:], arrival.arena[key.arena:], 0, &r)
		r.us = key.us
		e.addKept(&r)
	}
	return e.finish()
}

// keptUsageBefore reports whether a comes before b in report order.
func keptUsageBefore(a, b *keptUsageRow) bool {
	if a.validTS && b.validTS && a.us != b.us {
		return a.us < b.us
	}
	if a.sessionID != b.sessionID {
		return a.sessionID < b.sessionID
	}
	return db.UsageRowMessageOrdinal(a.messageOrdinal) < db.UsageRowMessageOrdinal(b.messageOrdinal)
}

// mergeActivityUsage returns prev without the rows of the sessions in
// replaced, merged in report order with fresh, the replaced sessions'
// current rows.
func mergeActivityUsage(prev *activityUsageKept, replaced map[string]bool, fresh *activityUsageKept) *activityUsageKept {
	e := newKeptUsageEncoder(prev.dict, len(prev.arena)+len(fresh.arena), prev.count+fresh.count)
	dropped := make([]bool, len(prev.dict))
	for _, session := range prev.sessions {
		dropped[session] = replaced[prev.dict[session]]
	}
	// Each fresh row's strings point into fresh's arena and dictionary, so
	// copying the decoded value keeps it valid.
	pending := make([]keptUsageRow, 0, fresh.count)
	for r := range fresh.rows() {
		pending = append(pending, *r)
	}
	next := 0
	for r := range prev.rows() {
		if dropped[r.refs[0]] {
			continue
		}
		for ; next < len(pending) && keptUsageBefore(&pending[next], r); next++ {
			e.add(&pending[next].clickActivityReportUsageRow, pending[next].us, pending[next].validTS)
		}
		e.addKept(r)
	}
	for ; next < len(pending); next++ {
		e.add(&pending[next].clickActivityReportUsageRow, pending[next].us, pending[next].validTS)
	}
	return e.finish()
}

// claudeUsageKey is a Claude message and request ID pair.
type claudeUsageKey struct{ message, request string }

// activityUsageSelection returns the kept rows a read of the candidate
// sessions returns, in report order, and how many there are: every row of
// a candidate session, and every message row of another session that
// carries the Claude message and request IDs of a candidate row. Those
// peers are what complete-snapshot selection compares the candidates with.
func activityUsageSelection(k *activityUsageKept, candidates []string) (iter.Seq[*clickActivityReportUsageRow], int) {
	selected := make(map[string]bool, len(candidates))
	for _, id := range candidates {
		selected[id] = true
	}
	candidate := make([]bool, len(k.dict))
	others := false
	for _, session := range k.sessions {
		candidate[session] = selected[k.dict[session]]
		others = others || !candidate[session]
	}
	if !others {
		return k.all(), k.count
	}
	peerKey := func(r *keptUsageRow) (claudeUsageKey, bool) {
		return claudeUsageKey{r.claudeMessageID, r.claudeRequestID}, r.claudeMessageID != "" && r.claudeRequestID != ""
	}
	// The other sessions' message rows are grouped by key first; a
	// candidate row carrying the key then selects the whole group.
	type peerGroup struct {
		rows     int
		selected bool
	}
	peers := map[claudeUsageKey]*peerGroup{}
	count := 0
	for r := range k.rows() {
		if candidate[r.refs[0]] {
			count++
			continue
		}
		if key, ok := peerKey(r); ok && r.source == "message" {
			group := peers[key]
			if group == nil {
				group = &peerGroup{}
				peers[key] = group
			}
			group.rows++
		}
	}
	if len(peers) > 0 {
		for r := range k.rows() {
			if !candidate[r.refs[0]] {
				continue
			}
			if key, ok := peerKey(r); ok {
				if group := peers[key]; group != nil && !group.selected {
					group.selected = true
					count += group.rows
				}
			}
		}
	}
	return func(yield func(*clickActivityReportUsageRow) bool) {
		for r := range k.rows() {
			if !candidate[r.refs[0]] {
				key, ok := peerKey(r)
				if !ok || r.source != "message" {
					continue
				}
				if group := peers[key]; group == nil || !group.selected {
					continue
				}
			}
			if !yield(&r.clickActivityReportUsageRow) {
				return
			}
		}
	}, count
}
