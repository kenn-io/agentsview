package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/config"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// localPushWatch runs a push-watch process that owns the SQLite archive. Local
// ingestion and remote pushes run on separate goroutines: watcher batches and
// periodic syncs write the archive as files change, and the push loop copies
// whatever the archive holds when each push starts. A push that takes an hour
// therefore never delays ingestion.
type localPushWatch struct {
	hooks              *archivePushWatchHooks
	appCfg             config.Config
	engine             *syncpkg.Engine
	label              string
	debounce, interval time.Duration
	full               bool
	startupSync        func(context.Context, *syncpkg.Engine, bool) (bool, error)
	push               func(context.Context, pushReason, bool) error
	wake               <-chan struct{}
}

func (w localPushWatch) run(ctx context.Context) error {
	lane := &archiveSyncLane{
		label:    w.label,
		engine:   w.engine,
		retry:    w.debounce,
		requests: make(chan struct{}, 1),
	}
	loop, stopLoop := newArchivePushLoop(
		w.hooks, w.label, w.debounce, w.interval,
		func(c context.Context, r pushReason, _ *syncpkg.WatchBatch) error {
			return lane.push(c, r, w.push)
		},
	)
	defer stopLoop()
	lane.loop = loop

	poller := newArchivePushUnwatchedPoller(ctx, w.hooks, w.engine)
	defer poller.Stop()

	stopWatcher, openDispatch, unwatchedDirs := startArchivePushWatcher(
		w.hooks, w.appCfg, w.engine,
		lane.watchCallback(w.appCfg),
		archivePushWatchWatcherOptions(lane.notifyCoverageDegraded, poller),
	)
	defer stopWatcher()
	lane.openDispatch = openDispatch
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"%s: %d root(s) not watched; polling every %s",
			w.label, len(unwatchedDirs), unwatchedPollInterval,
		)
	}

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

	if lane.ready.Load() {
		// The startup push runs on this goroutine before the loop starts, so
		// it skips the change debounce. Watcher dispatch is already open, so
		// a long push only delays later pushes.
		loop.enqueueUnscoped(reasonStartup, nil, false)
		loop.runNextTask(ctx, false)
		if ctx.Err() != nil {
			return nil
		}
	}
	loop.Run(ctx)
	return nil
}

// archiveSyncLane owns local archive writes for a push-watch process. Watcher
// batches sync on the watcher's dispatch goroutine; the lane goroutine runs
// authoritative syncs on the floor interval, on lifecycle wake, and after the
// watcher loses coverage. A sync that writes the archive marks the push loop
// dirty.
type archiveSyncLane struct {
	label  string
	engine *syncpkg.Engine
	loop   *pushLoop
	// retry spaces syncs while no complete local sync has run, so a failed
	// startup sync does not pause remote pushes for a whole floor interval.
	retry        time.Duration
	openDispatch func()
	requests     chan struct{}
	// ready is set once a complete local sync has run. Watcher dispatch
	// opens and pushes start only after that: a full push deletes remote
	// rows missing locally, so it must never read an archive that a failed
	// startup sync left incomplete.
	ready atomic.Bool
	// fullPending asks the next push to resend every session. The first
	// push that runs clears it, even if that push fails: each mirror
	// records an unfinished full push and resumes it on the next push,
	// and asking for a full push again would start it over.
	fullPending atomic.Bool
}

// markReady opens watcher dispatch the first time a complete local sync runs
// and reports whether this call did so.
func (l *archiveSyncLane) markReady() bool {
	if !l.ready.CompareAndSwap(false, true) {
		return false
	}
	l.openDispatch()
	return true
}

func (l *archiveSyncLane) push(
	ctx context.Context,
	reason pushReason,
	push func(context.Context, pushReason, bool) error,
) error {
	if !l.ready.Load() {
		// Skipping instead of failing keeps the loop from retrying on its
		// debounce. The sync that sets ready queues a push.
		log.Printf("%s: push (%s) paused until a local sync completes",
			l.label, reason)
		return nil
	}
	return push(ctx, reason, l.fullPending.Swap(false))
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

// archivePushWindow runs one remote push against the archive while local sync
// keeps writing. Before the push it waits for an in-progress sync to finish and
// flushes deferred signal recomputes, so the push copies settled sessions.
// Writes that land during the push reach the next push because each push takes
// its cutoff under the archive write lock (see db.MirrorPushCutoff).
func archivePushWindow(
	engine *syncpkg.Engine,
) func(context.Context, func() error) error {
	return func(_ context.Context, push func() error) error {
		if err := engine.RunExclusiveFlushed(func() error { return nil }); err != nil {
			return fmt.Errorf("starting push: %w", err)
		}
		return push()
	}
}
