package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// usageRollupSlowRequestThreshold gates the privacy-safe phase-timing log so
// warm reads stay silent and slow requests explain where the time went.
const usageRollupSlowRequestThreshold = 2 * time.Second

type usageRollupRequestTimings struct {
	capture, sweep, fill, read time.Duration
	rollup                     usageRollupMetrics
	candidates                 int
}

func (timings usageRollupRequestTimings) logIfSlow(started time.Time) {
	total := time.Since(started)
	if total < usageRollupSlowRequestThreshold {
		return
	}
	round := func(value time.Duration) time.Duration {
		return value.Round(time.Millisecond)
	}
	log.Printf("usage rollup read took %s: capture %s, deletion sweep %s, "+
		"fact fill %s, rollup build %s, rollup install %s, cache read %s "+
		"(%d candidate sessions; built %d daily rows, %d exception rows in %d groups)",
		round(total), round(timings.capture), round(timings.sweep),
		round(timings.fill), round(timings.rollup.BuildDuration),
		round(timings.rollup.InstallDuration), round(timings.read),
		timings.candidates, timings.rollup.DailyRows,
		timings.rollup.ExceptionRows, timings.rollup.ExceptionGroups)
}

type usagePreparedWindow struct {
	snapshot   usageQuerySnapshot
	installs   map[string]usageRollupInstall
	journaled  bool
	kind       usageQueryKind
	installSeq int64
	timings    usageRollupRequestTimings
}

// usageWindowObserver provides deterministic synchronization for tests.
type usageWindowObserver struct {
	beforePrepare func(context.Context)
	afterRead     func(journaled bool)
}

// SetUsageWindowPrepareForTest observes preparations. Call before starting reads.
func (db *DB) SetUsageWindowPrepareForTest(beforePrepare func(context.Context)) {
	db.usageCache.usageWindowObserver.beforePrepare = beforePrepare
}

type usageWindowMemoKey struct{}

type usageWindowMemo struct {
	mu      sync.Mutex
	windows map[string]func() (usagePreparedWindow, error)
}

// WithUsageWindowMemo reuses prepared windows across reads in one request.
func WithUsageWindowMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, usageWindowMemoKey{}, &usageWindowMemo{
		windows: make(map[string]func() (usagePreparedWindow, error)),
	})
}

func (memo *usageWindowMemo) drop(key string) {
	if memo == nil {
		return
	}
	memo.mu.Lock()
	defer memo.mu.Unlock()
	delete(memo.windows, key)
}

func usageWindowKey(filter UsageFilter, kind usageQueryKind, cursor bool) string {
	where, args := filter.appendUsageSessionFilterClauses("1=1", nil)
	return fmt.Sprintf("%d|%q|%q|%q|%t|%q|%#v", kind, filter.From, filter.To, filter.Timezone, cursor, where, args)
}

func (db *DB) sharedUsageWindow(ctx context.Context, key string, filter UsageFilter, kind usageQueryKind, cursor bool) (usagePreparedWindow, error) {
	memo, _ := ctx.Value(usageWindowMemoKey{}).(*usageWindowMemo)
	if memo == nil {
		return db.prepareUsageWindow(ctx, filter, kind, cursor, true, filter.Progress)
	}
	memo.mu.Lock()
	prepare, ok := memo.windows[key]
	if !ok {
		prepare = sync.OnceValues(func() (usagePreparedWindow, error) {
			prepared, err := db.prepareUsageWindow(ctx, filter, kind, cursor, true, filter.Progress)
			if err != nil {
				memo.drop(key)
			}
			return prepared, err
		})
		memo.windows[key] = prepare
	}
	memo.mu.Unlock()
	prepared, err := prepare()
	if ok {
		// Only the call that owns preparation reports its phase timings.
		prepared.timings = usageRollupRequestTimings{candidates: prepared.timings.candidates}
	}
	return prepared, err
}

func (db *DB) prepareUsageWindow(ctx context.Context, filter UsageFilter, kind usageQueryKind, cursor, journal bool, report func(string)) (usagePreparedWindow, error) {
	if db.usageCache.usageWindowObserver.beforePrepare != nil {
		db.usageCache.usageWindowObserver.beforePrepare(ctx)
	}
	if report == nil {
		report = func(string) {}
	}
	started := time.Now()
	report("Reading archived sessions for this report")
	filter.Progress = nil
	var prepared usagePreparedWindow
	var err error
	if journal && kind != usageQueryKindActivity {
		var ok bool
		prepared, ok, err = db.prepareJournaledUsageWindow(ctx, filter, kind, cursor, report)
		if ok || err != nil {
			return prepared, err
		}
	}
	snapshot := prepared.snapshot
	if snapshot.DatabaseID == "" {
		snapshot, err = db.captureUsageQuery(ctx, filter, kind)
		if err != nil {
			return usagePreparedWindow{}, err
		}
	}
	if !cursor {
		snapshot.CursorHighWater = 0
	}
	timings := usageRollupRequestTimings{capture: time.Since(started), candidates: len(snapshot.Versions)}
	report("Checking the usage cache for this report")
	cache, release, err := db.usageCache.acquireGeneration(ctx, snapshot.DatabaseID)
	if err != nil {
		return usagePreparedWindow{}, err
	}
	defer release()
	sweepStarted := time.Now()
	if err := cache.sweepDeletionJournal(ctx, db); err != nil {
		return usagePreparedWindow{}, err
	}
	timings.sweep = time.Since(sweepStarted)
	installs, measured, err := cache.fillAndRollupUsageWindow(ctx, &snapshot, &snapshot, report)
	if err != nil {
		return usagePreparedWindow{}, err
	}
	timings.fill, timings.rollup = measured.fill, measured.rollup
	return usagePreparedWindow{snapshot: snapshot, installs: installs, timings: timings}, nil
}

func (cache *usageCache) fillAndRollupUsageWindow(ctx context.Context, snapshot, window *usageQuerySnapshot, report func(string)) (map[string]usageRollupInstall, usageRollupRequestTimings, error) {
	timings := usageRollupRequestTimings{candidates: len(snapshot.Versions)}
	fillStarted := time.Now()
	report(fmt.Sprintf("Preparing usage data for %d sessions in this report", timings.candidates))
	fills, err := cache.fillUsageWindow(ctx, snapshot)
	if err != nil {
		return nil, timings, err
	}
	timings.fill = time.Since(fillStarted)
	// A journal refresh builds only changed sessions, but deletions also
	// remove those sessions from the report's complete window.
	if window != snapshot {
		window.dropDeleted(maps.Clone(fills))
	}
	snapshot.dropDeleted(fills)
	resolver := export.NewPricingResolverWithDigest(snapshot.PricingRows, snapshot.PricingDigest)
	report(fmt.Sprintf("Calculating daily totals for %d sessions in this report", timings.candidates))
	installs, metrics, err := cache.rollup.Ensure(ctx, *snapshot, fills, resolver)
	if err != nil {
		return nil, timings, err
	}
	timings.rollup = metrics
	return installs, timings, nil
}

// fillUsageWindow brings the facts of every snapshot session up to date,
// splitting large windows across the bulk-read slots.
func (cache *usageCache) fillUsageWindow(ctx context.Context, snapshot *usageQuerySnapshot) (map[string]usageFillResult, error) {
	shards := max(1, min(bulkReadSlots, len(snapshot.Versions)/usageFillShardMinSessions))
	var fills map[string]usageFillResult
	var err error
	if shards == 1 {
		fills, err = cache.fill.Ensure(ctx, snapshot.Versions, snapshot.CursorHighWater)
	} else {
		fills = make(map[string]usageFillResult, len(snapshot.Versions))
		var wait sync.WaitGroup
		var mu sync.Mutex
		// Units smaller than a shard let one worker install its facts while
		// the others extract theirs.
		units := slices.Collect(slices.Chunk(snapshot.Versions, usageFillShardMinSessions))
		var next atomic.Int64
		for range shards {
			wait.Go(func() {
				for unit := next.Add(1) - 1; unit < int64(len(units)); unit = next.Add(1) - 1 {
					results, fillErr := cache.fill.Ensure(ctx, units[unit], 0)
					mu.Lock()
					maps.Copy(fills, results)
					err = errors.Join(err, fillErr)
					mu.Unlock()
					if fillErr != nil {
						return
					}
				}
			})
		}
		wait.Wait()
		if err == nil && snapshot.CursorHighWater > 0 {
			err = cache.fill.ensureCursor(ctx, snapshot.CursorHighWater)
		}
	}
	return fills, err
}

func (db *DB) queryUsageRollups(
	ctx context.Context, filter UsageFilter, kind usageQueryKind,
	includeCursor bool,
) (usageQuerySnapshot, usageFactsResult, *export.PricingResolver, error) {
	// Retirement and cache-side install invalidation require a fresh capture.
	var lastErr error
	memo, _ := ctx.Value(usageWindowMemoKey{}).(*usageWindowMemo)
	cursor := includeCursor && usageCursorIncluded(filter)
	var key string
	if memo != nil {
		key = usageWindowKey(filter, kind, cursor)
	}
	// Day totals get one fresh journaled attempt after concurrent installs.
	// Further conflicts use full-window preparation within the same budget.
	for attempt := 1; attempt <= usageFillMaxAttempts+1; attempt++ {
		if attempt > 1 {
			memo.drop(key)
		}
		started := time.Now()
		var prepared usagePreparedWindow
		var err error
		if attempt == 1 {
			prepared, err = db.sharedUsageWindow(ctx, key, filter, kind, cursor)
		} else {
			prepared, err = db.prepareUsageWindow(ctx, filter, kind, cursor, attempt == 2 && kind == usageQueryKindDayTotals, filter.Progress)
		}
		if err != nil {
			if errors.Is(err, errUsageCacheSourceChanged) {
				lastErr = err
				continue
			}
			return usageQuerySnapshot{}, usageFactsResult{}, nil, err
		}
		snapshot := prepared.snapshot
		resolver := export.NewPricingResolverWithDigest(snapshot.PricingRows, snapshot.PricingDigest)
		cache, release, err := db.usageCache.acquireGeneration(ctx, snapshot.DatabaseID)
		if err != nil {
			if errors.Is(err, errUsageCacheSourceChanged) {
				lastErr = err
				continue
			}
			return usageQuerySnapshot{}, usageFactsResult{}, nil, err
		}
		readStarted := time.Now()
		if filter.Progress != nil {
			filter.Progress("Reading cached daily totals")
		}
		var facts usageFactsResult
		var queryErr error
		if kind == usageQueryKindActivity {
			var matching map[string]UsageSessionInfo
			matching, queryErr = cache.usageRollupActivityQuery(ctx, snapshot, filter, prepared.installs)
			facts.MatchingSessions = matching
		} else {
			facts, queryErr = cache.usageRollupQuery(ctx, prepared, filter, resolver)
		}
		timings := prepared.timings
		timings.read = time.Since(readStarted)
		if queryErr == nil {
			cache.recordUsageTimezoneRequest(ctx, usageTimezoneIdentityFor(snapshot.location, snapshot.Intervals))
			release()
			timings.logIfSlow(started)
			if observe := db.usageCache.usageWindowObserver.afterRead; observe != nil {
				observe(prepared.journaled)
			}
			return snapshot, facts, resolver, nil
		}
		release()
		if !usageCacheReadShouldRecapture(queryErr) {
			return usageQuerySnapshot{}, usageFactsResult{}, nil, queryErr
		}
		lastErr = queryErr
	}
	return usageQuerySnapshot{}, usageFactsResult{}, nil, lastErr
}

// GetTopSessionsByCost returns filtered sessions ranked by cost or tokens.
func (db *DB) GetTopSessionsByCost(
	ctx context.Context, filter UsageFilter, limit int,
) ([]TopSessionEntry, error) {
	snapshot, facts, _, err := db.queryUsageRollups(
		ctx, filter, usageQueryKindToken, false)
	if err != nil {
		return nil, err
	}
	type totals struct {
		input, output, cacheWrite, cacheRead int
		cost                                 money.Money
		authoritative                        *money.Money
	}
	bySession := make(map[string]*totals)
	for _, group := range facts.Groups {
		if group.SessionID == "" {
			continue
		}
		current := bySession[group.SessionID]
		if current == nil {
			current = &totals{}
			bySession[group.SessionID] = current
		}
		current.input += int(group.InputTokens)
		current.output += int(group.OutputTokens)
		current.cacheWrite += int(group.CacheCreationTokens)
		current.cacheRead += int(group.CacheReadTokens)
		current.cost, err = money.Add(current.cost,
			money.Money{Microdollars: group.CostMicrodollars})
		if err != nil {
			return nil, fmt.Errorf("summing top-session cost: %w", err)
		}
		if !filter.HasModelFilter() &&
			group.AuthoritativeCostMicrodollars != nil {
			value := money.Money{Microdollars: *group.AuthoritativeCostMicrodollars}
			current.authoritative = &value
		}
	}
	metadata := make(map[string]usageQuerySession, len(snapshot.Sessions))
	for _, session := range snapshot.Sessions {
		metadata[session.ID] = session
	}
	result := make([]TopSessionEntry, 0, len(bySession))
	for sessionID, value := range bySession {
		cost := value.cost
		if value.authoritative != nil {
			cost = *value.authoritative
		}
		entry := TopSessionEntry{
			SessionID: sessionID, DisplayName: sessionID,
			InputTokens: value.input, OutputTokens: value.output,
			CacheCreationTokens: value.cacheWrite,
			CacheReadTokens:     value.cacheRead,
			TotalTokens:         value.input + value.output + value.cacheWrite + value.cacheRead,
			Cost:                cost,
		}
		if session, ok := metadata[sessionID]; ok {
			entry.DisplayName = session.DisplayName
			entry.Agent = session.Agent
			entry.Project = session.Project
			entry.StartedAt = session.StartedAt
			entry.Machine = session.Machine
			entry.GroupKey = session.GroupKey
			entry.SessionName = session.SessionName
		}
		result = append(result, entry)
	}
	if filter.TopSessionsByGroup {
		return GroupTopSessions(result, limit, filter.TopSessionsSort, filter.TopSessionsTokenTypes)
	}
	return SortAndLimitTopSessions(
		result, limit, filter.TopSessionsSort, filter.TopSessionsTokenTypes,
	), nil
}

// GetUsageSessionCounts returns distinct billed survivor owners by metadata.
func (db *DB) GetUsageSessionCounts(
	ctx context.Context, filter UsageFilter,
) (UsageSessionCounts, error) {
	_, facts, _, err := db.queryUsageRollups(
		ctx, filter, usageQueryKindToken, false)
	if err != nil {
		return UsageSessionCounts{}, err
	}
	return NewUsageSessionCounts(facts.MatchingSessions), nil
}

// GetUsageMatchingSessionCount counts sessions with eligible usage activity.
func (db *DB) GetUsageMatchingSessionCount(
	ctx context.Context, filter UsageFilter,
) (int, error) {
	_, facts, _, err := db.queryUsageRollups(
		ctx, filter, usageQueryKindActivity, false)
	if err != nil {
		return 0, err
	}
	return len(facts.MatchingSessions), nil
}

// GetSessionUsage returns one session's exact, intra-session usage summary.
func (db *DB) GetSessionUsage(
	ctx context.Context, sessionID string, includeBreakdown bool,
) (*SessionUsage, error) {
	return db.getSessionUsageLegacy(ctx, sessionID, includeBreakdown)
}
