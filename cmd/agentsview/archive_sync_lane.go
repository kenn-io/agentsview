package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	stdsync "sync"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/config"
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
	hooks              *archivePushWatchHooks
	appCfg             config.Config
	engine             *syncpkg.Engine
	label              string
	debounce, interval time.Duration
	full               bool
	// startupSync runs the first local sync. Used only with engine.
	startupSync func(context.Context, *syncpkg.Engine, bool) (bool, error)
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
		requests: make(chan struct{}, 1),
	}
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
	stopWatcher, openDispatch, unwatchedDirs := startArchivePushWatcher(
		w.hooks, w.appCfg, w.engine, callback, options,
	)
	stopWatcher = stdsync.OnceFunc(stopWatcher)
	defer stopWatcher()
	// Stop the watcher as soon as shutdown starts. A watcher batch can hold
	// the sync lock, and the final push waits for that lock.
	defer context.AfterFunc(ctx, stopWatcher)()
	lane.openDispatch = openDispatch
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"%s: %d root(s) not watched; %s",
			w.label, len(unwatchedDirs), unwatchedCoverage,
		)
	}

	wake := w.wake
	if w.engine == nil {
		lane.fullPending.Store(w.full)
	} else {
		didResync, startupErr := w.startupSync(ctx, w.engine, w.full)
		if startupErr != nil && errors.Is(startupErr, context.Canceled) {
			return nil
		}
		// A resync rebuilt the archive, so the first push that runs resends
		// every session, even when a failed startup sync delays it.
		lane.fullPending.Store(didResync)
		if startupErr != nil {
			log.Printf(
				"%s: startup sync failed: %v; remote pushes stay paused until a "+
					"local sync completes; retrying every %s",
				w.label, startupErr, w.debounce,
			)
		} else if ctx.Err() == nil {
			lane.markReady()
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
	engine *syncpkg.Engine
	loop   *pushLoop
	// retry spaces syncs while no complete local sync has run, so a failed
	// startup sync does not pause remote pushes for a whole floor interval.
	retry        time.Duration
	openDispatch func()
	requests     chan struct{}
	// ready is set once the archive has caught up: after a complete local
	// sync, or after the first successful push when the daemon owns the
	// archive and syncs before it pushes. Watcher dispatch opens then. A local
	// owner also holds pushes until then: a full push deletes remote rows
	// missing locally, so it must never read an archive that a failed startup
	// sync left incomplete.
	ready atomic.Bool
	// fullPending asks the next push to resend every session. The first
	// push that runs clears it, even if that push fails: each mirror
	// records an unfinished full push and resumes it on the next push,
	// and asking for a full push again would start it over.
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
		log.Printf("%s: push (%s) paused until a local sync completes",
			l.label, reason)
		return nil
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
	if err := push(ctx, reason, l.fullPending.Swap(false), batch); err != nil {
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
		l.syncAll(ctx)
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

func (l *archiveSyncLane) syncAll(ctx context.Context) {
	stats := l.engine.SyncAll(ctx, nil)
	if ctx.Err() != nil {
		return
	}
	opened := false
	switch {
	case !stats.AuthoritativeDiscoveryComplete():
		log.Printf("%s: local sync discovery incomplete", l.label)
	case !stats.ProcessingComplete():
		log.Printf("%s: local sync processing incomplete", l.label)
	default:
		opened = l.markReady()
	}
	// The first complete sync queues the push that startup skipped. After
	// that, a sync that wrote nothing leaves the push loop alone.
	if opened || (l.ready.Load() && stats.ChangedArchive()) {
		l.loop.NotifyDirty()
	}
}
