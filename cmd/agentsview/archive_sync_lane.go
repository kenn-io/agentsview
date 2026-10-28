package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	stdsync "sync"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/archivesource"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// archivePushWatch runs a push-watch process. The same loop serves both
// archive owners. When this process owns the SQLite archive, engine is set:
// watcher batches and periodic syncs write the archive on their own
// goroutines, and each push copies whatever the archive holds when it starts,
// so a push that takes an hour never delays ingestion. When the daemon owns
// the archive, engine is nil: the daemon syncs before each push, and watcher
// batches travel with the push requests.
type archivePushWatch struct {
	hooks  *archivePushWatchHooks
	appCfg config.Config
	engine *syncpkg.Engine
	// database is the archive engine writes. Set it with engine.
	database           *db.DB
	label              string
	debounce, interval time.Duration
	full               bool
	// push runs one push. batch is the watcher scope a daemon owner syncs
	// before it pushes; a local owner has already synced it.
	push func(ctx context.Context, reason pushReason, full bool, batch *syncpkg.WatchBatch) error
	wake <-chan struct{}
}

func (w archivePushWatch) run(ctx context.Context) error {
	lane := &archiveSyncLane{
		label:    w.label,
		engine:   w.engine,
		retry:    w.debounce,
		maxRetry: w.interval,
		requests: make(chan struct{}, 1),
	}
	lane.fullPending.Store(w.full)
	loop, stopLoop := newArchivePushLoop(
		w.hooks, w.label, w.debounce, w.interval,
		func(c context.Context, r pushReason, batch *syncpkg.WatchBatch) error {
			return lane.push(c, r, batch, w.push)
		},
	)
	defer stopLoop()
	lane.loop = loop

	callback := func(c context.Context, batch syncpkg.WatchBatch) error {
		return notifyPushForWatchBatchWithConfig(c, loop, w.appCfg, batch)
	}
	options := syncpkg.WatcherOptions{OnCoverageDegraded: loop.NotifyCoverageDegraded}
	unwatchedCoverage := fmt.Sprintf("relying on the %s floor for coverage", w.interval)
	if w.engine != nil {
		poller := newArchivePushUnwatchedPoller(ctx, w.hooks, w.engine)
		defer poller.Stop()
		callback = lane.watchCallback(w.appCfg)
		options = archivePushWatchWatcherOptions(lane.notifyCoverageDegraded, poller)
		unwatchedCoverage = fmt.Sprintf("polling every %s", unwatchedPollInterval)
	}
	stopWatcher, openDispatch, unwatchedDirs, flushPending := startArchivePushWatcher(
		w.hooks, w.appCfg, w.engine, callback, options,
	)
	stopWatcher = stdsync.OnceFunc(stopWatcher)
	defer stopWatcher()
	// Stop the watcher as soon as shutdown starts. A watcher batch can hold
	// the sync lock, and the final push waits for that lock.
	defer context.AfterFunc(ctx, stopWatcher)()
	lane.openDispatch = stdsync.OnceFunc(openDispatch)
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"%s: %d root(s) not watched; %s",
			w.label, len(unwatchedDirs), unwatchedCoverage,
		)
	}

	wake := w.wake
	if w.engine != nil {
		manager, err := archivesource.NewManager(ctx, w.appCfg, w.database, w.engine, flushPending)
		if err != nil {
			return fmt.Errorf("configuring archive sources: %w", err)
		}
		defer manager.Close()
		lane.archiveSources = manager
		lane.startupSync = runPGWatchStartupSync
		if w.hooks != nil && w.hooks.startupSync != nil {
			lane.startupSync = w.hooks.startupSync
		}
		lane.full = w.full
		lane.database = w.database
		startDatabaseID, err := w.database.GetDatabaseID(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("reading archive database id: %w", err)
		}
		lane.startDatabaseID = startDatabaseID
		lane.syncStartup(ctx)
		if ctx.Err() != nil {
			return nil
		}

		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		laneDone := make(chan struct{})
		go func() {
			defer close(laneDone)
			lane.run(ctx, ticker.C, w.wake)
		}()
		defer func() { <-laneDone }()
		// The lane turns lifecycle wakes into syncs, and a sync that writes
		// the archive queues a push.
		wake = nil
	}

	if lane.canPush() {
		// The startup push runs on this goroutine before the loop starts, so
		// it skips the change debounce. A failed startup push stays queued
		// and retries on the debounce.
		loop.enqueueUnscoped(reasonStartup, nil, false)
		loop.runNextTask(ctx, false)
		if ctx.Err() != nil {
			// Stopping during the first push skips the final push, which would
			// repeat the push just interrupted. A local owner still archives
			// the changes the watcher had not handed over.
			if w.engine != nil {
				flushCtx, cancel := loop.flushContext()
				defer cancel()
				if err := lane.syncBeforeExit(flushCtx, ""); err != nil {
					log.Printf("%s: %v", w.label, err)
				}
			}
			return nil
		}
	}
	loop.RunWithWake(ctx, wake)
	return nil
}

// archiveSyncLane tracks when the archive is ready to push and, when this
// process owns the archive, writes it. Watcher batches sync on the watcher's
// dispatch goroutine; the lane goroutine runs authoritative syncs on the floor
// interval, on lifecycle wake, and after the watcher loses coverage. A sync
// that writes the archive marks the push loop dirty.
type archiveSyncLane struct {
	label string
	// engine is nil when the daemon owns the archive.
	engine         *syncpkg.Engine
	archiveSources *archivesource.Manager
	loop           *pushLoop
	// retry spaces syncs while no complete local sync has run, so a failed
	// startup sync does not pause remote pushes for a whole floor interval.
	// It doubles after each further failure, up to maxRetry: a retry can
	// rebuild the whole archive.
	retry, maxRetry time.Duration
	startupFailures int
	openDispatch    func()
	requests        chan struct{}
	// startupSync runs the startup sync, including any archive rebuild it
	// needs. The lane retries it until one completes. full is the watch's
	// --full request. Local owners only.
	startupSync func(context.Context, *syncpkg.Engine, bool) (bool, error)
	full        bool
	// database and startDatabaseID detect a rebuild since the watch started:
	// a rebuild gives the archive a new database ID. Local owners only.
	database        *db.DB
	startDatabaseID string
	// ready is set once the archive has caught up: after a complete local
	// sync, or after the first successful push when the daemon owns the
	// archive and syncs before it pushes. Watcher dispatch opens then. A local
	// owner also holds pushes until then: a full push deletes remote rows
	// missing locally, so it must never read an archive that a failed startup
	// sync left incomplete.
	ready atomic.Bool
	// fullPending asks the next push to resend every session. The first
	// push that reaches the destination clears it, even if that push fails:
	// each mirror records an unfinished full push and resumes it on the next
	// push, and asking for a full push again would start it over. A push that
	// fails before it reaches the destination recorded nothing to resume, so
	// the request stays.
	fullPending atomic.Bool
}

// markReady opens watcher dispatch the first time the archive catches up and
// reports whether this call did so.
func (l *archiveSyncLane) markReady() bool {
	if !l.ready.CompareAndSwap(false, true) {
		return false
	}
	l.openDispatch()
	return true
}

// canPush reports whether a push may run now. A daemon owner always may: its
// push syncs the archive first.
func (l *archiveSyncLane) canPush() bool {
	return l.engine == nil || l.ready.Load()
}

func (l *archiveSyncLane) push(
	ctx context.Context,
	reason pushReason,
	batch *syncpkg.WatchBatch,
	push func(context.Context, pushReason, bool, *syncpkg.WatchBatch) error,
) error {
	if !l.canPush() {
		// Skipping instead of failing keeps the loop from retrying on its
		// debounce. The sync that sets ready queues a push.
		log.Printf("%s: push (%s) skipped: waiting for a local sync that "+
			"finishes without errors", l.label, reason)
		return nil
	}
	if l.engine != nil && reason == reasonShutdown {
		if err := l.syncBeforeExit(ctx, " before the final push"); err != nil {
			return err
		}
	}
	if l.engine != nil {
		// Wait for an in-progress sync and flush deferred signal recomputes,
		// so the push copies settled sessions. Local sync keeps writing
		// during the push; those writes reach the next push because each
		// push takes its cutoff under the archive write lock.
		if err := l.engine.SettleForPush(ctx); err != nil {
			return fmt.Errorf("starting push: %w", err)
		}
	}
	full := l.fullPending.Swap(false)
	if err := push(ctx, reason, full, batch); err != nil {
		if _, notStarted := errors.AsType[storage.PushNotStartedError](err); full && notStarted {
			l.fullPending.Store(true)
		}
		return err
	}
	if l.engine == nil {
		l.markReady()
	}
	return nil
}

// requestSync asks the lane for an authoritative sync. Requests coalesce.
func (l *archiveSyncLane) requestSync() {
	select {
	case l.requests <- struct{}{}:
	default:
	}
}

// notifyCoverageDegraded catches the archive up with an authoritative sync
// and logs the loss; the unwatched-root poller takes over the roots.
func (l *archiveSyncLane) notifyCoverageDegraded(roots []string) error {
	l.requestSync()
	return l.loop.NotifyCoverageDegraded(roots)
}

func (l *archiveSyncLane) watchCallback(cfg config.Config) syncpkg.WatchCallback {
	return func(ctx context.Context, batch syncpkg.WatchBatch) error {
		if watchBatchIsAffirmativelyNonData(ctx, cfg, batch) {
			return nil
		}
		err := syncWatchBatch(ctx, l.engine, batch, func() watchRecoveryScope {
			return probeWatchRecoveryScope(cfg)
		})
		// A failed batch can still have written some sessions.
		l.loop.NotifyDirty()
		return err
	}
}

func (l *archiveSyncLane) run(
	ctx context.Context, tick <-chan time.Time, wake <-chan struct{},
) {
	for {
		if !l.wait(ctx, tick, &wake) {
			return
		}
		if l.ready.Load() {
			l.syncAll(ctx)
		} else if l.syncStartup(ctx) {
			// Queue the push that startup skipped.
			l.loop.NotifyDirty()
		}
	}
}

// wait blocks until the next sync is due and reports false when ctx ends.
func (l *archiveSyncLane) wait(
	ctx context.Context, tick <-chan time.Time, wake *<-chan struct{},
) bool {
	var retry <-chan time.Time
	if !l.ready.Load() {
		timer := time.NewTimer(l.retry)
		defer timer.Stop()
		retry = timer.C
	}
	for {
		select {
		case <-ctx.Done():
			return false
		case <-retry:
		case <-tick:
		case <-l.requests:
		case _, ok := <-*wake:
			if !ok {
				*wake = nil
				continue
			}
		}
		return true
	}
}

// syncStartup runs the startup sync and marks the archive ready when it
// completes, reporting whether this call did so. Retries run the same sync,
// so an archive rebuild that an earlier attempt did not finish runs again.
func (l *archiveSyncLane) syncStartup(ctx context.Context) bool {
	rebuilt, err := l.archiveRebuilt(ctx)
	var didResync bool
	if err == nil {
		// Once a rebuild has replaced the archive, --full on a retry would
		// rebuild it again.
		didResync, err = l.startupSync(ctx, l.engine, l.full && !rebuilt)
	}
	if err == nil {
		// A rebuild can replace the archive and still report failures, so
		// check again after the sync.
		rebuilt, err = l.archiveRebuilt(ctx)
	}
	if err == nil && l.archiveSources != nil {
		// Local ingestion is complete. Let pending watcher batches run
		// between attached sources while remote pushes still wait.
		l.openDispatch()
		_, err = l.archiveSources.Drain(ctx, nil)
		if err != nil && ctx.Err() == nil && !l.database.NeedsResync() {
			log.Printf("%s: attachment ingestion warning: %v", l.label, err)
			err = nil
		}
	}
	if ctx.Err() != nil {
		return false
	}
	if err != nil {
		if l.startupFailures > 0 {
			l.retry = min(2*l.retry, max(l.maxRetry, l.retry))
		}
		l.startupFailures++
		log.Printf("%s: startup sync failed: %v. Remote pushes stay paused. "+
			"Next retry in %s.", l.label, err, l.retry)
		return false
	}
	// Only a full push removes remote rows that a rebuild dropped locally.
	if didResync || rebuilt {
		l.fullPending.Store(true)
	}
	return l.markReady()
}

// archiveRebuilt reports whether a rebuild replaced the archive since the
// watch started.
func (l *archiveSyncLane) archiveRebuilt(ctx context.Context) (bool, error) {
	id, err := l.database.GetDatabaseID(ctx)
	if err != nil {
		return false, fmt.Errorf("reading archive database id: %w", err)
	}
	return id != l.startDatabaseID, nil
}

// syncAll runs a periodic sync after the archive is ready.
func (l *archiveSyncLane) syncAll(ctx context.Context) {
	stats := l.engine.SyncAll(ctx, nil)
	if stats.ProcessingComplete() && l.archiveSources != nil {
		archiveStats, err := l.archiveSources.DrainScheduled(ctx)
		stats.Merge(archiveStats)
		if err != nil && ctx.Err() == nil {
			log.Printf("%s: %v", l.label, err)
		}
	}
	if ctx.Err() != nil {
		return
	}
	switch {
	case !stats.AuthoritativeDiscoveryComplete():
		log.Printf("%s: local sync could not list every session. "+
			"The next local sync will try again.", l.label)
	case !stats.ProcessingComplete():
		log.Printf("%s: local sync failed to process some sessions. "+
			"The next local sync will try again.", l.label)
	}
	// A sync that wrote nothing leaves the push loop alone.
	if stats.ChangedArchive() {
		l.loop.NotifyDirty()
	}
}

// syncBeforeExit runs the last local sync of a stopping watch. The watcher
// has stopped, so changes it had not handed over reach the archive only
// through this sync. what ends the progress line.
func (l *archiveSyncLane) syncBeforeExit(ctx context.Context, what string) error {
	log.Printf("%s: stopping. Syncing remaining changes%s.", l.label, what)
	l.engine.SyncAll(ctx, nil)
	if ctx.Err() != nil {
		return errors.New("the final sync did not finish in time. The next " +
			"run will pick up changes made since the last sync")
	}
	return nil
}
