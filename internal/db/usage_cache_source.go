package db

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// UsageSourceVersion identifies the committed facts of a session.
type UsageSourceVersion = usageSourceVersion

// UsageSourceSession is the metadata that determines attribution and filters.
type UsageSourceSession = usageQuerySession

// UsageSourceSnapshot contains values copied from one source transaction.
type UsageSourceSnapshot = usageQuerySnapshot

// ErrUsageSourceUnavailable means the source cannot currently support caching.
var ErrUsageSourceUnavailable = errors.New("usage cache source unavailable")

// UsageSourceRequest selects a window, explicit sessions, or journal changes.
// A non-nil IDs selects exactly those sessions, including an empty set.
type UsageSourceRequest struct {
	Filter        UsageFilter
	IDs           *[]string
	AfterRevision *int64
}

// UsageSourceAccount is one imported account billing record.
type UsageSourceAccount struct {
	ID                                                                                int64
	Agent, Timestamp, Model, DedupKey                                                 string
	InputTokens, OutputTokens, CacheWriteTokens, CacheReadTokens, ChargedMicrodollars int64
	Headless                                                                          bool
}

// UsageCacheSource supplies source snapshots while the shared cache owns facts,
// pricing, deduplication, publication, and generation lifetimes.
type UsageCacheSource interface {
	UsageSourceIdentity(context.Context) (string, error)
	CaptureUsage(context.Context, UsageSourceRequest) (UsageSourceSnapshot, error)
	StreamUsage(context.Context, []string, func(string, usagefacts.Fact) error) (UsageSourceSnapshot, error)
	ReadUsageAccounts(context.Context) (string, int64, []UsageSourceAccount, error)
	BuildProjectIdentityMap(context.Context, []string) (map[string]export.ProjectMapEntry, error)
	UsageSessionNames(context.Context, []string) (map[string][2]string, error)
}

// UsageCacheReader exposes aggregate reads backed by the shared cache engine.
// A read that would first have to fill or rebuild much of the cache returns
// an error instead, so the caller serves its direct read, and one background
// worker brings the cache up to date for later reads.
type UsageCacheReader struct {
	engine *DB
	// deferWork makes reads hand large preparation to the warm worker.
	deferWork bool

	warmMu      sync.Mutex
	warmPending map[string]bool // timezone names waiting for a warm pass
	warmRunning string          // timezone of the running warm pass
	warmBusy    bool            // a warm pass is running
	warmIdle    chan struct{}   // closed while no warm pass runs
	warmCtx     context.Context
	warmCancel  context.CancelFunc
	// directReads counts callers serving a direct read after a deferred
	// read; lastDeferred is when a read last deferred. A warm pass waits
	// for both to settle, so it does not compete with those direct reads.
	directReads  int
	lastDeferred time.Time
}

// UsageChangeSummary describes journal changes after a claimed revision.
type UsageChangeSummary struct {
	Sessions int  // sessions with changed usage or metadata
	Global   bool // pricing, project identity, or account usage changed
}

// UsageChangeSummarizer is an optional UsageCacheSource capability. It lets
// a deferring read decide from one cheap query whether the cache is close
// enough to current to refresh during the read.
type UsageChangeSummarizer interface {
	SummarizeUsageChanges(ctx context.Context, afterRevision int64) (UsageChangeSummary, error)
}

const (
	// usageWarmDirectReadWait bounds how long a warm pass yields to direct
	// reads, so steady traffic cannot postpone it indefinitely.
	usageWarmDirectReadWait = 5 * time.Second
	// usageWarmDeferGrace covers the gap between a deferred read returning
	// and its caller calling HoldWarm before its direct read.
	usageWarmDeferGrace   = 20 * time.Millisecond
	usageWarmPollInterval = 10 * time.Millisecond
)

// NewUsageCacheReader keeps each remote target in its own retirement directory.
// The source identity further separates incarnations inside that directory.
func NewUsageCacheReader(directory string, source UsageCacheSource) (*UsageCacheReader, error) {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		log.Printf("creating persistent usage cache directory: %v; using temporary storage", err)
	}
	engine := &DB{usageSource: source}
	engine.usageCache = newUsageCacheManager(filepath.Join(directory, "source"))
	engine.usageCache.attachArchive(engine)
	// Warm passes belong to the reader, not to the request that queued them,
	// so they start from Background and end when Close cancels them.
	warmCtx, warmCancel := context.WithCancel(context.Background())
	idle := make(chan struct{})
	close(idle)
	return &UsageCacheReader{
		engine: engine, warmPending: make(map[string]bool), warmIdle: idle,
		warmCtx: warmCtx, warmCancel: warmCancel,
	}, nil
}

func (reader *UsageCacheReader) Close() error {
	reader.warmCancel()
	reader.warmMu.Lock()
	idle := reader.warmIdle
	reader.warmMu.Unlock()
	<-idle
	return reader.engine.usageCache.Close()
}

// DeferWork makes reads that would fill or rebuild much of the cache return
// an error and queue a background warm pass instead. Call it before reads.
func (reader *UsageCacheReader) DeferWork() { reader.deferWork = true }

func (reader *UsageCacheReader) readContext(ctx context.Context) context.Context {
	if !reader.deferWork {
		return ctx
	}
	return withUsageWorkDeferred(ctx)
}

// WaitWarm waits until no background warm pass is running or queued.
func (reader *UsageCacheReader) WaitWarm(ctx context.Context) error {
	reader.warmMu.Lock()
	idle := reader.warmIdle
	reader.warmMu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-idle:
		return nil
	}
}

func (reader *UsageCacheReader) GetDailyUsage(ctx context.Context, filter UsageFilter) (DailyUsageResult, error) {
	if reader.warming(filter) {
		return DailyUsageResult{}, ErrUsageCacheWarming
	}
	result, err := reader.engine.GetDailyUsage(reader.readContext(ctx), filter)
	return result, reader.deferred(filter, err)
}

func (reader *UsageCacheReader) GetTopSessionsByCost(ctx context.Context, filter UsageFilter, limit int) ([]TopSessionEntry, error) {
	if reader.warming(filter) {
		return nil, ErrUsageCacheWarming
	}
	result, err := reader.engine.GetTopSessionsByCost(reader.readContext(ctx), filter, limit)
	return result, reader.deferred(filter, err)
}

func (reader *UsageCacheReader) GetUsageSessionCounts(ctx context.Context, filter UsageFilter) (UsageSessionCounts, error) {
	if reader.warming(filter) {
		return UsageSessionCounts{}, ErrUsageCacheWarming
	}
	result, err := reader.engine.GetUsageSessionCounts(reader.readContext(ctx), filter)
	return result, reader.deferred(filter, err)
}

func (reader *UsageCacheReader) GetUsageMatchingSessionCount(ctx context.Context, filter UsageFilter) (int, error) {
	if reader.warming(filter) {
		return 0, ErrUsageCacheWarming
	}
	result, err := reader.engine.GetUsageMatchingSessionCount(reader.readContext(ctx), filter)
	return result, reader.deferred(filter, err)
}

// warming reports whether a deferring read should skip the cache because a
// warm pass for its timezone is queued or running. The pass will replace
// what the read would have checked.
func (reader *UsageCacheReader) warming(filter UsageFilter) bool {
	if !reader.deferWork {
		return false
	}
	reader.warmMu.Lock()
	defer reader.warmMu.Unlock()
	queued := reader.warmPending[filter.Timezone] ||
		(reader.warmBusy && reader.warmRunning == filter.Timezone)
	if queued {
		reader.lastDeferred = time.Now()
	}
	return queued
}

// deferred queues a warm pass for a read that declined to build the cache.
func (reader *UsageCacheReader) deferred(filter UsageFilter, err error) error {
	if errors.Is(err, ErrUsageCacheWarming) {
		reader.queueWarm(filter.Timezone)
	}
	return err
}

// HoldWarm marks a direct read served after a deferred cache read. Warm
// passes wait until every hold is released, within a bounded time. Call the
// returned function when the direct read finishes.
func (reader *UsageCacheReader) HoldWarm() func() {
	reader.warmMu.Lock()
	reader.directReads++
	reader.warmMu.Unlock()
	return sync.OnceFunc(func() {
		reader.warmMu.Lock()
		reader.directReads--
		reader.warmMu.Unlock()
	})
}

// waitForDirectReads lets the direct reads that deferred to this pass
// finish first. It polls because a deferred read takes its hold just after
// it returns; the grace period covers that gap.
func (reader *UsageCacheReader) waitForDirectReads() {
	deadline := time.Now().Add(usageWarmDirectReadWait)
	for time.Now().Before(deadline) {
		reader.warmMu.Lock()
		quiet := reader.directReads == 0 &&
			time.Since(reader.lastDeferred) >= usageWarmDeferGrace
		reader.warmMu.Unlock()
		if quiet {
			return
		}
		select {
		case <-reader.warmCtx.Done():
			return
		case <-time.After(usageWarmPollInterval):
		}
	}
}

func (reader *UsageCacheReader) queueWarm(timezone string) {
	reader.warmMu.Lock()
	defer reader.warmMu.Unlock()
	if reader.warmCtx.Err() != nil {
		return
	}
	reader.warmPending[timezone] = true
	reader.lastDeferred = time.Now()
	select {
	case <-reader.warmIdle:
	default:
		return // The running worker drains the queue before it stops.
	}
	idle := make(chan struct{})
	reader.warmIdle = idle
	go reader.runWarm(idle)
}

// runWarm reads the whole history of each queued timezone. That read fills
// every changed session, rebuilds stale rollups, and records journal
// coverage for the zone, so later reads of any window refresh only the
// sessions changed since. One worker keeps the warm's memory to one pass.
func (reader *UsageCacheReader) runWarm(idle chan struct{}) {
	for {
		reader.waitForDirectReads()
		reader.warmMu.Lock()
		var timezone string
		found := false
		for name := range reader.warmPending {
			timezone, found = name, true
			delete(reader.warmPending, name)
			break
		}
		if !found || reader.warmCtx.Err() != nil {
			reader.warmBusy = false
			close(idle)
			reader.warmMu.Unlock()
			return
		}
		reader.warmBusy, reader.warmRunning = true, timezone
		reader.warmMu.Unlock()
		_, err := reader.engine.GetDailyUsage(reader.warmCtx, UsageFilter{Timezone: timezone})
		if err != nil && reader.warmCtx.Err() == nil {
			log.Printf("warming usage cache for timezone %q: %v", timezone, err)
		}
	}
}

func (db *DB) captureExternalUsage(ctx context.Context, filter UsageFilter, ids *[]string, after *int64) (usageQuerySnapshot, error) {
	snapshot, err := db.usageSource.CaptureUsage(ctx, UsageSourceRequest{Filter: filter, IDs: ids, AfterRevision: after})
	if err != nil {
		return snapshot, err
	}
	if snapshot.DatabaseID != "" {
		db.usageSourceDatabaseID.Store(&snapshot.DatabaseID)
	}
	snapshot.exactAccountRevision = true
	snapshot.location = filter.Location()
	snapshot.Intervals = usageQueryIntervals(filter)
	snapshot.PricingDigest, err = export.EffectivePricingDigest(snapshot.PricingRows)
	snapshot.PricingIdentity = usagePricingIdentityWithDigest(snapshot.PricingDigest)
	for i := range snapshot.Versions {
		snapshot.Versions[i].CapturePricingDigest = snapshot.PricingDigest
	}
	return snapshot, err
}

func (db *DB) usageProjectIdentityMap(ctx context.Context, labels []string) (map[string]export.ProjectMapEntry, error) {
	if db.usageSource != nil {
		return db.usageSource.BuildProjectIdentityMap(ctx, labels)
	}
	return db.BuildProjectIdentityMap(ctx, labels)
}

func (db *DB) usageTopSessionMetadata(ctx context.Context, ids []string) (map[string]topSessionMetadata, error) {
	if db.usageSource == nil {
		return db.loadTopSessionMetadata(ctx, ids)
	}
	names, err := db.usageSource.UsageSessionNames(ctx, ids)
	result := make(map[string]topSessionMetadata, len(names))
	for id, name := range names {
		result[id] = topSessionMetadata{displayName: name[0], sessionName: name[1]}
	}
	return result, err
}

// A nil ranking value uses billed output; PostgreSQL can supply its SQL value.
func usageSnapshotRankingOutput(fact usagefacts.Fact) int64 {
	if fact.RankingOutputTokens != nil {
		return *fact.RankingOutputTokens
	}
	return fact.OutputTokens
}

func (c *usageFillCoordinator) extractExternalSessions(ctx context.Context, versions []usageSourceVersion, expected map[string]usageFillInstallExpectation) (_ *usageFactSpool, _ map[string]usageSourceVersion, err error) {
	spool, err := newUsageFactSpool(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, spool.Close())
		}
	}()
	tx, err := spool.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	writer := usageFactSpoolWriter{ctx: ctx, tx: tx}
	counts := make(map[string][3]int, len(versions))
	snapshot, err := c.archive.usageSource.StreamUsage(ctx, usageSourceVersionIDs(versions), func(id string, fact usagefacts.Fact) error {
		count := counts[id]
		kind, base := 0, 0
		if fact.Source != "message" {
			kind, base = 2, usageEventFactBase
		} else if !fact.TokenEligible {
			kind, base = 1, usageActivityFactBase
		}
		if err := writer.Add(id, base+count[kind], fact); err != nil {
			return err
		}
		count[kind]++
		counts[id] = count
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	capturedPricing, err := export.EffectivePricingDigest(snapshot.PricingRows)
	if err != nil {
		return nil, nil, err
	}
	capturedVersions := make(map[string]usageSourceVersion, len(snapshot.Versions))
	for _, version := range snapshot.Versions {
		capturedVersions[version.SessionID] = version
	}
	for _, version := range versions {
		if version.CapturePricingDigest != "" && version.CapturePricingDigest != capturedPricing {
			return nil, nil, errUsageCacheSourceChanged
		}
		captured := capturedVersions[version.SessionID]
		captured.SessionID = version.SessionID
		if !version.Equal(captured) {
			return nil, nil, errUsageCacheSourceChanged
		}
	}
	if snapshot.DatabaseID != c.cache.databaseID {
		return nil, nil, errUsageCacheSourceChanged
	}
	extracted := make(map[string]usageSourceVersion, len(snapshot.Versions))
	for _, version := range snapshot.Versions {
		extracted[version.SessionID] = version
		count := counts[version.SessionID]
		var revision any
		if expectation := expected[version.SessionID]; expectation.Exists {
			revision = expectation.InstallRevision
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(session_id, install_revision, message_count, activity_count, event_count) VALUES (?, ?, ?, ?, ?)`, version.SessionID, revision, count[0], count[1], count[2]); err != nil {
			return nil, nil, err
		}
	}
	if err := writer.Flush(); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	spool.changeRevision = snapshot.ChangeRevision
	return spool, extracted, nil
}

// Account sources can update or delete existing IDs. Replace their facts and
// invalidate both removed and arriving identities in the same transaction.
func (c *usageFillCoordinator) replaceExternalAccounts(ctx context.Context, target int64) error {
	databaseID, revision, accounts, err := c.archive.usageSource.ReadUsageAccounts(ctx)
	if err != nil {
		return err
	}
	if databaseID != c.cache.databaseID || revision != target {
		return errUsageCacheSourceChanged
	}
	tx, err := c.cache.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer tx.Close()
	if _, err := tx.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer func() { _, _ = tx.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`) }()
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM usage_cache_metadata WHERE key=?`, usageCacheMetadataCursorHighWaterMark).Scan(&current); err != nil {
		return err
	}
	currentRevision, err := strconv.ParseInt(current, 10, 64)
	if err != nil {
		return err
	}
	if currentRevision >= target {
		return nil
	}
	identities := newUsageDedupIdentitySet()
	rows, err := tx.QueryContext(ctx, `SELECT dedup_key FROM cursor_usage_facts`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			_ = rows.Close()
			return err
		}
		identities.add("", "", "", key)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_rollup_installs WHERE session_id=?`, usageRollupCursorSessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM cursor_usage_facts`); err != nil {
		return err
	}
	for _, account := range accounts {
		identities.add("", "", "", account.DedupKey)
		millis, _, _ := usagefacts.ParseTimestamp(account.Timestamp)
		if _, err := tx.ExecContext(ctx, `INSERT INTO cursor_usage_facts VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, account.ID, account.Agent, millis, account.Timestamp, account.Model, max(account.InputTokens, 0), max(account.OutputTokens, 0), max(account.CacheWriteTokens, 0), max(account.CacheReadTokens, 0), account.ChargedMicrodollars, boolInt(account.Headless), account.DedupKey); err != nil {
			return err
		}
	}
	if _, err := invalidateUsageDedupSharers(ctx, tx, identities, nil); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usage_cache_metadata SET value=? WHERE key=?`, strconv.FormatInt(target, 10), usageCacheMetadataCursorHighWaterMark); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `COMMIT`)
	return err
}

func (db *DB) captureExternalJournaledUsageWindow(ctx context.Context, filter UsageFilter, cursor bool, databaseID string, zone usageZoneCoverage, lost, window []string) (usageQuerySnapshot, usageQuerySnapshot, []string, bool, error) {
	requested := slices.Concat(window, lost)
	snapshot, err := db.captureExternalUsage(ctx, filter, &requested, &zone.revision.Int64)
	if err != nil {
		return snapshot, usageQuerySnapshot{}, nil, false, err
	}
	if snapshot.DatabaseID != databaseID {
		return snapshot, usageQuerySnapshot{}, nil, false, errUsageCacheSourceChanged
	}
	if zone.revision.Int64 > snapshot.ChangeRevision || snapshot.PricingIdentity != zone.pricing {
		return usageQuerySnapshot{}, usageQuerySnapshot{}, nil, false, nil
	}
	if !cursor {
		snapshot.CursorHighWater = 0
	}
	refreshIDs := slices.Concat(snapshot.ChangeIDs, lost)
	slices.Sort(refreshIDs)
	refreshIDs = slices.Compact(refreshIDs)
	refresh := snapshot
	refresh.Sessions, refresh.Versions = nil, nil
	for i, session := range snapshot.Sessions {
		if _, ok := slices.BinarySearch(refreshIDs, session.ID); ok {
			refresh.Sessions = append(refresh.Sessions, session)
			refresh.Versions = append(refresh.Versions, snapshot.Versions[i])
		}
	}
	return snapshot, refresh, refreshIDs, true, nil
}

// A complete source capture permits removal of stale deleted owners before
// its first global coverage claim. Bounded windows retain their own proof.
func (cache *usageCache) sweepAbsentUsageOwners(ctx context.Context, snapshot usageQuerySnapshot) error {
	rows, err := cache.db.QueryContext(ctx, `SELECT session_id FROM usage_cached_sessions`)
	if err != nil {
		return err
	}
	active := make(map[string]bool, len(snapshot.Sessions))
	for _, session := range snapshot.Sessions {
		active[session.ID] = true
	}
	var deleted []string
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		if !active[id] {
			deleted = append(deleted, id)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	return cache.fill.deleteNotificationSessions(deleted, snapshot.ChangeRevision)
}
