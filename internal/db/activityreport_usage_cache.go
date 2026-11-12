package db

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"golang.org/x/sync/errgroup"
)

// activityUsageWindow is every live session with usage inside an Activity
// report's padded usage bounds, captured from the archive after the usage
// cache brought their facts up to date.
type activityUsageWindow struct {
	databaseID string
	sessions   map[string]usageQuerySession
}

// prepareActivityUsageWindow captures and fills the sessions with usage
// between lowerBound and upperBound. The fill takes its own archive reads, so
// the caller must not hold a bulk read.
func (db *DB) prepareActivityUsageWindow(
	ctx context.Context, lowerBound, upperBound string,
) (activityUsageWindow, error) {
	// Discovery pads these whole UTC days further, so it finds every session
	// with a usage row whose timestamp text lies inside the bounds.
	snapshot, err := db.captureUsageQuery(ctx, UsageFilter{
		From:     lowerBound[:len(time.DateOnly)],
		To:       upperBound[:len(time.DateOnly)],
		Timezone: "UTC",
	}, usageQueryKindToken)
	if err != nil {
		return activityUsageWindow{}, err
	}
	snapshot.CursorHighWater = 0 // Activity has no Cursor usage.
	cache, release, err := db.usageCache.acquireGeneration(ctx, snapshot.DatabaseID)
	if err != nil {
		return activityUsageWindow{}, err
	}
	defer release()
	fills, err := cache.fillUsageWindow(ctx, &snapshot)
	if err != nil {
		return activityUsageWindow{}, err
	}
	snapshot.dropDeleted(fills)
	window := activityUsageWindow{
		databaseID: snapshot.DatabaseID,
		sessions:   make(map[string]usageQuerySession, len(snapshot.Sessions)),
	}
	for _, session := range snapshot.Sessions {
		window.sessions[session.ID] = session
	}
	return window, nil
}

// activityUsageFact is one cached usage fact with the session fields and
// parsed instant that Activity's survivor order needs.
type activityUsageFact struct {
	row           activityUsageFactRow
	tokens        activityReportRowTokens
	reported      sql.NullInt64
	costStatus    string
	costSource    string
	pricingTS     string
	requestScoped bool
	validTS       bool
	millis, nanos int64
}

type activityUsageFactRow struct {
	SessionID, UsageSource, Model, ProviderID                   string
	ClaudeMessageID, ClaudeRequestID, SourceUUID, UsageDedupKey string
	Timestamp, Project, Machine, Agent                          string
	MessageOrdinal                                              int64
	WebSearchRequests                                           int
}

const activityUsageFactColumns = `cs.session_id, f.source, f.message_ordinal,
	f.raw_timestamp, f.timestamp_ms, f.timestamp_ns,
	f.model, f.provider_id, f.input_tokens, f.output_tokens, f.reasoning_tokens,
	f.cache_creation_tokens, f.cache_creation_1h_tokens, f.cache_read_tokens,
	f.web_search_requests, f.reported_cost_microdollars, f.cost_status, f.cost_source,
	f.request_scoped, f.claude_message_id, f.claude_request_id, f.source_uuid,
	f.usage_dedup_key`

// activityUsageCandidateFactsSQL reads the candidate sessions' token facts.
// Rows that use the session start are bounded in Go.
const activityUsageCandidateFactsSQL = `SELECT ` + activityUsageFactColumns + `
	FROM json_each(?1) ids
	CROSS JOIN usage_cached_sessions cs ON cs.session_id = ids.value
	CROSS JOIN usage_facts f ON f.cached_session_id = cs.id
	WHERE f.token_eligible AND (f.uses_session_start OR f.raw_timestamp BETWEEN ?2 AND ?3)`

// activityUsagePeerFactsSQL reads other sessions' token facts that share a
// Claude message and request pair with a candidate fact. Go keeps only pairs
// whose candidate fact lies inside the bounds.
const activityUsagePeerFactsSQL = `SELECT DISTINCT ` + activityUsageFactColumns + `
	FROM json_each(?1) ids
	CROSS JOIN usage_cached_sessions candidate ON candidate.session_id = ids.value
	CROSS JOIN usage_facts c ON c.cached_session_id = candidate.id
	CROSS JOIN usage_facts f ON f.claude_message_id = c.claude_message_id
		AND f.claude_request_id = c.claude_request_id
	CROSS JOIN usage_cached_sessions cs ON cs.id = f.cached_session_id
	WHERE c.claude_message_id != '' AND c.token_eligible
		AND f.claude_message_id != ''
		AND f.cached_session_id != c.cached_session_id
		AND f.token_eligible AND f.source = 'message'
		AND cs.session_id NOT IN (SELECT value FROM json_each(?1))
		AND (f.uses_session_start OR f.raw_timestamp BETWEEN ?2 AND ?3)`

// activityReportCachedUsage is activityReportUsage read from the usage cache:
// the same rows, survivor selection and pricing, without re-reading and
// re-parsing raw message usage.
func (db *DB) activityReportCachedUsage(
	ctx context.Context, window activityUsageWindow, ids []string,
	lowerBound, upperBound string, q activity.Query, collectRead, compact bool,
) ([]activity.UsageRow, *export.PricingBlock, []string, time.Time, error) {
	pricing, err := db.loadPricingMap(ctx)
	if err != nil {
		return nil, nil, nil, time.Time{}, fmt.Errorf("loading pricing: %w", err)
	}
	rateResolver := export.NewPricingResolver(pricing)
	facts, err := db.loadActivityUsageFacts(ctx, window, ids, lowerBound, upperBound)
	if err != nil {
		return nil, nil, nil, time.Time{}, err
	}
	slices.SortFunc(facts, compareActivityUsageFacts)
	selection := make([]activity.UsageSelectionRow, len(facts))
	var read []string
	var seen map[string]struct{}
	if collectRead {
		seen = make(map[string]struct{}, len(ids))
		for _, id := range ids {
			seen[id] = struct{}{}
		}
	}
	var next time.Time
	if compact {
		next = q.RangeEnd
	}
	for i, fact := range facts {
		row := fact.row
		if compact && fact.validTS {
			timestamp := time.UnixMilli(fact.millis).Add(time.Duration(fact.nanos))
			if !timestamp.Before(q.EffectiveEnd) && timestamp.Before(next) {
				next = timestamp
			}
		}
		if seen != nil {
			if _, ok := seen[row.SessionID]; !ok {
				read = append(read, row.SessionID)
				seen[row.SessionID] = struct{}{}
			}
		}
		selection[i] = activity.UsageSelectionRow{Agent: row.Agent, ClaudeMessageID: row.ClaudeMessageID, ClaudeRequestID: row.ClaudeRequestID, SourceUUID: row.SourceUUID, UsageDedupKey: row.UsageDedupKey, SessionID: row.SessionID, MessageOrdinal: row.MessageOrdinal, Timestamp: time.UnixMilli(fact.millis).Add(time.Duration(fact.nanos)), TimestampValid: fact.validTS, OutputTokens: fact.tokens.output, WebSearchRequests: row.WebSearchRequests}
	}
	mask, attribution, webSearchRequests, err := activity.SelectUsageSurvivorsForSessions(ctx, q.RangeStart, q.RangeEnd, q.EffectiveEnd, selection, ids)
	if err != nil {
		return nil, nil, nil, time.Time{}, err
	}
	survivors := 0
	for _, keep := range mask {
		if keep {
			survivors++
		}
	}
	out := make([]activity.UsageRow, 0, survivors)
	for k, fact := range facts {
		if !mask[k] {
			continue
		}
		f := fact.row
		row := activity.UsageRow{SessionID: f.SessionID, UsageSource: f.UsageSource, Model: f.Model, ProviderID: f.ProviderID, ClaudeMessageID: f.ClaudeMessageID, ClaudeRequestID: f.ClaudeRequestID, SourceUUID: f.SourceUUID, UsageDedupKey: f.UsageDedupKey, Timestamp: f.Timestamp, Project: f.Project, Machine: f.Machine, Agent: f.Agent, MessageOrdinal: f.MessageOrdinal, WebSearchRequests: f.WebSearchRequests, InputTokens: fact.tokens.input, OutputTokens: fact.tokens.output, CacheCreationTokens: fact.tokens.cacheCreation, CacheReadTokens: fact.tokens.cacheRead}
		reported := fact.reported
		if fact.costSource == CopilotReportedCostSource && reported.Valid {
			row.SessionCost = &money.Money{Microdollars: reported.Int64}
			reported = sql.NullInt64{}
			rateResolver.RecordUnattributedReported()
		}
		reported = ReportedCostForPricing(reported, fact.costStatus,
			catalogPricesAt(rateResolver, row.Model, fact.pricingTS))
		row.Cost, row.Priced, row.Contributes, err = activityReportRowCost(
			row.Model, fact.pricingTS, row.ProviderID, reported,
			fact.requestScoped, fact.tokens, webSearchRequests[k], rateResolver)
		if err != nil {
			return nil, nil, nil, time.Time{}, err
		}
		row.CostSource = export.CostSourceComputed
		if reported.Valid {
			row.CostSource = export.CostSourceReported
		}
		row.SessionID = attribution[k]
		row.WebSearchRequests = webSearchRequests[k]
		out = append(out, row)
	}
	block, err := rateResolver.BuildBlock()
	if err != nil {
		return nil, nil, nil, time.Time{}, fmt.Errorf("building pricing block: %w", err)
	}
	if compact {
		out, err = sumActivityUsageRows(out, q)
		if err != nil {
			return nil, nil, nil, time.Time{}, err
		}
	}
	return out, &block, read, next, nil
}

// sumActivityUsageRows retains individual rows for sessions with a reported
// session total, whose cost is allocated across those rows.
func sumActivityUsageRows(rows []activity.UsageRow, q activity.Query) ([]activity.UsageRow, error) {
	withTotal := make(map[string]bool)
	for _, row := range rows {
		// Reordering signed costs can make an otherwise valid report overflow.
		if row.Cost.Microdollars < 0 || (row.SessionCost != nil && row.SessionCost.Microdollars < 0) {
			return rows, nil
		}
		if row.SessionCost != nil {
			withTotal[row.SessionID] = true
		}
	}
	type key struct {
		session, model string
		window         int
	}
	indexes := make(map[key]int)
	windows := activity.QueryWindows(q)
	out := rows[:0]
	for _, row := range rows {
		if withTotal[row.SessionID] {
			out = append(out, row)
			continue
		}
		ts, err := time.Parse(time.RFC3339Nano, row.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("parsing activity usage timestamp: %w", err)
		}
		k := key{row.SessionID, row.Model, activity.WindowIndex(windows, ts)}
		if i, ok := indexes[k]; ok && out != nil {
			sum := &out[i]
			sum.InputTokens += row.InputTokens
			sum.OutputTokens += row.OutputTokens
			sum.CacheCreationTokens += row.CacheCreationTokens
			sum.CacheReadTokens += row.CacheReadTokens
			sum.WebSearchRequests += row.WebSearchRequests
			sum.Cost, err = money.Add(sum.Cost, row.Cost)
			if err != nil {
				return nil, fmt.Errorf("summing activity usage: %w", err)
			}
		} else {
			indexes[k] = len(out)
			out = append(out, row)
		}
	}
	if len(out) < len(rows) {
		out = slices.Clone(out)
	}
	return out, nil
}

// activityUsageFactBlock bounds each allocation of scanned facts: facts are
// large, and growing one slice of them copied several times its final size.
const activityUsageFactBlock = 8192

// activityUsageFactReaders bounds the parallel cache reads of one report. It
// matches the cache's connection pool.
const activityUsageFactReaders = readerMaxOpenConns

// loadActivityUsageFacts reads the candidate sessions' facts inside the
// bounds, plus the Claude snapshot peers that sessions outside the candidate
// set hold for those facts. Large candidate sets are read in parallel.
func (db *DB) loadActivityUsageFacts(
	ctx context.Context, window activityUsageWindow, ids []string,
	lowerBound, upperBound string,
) ([]*activityUsageFact, error) {
	candidates := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := window.sessions[id]; ok {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(candidates)
	if err != nil {
		return nil, fmt.Errorf("encoding activity usage session IDs: %w", err)
	}
	cache, release, err := db.usageCache.acquireGeneration(ctx, window.databaseID)
	if err != nil {
		return nil, err
	}
	defer release()
	read := func(ctx context.Context, query string, ids []byte) ([]*activityUsageFact, error) {
		rows, err := cache.db.QueryContext(ctx, query, string(ids), lowerBound, upperBound)
		if err != nil {
			return nil, fmt.Errorf("querying activity usage facts: %w", err)
		}
		defer rows.Close()
		var facts []*activityUsageFact
		var block []activityUsageFact
		var scan activityUsageScan
		for rows.Next() {
			if len(block) == cap(block) {
				block = make([]activityUsageFact, 0, activityUsageFactBlock)
			}
			block = append(block, activityUsageFact{})
			fact := &block[len(block)-1]
			ok, err := scan.fact(rows, fact, window, lowerBound, upperBound)
			if err != nil {
				return nil, fmt.Errorf("scanning activity usage facts: %w", err)
			}
			if ok {
				facts = append(facts, fact)
			} else {
				block = block[:len(block)-1]
			}
		}
		return facts, rows.Err()
	}
	readers := min(activityUsageFactReaders, max(1, len(candidates)/usageFillShardMinSessions))
	parts := make([][]*activityUsageFact, readers+1)
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() (err error) {
		parts[readers], err = read(groupCtx, activityUsagePeerFactsSQL, encoded)
		return err
	})
	for i, chunk := range slices.Collect(slices.Chunk(candidates, (len(candidates)+readers-1)/readers)) {
		group.Go(func() error {
			ids, err := json.Marshal(chunk)
			if err == nil {
				parts[i], err = read(groupCtx, activityUsageCandidateFactsSQL, ids)
			}
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	peers := parts[readers]
	facts := slices.Concat(parts[:readers]...)
	if len(peers) == 0 {
		return facts, nil
	}
	// A peer counts only when a candidate fact inside the bounds shares it.
	type claudePair struct{ message, request string }
	matched := make(map[claudePair]bool, len(peers))
	for _, peer := range peers {
		matched[claudePair{peer.row.ClaudeMessageID, peer.row.ClaudeRequestID}] = false
	}
	for _, fact := range facts {
		pair := claudePair{fact.row.ClaudeMessageID, fact.row.ClaudeRequestID}
		if _, ok := matched[pair]; ok {
			matched[pair] = true
		}
	}
	for _, peer := range peers {
		if matched[claudePair{peer.row.ClaudeMessageID, peer.row.ClaudeRequestID}] {
			facts = append(facts, peer)
		}
	}
	return facts, nil
}

// activityUsageScan holds the scan targets that do not live on a fact, so a
// reader allocates them once. Counters scan as int64, which database/sql
// assigns without the text round trip it uses for int.
type activityUsageScan struct {
	ordinal, millis, nanos sql.NullInt64
	counts                 [7]int64
}

// fact scans one fact row. It reports false for a fact outside the bounds or
// the window, which archive reads would not return.
func (scan *activityUsageScan) fact(
	rows *sql.Rows, fact *activityUsageFact, window activityUsageWindow,
	lowerBound, upperBound string,
) (bool, error) {
	counts := &scan.counts
	row := &fact.row
	if err := rows.Scan(
		&row.SessionID, &row.UsageSource, &scan.ordinal, &fact.pricingTS,
		&scan.millis, &scan.nanos, &row.Model, &row.ProviderID,
		&counts[0], &counts[1], &counts[2], &counts[3], &counts[4], &counts[5],
		&counts[6], &fact.reported, &fact.costStatus, &fact.costSource,
		&fact.requestScoped, &row.ClaudeMessageID, &row.ClaudeRequestID,
		&row.SourceUUID, &row.UsageDedupKey,
	); err != nil {
		return false, err
	}
	session, ok := window.sessions[row.SessionID]
	if !ok {
		return false, nil
	}
	fact.tokens = activityReportRowTokens{
		input: int(counts[0]), output: int(counts[1]), reasoning: int(counts[2]),
		cacheCreation: int(counts[3]), cacheCreation1h: int(counts[4]),
		cacheRead: int(counts[5]),
	}
	row.WebSearchRequests = int(counts[6])
	row.Timestamp = fact.pricingTS
	// A fact uses the session start exactly when its stored time is blank.
	if row.Timestamp == "" {
		row.Timestamp = session.StartedAt
		if session.StartedAtMillis != nil {
			scan.millis = sql.NullInt64{Int64: *session.StartedAtMillis, Valid: true}
			scan.nanos = sql.NullInt64{Int64: *session.StartedAtNanos, Valid: true}
		}
	}
	if row.Timestamp < lowerBound || row.Timestamp > upperBound {
		return false, nil
	}
	fact.validTS, fact.millis, fact.nanos = scan.millis.Valid, scan.millis.Int64, scan.nanos.Int64
	row.MessageOrdinal = UsageRowMessageOrdinal(scan.ordinal)
	row.Project, row.Machine, row.Agent = session.Project, session.Machine, session.Agent
	return true, nil
}

// compareActivityUsageFacts orders facts as the raw usage rows sort: by
// instant, session and ordinal, then by stored values. Raw token JSON never
// breaks a tie because a session has one message row per ordinal.
func compareActivityUsageFacts(a, b *activityUsageFact) int {
	switch {
	case a.validTS && b.validTS:
		if c := cmp.Or(cmp.Compare(a.millis, b.millis), cmp.Compare(a.nanos, b.nanos)); c != 0 {
			return c
		}
	case a.validTS != b.validTS:
		if a.validTS {
			return -1
		}
		return 1
	default:
		if c := cmp.Compare(a.row.Timestamp, b.row.Timestamp); c != 0 {
			return c
		}
	}
	if c := cmp.Compare(a.row.SessionID, b.row.SessionID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.row.MessageOrdinal, b.row.MessageOrdinal); c != 0 {
		return c
	}
	return cmp.Or(
		cmp.Compare(a.row.UsageSource, b.row.UsageSource),
		cmp.Compare(a.row.Model, b.row.Model),
		cmp.Compare(a.tokens.input, b.tokens.input),
		cmp.Compare(a.tokens.output, b.tokens.output),
		cmp.Compare(a.tokens.cacheCreation, b.tokens.cacheCreation),
		cmp.Compare(a.tokens.cacheRead, b.tokens.cacheRead),
		cmp.Compare(a.tokens.reasoning, b.tokens.reasoning),
		compareNullInt64(a.reported, b.reported),
		cmp.Compare(a.costSource, b.costSource),
		cmp.Compare(a.row.ClaudeMessageID, b.row.ClaudeMessageID),
		cmp.Compare(a.row.ClaudeRequestID, b.row.ClaudeRequestID),
		cmp.Compare(a.row.SourceUUID, b.row.SourceUUID),
		cmp.Compare(a.row.UsageDedupKey, b.row.UsageDedupKey),
	)
}
