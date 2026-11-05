package db

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
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
	installSeq int64
	timings    usageRollupRequestTimings
}

// usageWindowObserver provides deterministic synchronization for tests.
type usageWindowObserver struct {
	beforePrepare func(context.Context)
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
		return db.prepareUsageWindow(ctx, filter, kind, cursor, filter.Progress)
	}
	memo.mu.Lock()
	prepare, ok := memo.windows[key]
	if !ok {
		prepare = sync.OnceValues(func() (usagePreparedWindow, error) {
			prepared, err := db.prepareUsageWindow(ctx, filter, kind, cursor, filter.Progress)
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

func (db *DB) prepareUsageWindow(ctx context.Context, filter UsageFilter, kind usageQueryKind, cursor bool, report func(string)) (usagePreparedWindow, error) {
	if db.usageCache.usageWindowObserver.beforePrepare != nil {
		db.usageCache.usageWindowObserver.beforePrepare(ctx)
	}
	if report == nil {
		report = func(string) {}
	}
	started := time.Now()
	report("Reading archived sessions for this report")
	filter.Progress = nil
	prepared, ok, err := db.prepareJournaledUsageWindow(ctx, filter, kind, cursor, report)
	if ok || err != nil {
		return prepared, err
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
	fillStarted := time.Now()
	report(fmt.Sprintf("Preparing usage data for %d sessions in this report", timings.candidates))
	fills, err := cache.fill.Ensure(ctx, snapshot.Versions, snapshot.CursorHighWater)
	if err != nil {
		return usagePreparedWindow{}, err
	}
	timings.fill = time.Since(fillStarted)
	snapshot.dropDeleted(fills)
	resolver := export.NewPricingResolverWithDigest(snapshot.PricingRows, snapshot.PricingDigest)
	report(fmt.Sprintf("Calculating daily totals for %d sessions in this report", timings.candidates))
	installs, metrics, err := cache.rollup.Ensure(ctx, snapshot, fills, resolver)
	if err != nil {
		return usagePreparedWindow{}, err
	}
	timings.rollup = metrics
	return usagePreparedWindow{snapshot: snapshot, installs: installs, timings: timings}, nil
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
	for attempt := 1; attempt <= usageFillMaxAttempts; attempt++ {
		if attempt > 1 {
			memo.drop(key)
		}
		started := time.Now()
		var prepared usagePreparedWindow
		var err error
		if attempt == 1 {
			prepared, err = db.sharedUsageWindow(ctx, key, filter, kind, cursor)
		} else {
			prepared, err = db.prepareUsageWindow(ctx, filter, kind, cursor, filter.Progress)
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
			matching, queryErr = cache.usageRollupActivityQuery(ctx, prepared, filter)
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
