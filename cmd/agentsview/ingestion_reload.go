package main

import (
	"context"
	"errors"
	"log"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/archivesource"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

const ingestionWaitBudget = 2 * time.Second

// readFlushMinGap is the shortest time between syncs started for readers.
const readFlushMinGap = time.Second

// collectionPaused is the gap that keeps background polling from running.
const collectionPaused = time.Duration(math.MaxInt64)

var errIngestionStopped = errors.New("daemon ingestion is shutting down")

// daemonIngestion owns the daemon parts that follow the configured session
// providers: the sync engine's provider set, the file watcher, unwatched-root
// polling, and the live activity poller. Reload applies newly saved provider
// settings to all of them without restarting the daemon.
type daemonIngestion struct {
	ctx         context.Context
	engine      *agentsync.Engine
	database    *db.DB
	idleTracker *server.IdleTracker
	// load rebuilds the configuration the same way daemon startup and the
	// sync workers do: defaults, config file, environment, then serve flags.
	load func() (config.Config, error)

	cfg atomic.Pointer[config.Config]
	// syncInterval is shared by every watcher generation, so a shorter
	// interval requested by the server survives provider reloads. While
	// nothing holds it, the watcher syncs at the background interval.
	syncInterval agentsync.IntervalOverride
	// attached drains configured archive sources after an idle pause.
	attached *archivesource.Manager
	// scheduled runs the periodic reconciliation pass. A pause that skipped
	// one runs it when the pause ends.
	scheduled func(context.Context)

	// applyMu serializes reloads. Each reload reads the saved configuration
	// under it, so the last one to run applies the latest settings.
	applyMu sync.Mutex
	// mu guards the fields below.
	mu           sync.Mutex
	watchers     *ingestionWatchers
	dispatchOpen bool
	live         *liveActivity
	stopped      bool
	applying     sync.WaitGroup
	// paused holds watcher dispatch and polling after client inactivity. The
	// watcher stays attached and keeps collecting changed paths.
	paused bool
	// pending marks an archive that has not yet synced what a pause collected.
	pending bool
	// missedScheduled records a periodic pass skipped during the pause.
	missedScheduled bool
	catchup         chan struct{}
	catchupDeadline time.Time
	lastCatchup     time.Time
	liveEnabled     bool
}

// ingestionWatchers is one generation of change detection for a fixed
// provider set.
type ingestionWatchers struct {
	stopWatcher   func()
	openDispatch  func()
	pauseDispatch func()
	queueRetry    func(agentsync.WatchBatch)
	flushPending  func(context.Context) error
	poller        *sharedUnwatchedPollCoordinator
}

func (w *ingestionWatchers) stop() {
	// Stop the watcher first so it cannot hand the poller new obligations.
	w.stopWatcher()
	w.poller.Stop()
}

func newDaemonIngestion(
	ctx context.Context,
	cfg config.Config,
	engine *agentsync.Engine,
	database *db.DB,
	idleTracker *server.IdleTracker,
	load func() (config.Config, error),
) *daemonIngestion {
	d := &daemonIngestion{
		ctx:         ctx,
		engine:      engine,
		database:    database,
		idleTracker: idleTracker,
		load:        load,
	}
	d.cfg.Store(&cfg)
	d.watchers = d.startWatchers(cfg)
	return d
}

// SyncIntervalOverride returns the override that can shorten the minimum
// interval between watcher-triggered syncs.
func (d *daemonIngestion) SyncIntervalOverride() *agentsync.IntervalOverride {
	return &d.syncInterval
}

// Config returns the configuration whose provider set the daemon currently
// runs.
func (d *daemonIngestion) Config() config.Config {
	return *d.cfg.Load()
}

// OpenWatcherDispatch lets the current watcher, and every later replacement,
// deliver change batches. Startup calls it once the initial pass reconciles.
func (d *daemonIngestion) OpenWatcherDispatch() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dispatchOpen = true
	if !d.paused {
		d.watchers.openDispatch()
	}
}

// QueueWatchRetry hands a batch to the current watcher's retry queue.
func (d *daemonIngestion) QueueWatchRetry(batch agentsync.WatchBatch) {
	d.mu.Lock()
	watchers := d.watchers
	d.mu.Unlock()
	watchers.queueRetry(batch)
}

// FlushPending acknowledges fresh work from the current watcher generation.
func (d *daemonIngestion) FlushPending(ctx context.Context) error {
	d.mu.Lock()
	watchers, stopped := d.watchers, d.stopped
	d.mu.Unlock()
	if stopped {
		return errIngestionStopped
	}
	return watchers.flushPending(ctx)
}

// StartLiveActivity starts polling live agent processes for the current
// provider set. Reloads restart it with the new set.
func (d *daemonIngestion) StartLiveActivity() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.liveEnabled = true
	if d.stopped || d.live != nil {
		return
	}
	d.live = startLiveActivityPoller(
		d.ctx, d.Config(), d.database, d.engine, d.idleTracker, d.collectionGap,
	)
}

// collectionGap is the shortest time a background poller may leave between
// passes: none while someone holds a live sync interval, the background sync
// interval while nobody does, and collectionPaused during an idle pause.
func (d *daemonIngestion) collectionGap() time.Duration {
	d.mu.Lock()
	paused := d.paused
	d.mu.Unlock()
	switch {
	case paused:
		return collectionPaused
	case d.syncInterval.Value() > 0:
		return 0
	}
	return d.Config().BackgroundSyncFloor()
}

// Active admits scheduled work while collection is not paused.
func (d *daemonIngestion) Active() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.stopped && !d.paused
}

func (d *daemonIngestion) Pending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pending
}

// setScheduled installs the periodic pass that a pause may owe.
func (d *daemonIngestion) setScheduled(run func(context.Context)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.scheduled = run
}

// AdmitScheduled reports whether the periodic pass may run now. A pass turned
// away by a pause is owed when the pause ends.
func (d *daemonIngestion) AdmitScheduled() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return false
	}
	if d.paused {
		d.missedScheduled = true
		return false
	}
	return true
}

// Suspend pauses collection after client inactivity. The watcher stays
// attached and keeps the set of changed paths, so the next reader syncs those
// paths instead of rediscovering every source. Nothing syncs or polls until
// then. An in-flight catch-up keeps collection running.
func (d *daemonIngestion) Suspend() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.paused || d.catchup != nil {
		return
	}
	d.paused, d.pending = true, true
	d.watchers.pauseDispatch()
}

// EnsureFresh brings the archive up to date for a reader. It ends an idle
// pause, and while the background interval is holding changes back it syncs
// them first. Readers share one catch-up and its wait deadline.
func (d *daemonIngestion) EnsureFresh(ctx context.Context) bool {
	finish, ok := d.idleTracker.BeginWork()
	if !ok {
		return false
	}
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		finish()
		return false
	}
	done, started := d.catchup, false
	if done == nil {
		if !d.pending && !d.readFlushDueLocked() {
			d.mu.Unlock()
			finish()
			return true
		}
		done, started = d.registerCatchupLocked(), true
		d.applying.Go(func() {
			defer finish()
			d.catchUp(d.ctx, false)
			d.finishCatchup(done)
		})
	}
	deadline := d.catchupDeadline
	d.mu.Unlock()
	if !started {
		finish()
	}
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	select {
	case <-waitCtx.Done():
		return false
	case <-done:
		return !d.Pending()
	}
}

// readFlushDueLocked reports whether a read should sync the changes the
// watcher holds. That applies once startup has opened dispatch and only while
// the background interval is what holds them: a live interval already syncs
// as often as a read would. Reads start a sync no more often than a live
// watcher does.
func (d *daemonIngestion) readFlushDueLocked() bool {
	if !d.dispatchOpen || d.syncInterval.Value() > 0 {
		return false
	}
	cfg := d.Config()
	if cfg.BackgroundSyncFloor() <= cfg.SyncInterval {
		return false
	}
	return time.Since(d.lastCatchup) >= max(cfg.SyncInterval, readFlushMinGap)
}

// endPauseLocked reopens watcher delivery after an idle pause. Delivery stays
// closed until startup has opened it.
func (d *daemonIngestion) endPauseLocked() {
	d.paused = false
	if d.dispatchOpen {
		d.watchers.openDispatch()
	}
}

func (d *daemonIngestion) finishCatchup(done chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.catchup = nil
	close(done)
}

func (d *daemonIngestion) registerCatchupLocked() chan struct{} {
	d.catchup, d.catchupDeadline = make(chan struct{}), time.Now().Add(ingestionWaitBudget)
	return d.catchup
}

func (d *daemonIngestion) foregroundSync(runner server.LocalSyncRunner) server.LocalSyncRunner {
	if d == nil {
		return runner
	}
	return func(ctx context.Context, progress func(agentsync.Progress)) (agentsync.SyncStats, error) {
		d.mu.Lock()
		if !d.pending || d.stopped {
			d.mu.Unlock()
			return runner(ctx, progress)
		}
		if d.catchup == nil {
			done := d.registerCatchupLocked()
			defer d.finishCatchup(done)
		}
		// The runner imports attached archives, which first flushes the
		// watcher, so delivery has to be open before it runs.
		d.endPauseLocked()
		d.mu.Unlock()
		stats, err := runner(ctx, progress)
		if err == nil {
			// The full pass covered every source, including the ones the
			// periodic pass and attached sources own. The watcher's set still
			// carries what a full pass leaves alone, such as a source deleted
			// during the pause.
			d.catchUp(ctx, true)
		}
		return stats, err
	}
}

// catchUp syncs the changes the watcher collected and ends an idle pause. It
// does not rediscover the archive: the watcher stayed attached, so its set is
// complete, and a set too large to keep was already promoted to a full sync.
// Sources no watcher covers are caught up by the polls and the periodic pass
// that the pause held back, unless a full pass just ran.
func (d *daemonIngestion) catchUp(ctx context.Context, fullPassRan bool) {
	d.mu.Lock()
	// pending outlives paused when a foreground sync ended the pause and then
	// failed, so it is what says a wake still owes its catch-up.
	watchers, live, owed, open := d.watchers, d.live, d.pending, d.dispatchOpen
	missedScheduled, scheduled := d.missedScheduled, d.scheduled
	d.missedScheduled = false
	d.endPauseLocked()
	d.mu.Unlock()

	var err error
	switch {
	case open:
		err = watchers.flushPending(ctx)
	case owed && !d.engine.StartupReconciled():
		// The startup pass has not run, so no watcher batch may be delivered
		// yet. This reader is the first to need the archive: run that pass.
		if stats := syncAllReleasingStartupMaintenance(ctx, d.engine, nil); !stats.ProcessingComplete() {
			log.Printf("idle catch-up incomplete: %d failed, aborted=%v", stats.Failed, stats.Aborted)
		}
	}
	// Some agents keep their session file open, and not every platform
	// reports growth of an open file. The live-activity poll finds it.
	if open && live != nil && !fullPassRan {
		live.pollNow(ctx)
	}
	if owed && !fullPassRan {
		watchers.poller.requestPoll()
		switch {
		case missedScheduled && scheduled != nil:
			scheduled(ctx)
		case err == nil && d.attached != nil:
			_, err = d.attached.DrainScheduled(ctx)
		}
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("archive catch-up incomplete: %v", err)
	}

	d.mu.Lock()
	d.pending = false
	d.lastCatchup = time.Now()
	// A reload may have installed a new watcher generation meanwhile.
	if d.dispatchOpen && !d.paused {
		d.watchers.openDispatch()
	}
	d.mu.Unlock()
}

// Reload reads the saved configuration and applies it in the background. It
// returns once the configuration loads; applying it waits for any in-flight
// sync pass. Reload does not sync: sessions already on disk under newly
// enabled roots are picked up by the next sync.
func (d *daemonIngestion) Reload(context.Context) (config.Config, error) {
	cfg, err := d.load()
	if err != nil {
		return config.Config{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return config.Config{}, errIngestionStopped
	}
	d.applying.Go(d.applySaved)
	return cfg, nil
}

// Stop waits for in-flight reloads, then stops watchers, polling, and live
// activity polling.
func (d *daemonIngestion) Stop() {
	d.mu.Lock()
	d.stopped = true
	d.mu.Unlock()
	d.applying.Wait()
	d.mu.Lock()
	watchers, live := d.watchers, d.live
	d.live = nil
	d.mu.Unlock()
	if live != nil {
		live.stop()
	}
	if watchers != nil {
		watchers.stop()
	}
}

// applySaved switches the engine, watchers, and polling to the saved
// configuration.
func (d *daemonIngestion) applySaved() {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	if d.ctx.Err() != nil {
		return
	}
	next, err := d.load()
	if err != nil {
		log.Printf("reload session provider settings: %v", err)
		return
	}
	prev := d.Config()
	d.cfg.Store(&next)
	if reflect.DeepEqual(engineSourceConfig(prev), engineSourceConfig(next)) {
		return
	}
	// Register the replacement watcher before the engine switches, so once
	// the engine reports a new root, every later change under it is seen.
	// Registration can take tens of milliseconds, and a session written in
	// that gap would otherwise wait for the next full sync. The watcher only
	// collects until dispatch opens below, so its first batch is still
	// handled by the new provider set. It also starts before the old watcher
	// stops, so no change falls between them; a change seen by both is
	// synced twice, which is harmless.
	d.mu.Lock()
	active := !d.stopped
	d.mu.Unlock()
	var watchers *ingestionWatchers
	if active {
		watchers = d.startWatchers(next)
	}
	d.engine.ReconfigureSources(engineSourceConfig(next))
	d.mu.Lock()
	old, oldLive := d.watchers, d.live
	flushOld := false
	if watchers != nil && !d.stopped {
		// Stopping the old watcher discards the changes it still holds. An
		// open watcher syncs them first. A paused one cannot, so the
		// replacement owes a full pass when the pause ends.
		flushOld = d.dispatchOpen && !d.paused
		if d.paused {
			watchers.queueRetry(agentsync.WatchBatch{FullSync: true})
		}
		d.installWatchersLocked(watchers, oldLive != nil)
		watchers = nil
	} else {
		old, oldLive = nil, nil
	}
	d.mu.Unlock()
	if watchers != nil {
		watchers.stop()
	}
	if old != nil {
		if flushOld {
			if err := old.flushPending(d.ctx); err != nil && d.ctx.Err() == nil {
				log.Printf("syncing changes held before provider settings changed: %v", err)
			}
		}
		old.stop()
	}
	if oldLive != nil {
		oldLive.stop()
	}

	log.Printf("applied session provider settings: disabled=%v",
		next.DisabledAgents)
}

// installWatchersLocked makes watchers current. Dispatch opens only once
// startup has opened it, so a replacement never delivers batches early.
func (d *daemonIngestion) installWatchersLocked(watchers *ingestionWatchers, live bool) {
	d.watchers = watchers
	if d.dispatchOpen && !d.paused {
		watchers.openDispatch()
	}
	if live {
		d.live = startLiveActivityPoller(
			d.ctx, d.Config(), d.database, d.engine, d.idleTracker, d.collectionGap,
		)
	}
}

func (d *daemonIngestion) startWatchers(cfg config.Config) *ingestionWatchers {
	poller := newUnwatchedPollCoordinator(d.ctx, d.engine, d.idleTracker, d.collectionGap)
	watcher := startFileWatcher(
		cfg, cfg.BackgroundSyncFloor(), d.engine, d.syncWatchBatch,
		agentsync.WatcherOptions{
			MinIntervalOverride: &d.syncInterval,
			// The background interval must not slow the retry of a failed
			// sync for someone who is watching.
			RetryInterval: cfg.SyncInterval,
			OnCoverageDegraded: func(degradedRoots []string) error {
				scopes := make([]pollingScope, 0, len(degradedRoots))
				for _, r := range degradedRoots {
					scopes = append(scopes, pollingScope{Root: r})
				}
				return poller.AddObligation(pollingObligation{
					Key: "watcher-fallback", Scopes: scopes,
				})
			},
			OnPollingRequired: func(obligation agentsync.PollingObligation) error {
				return poller.AddObligation(syncObligationToPoller(obligation))
			},
			OnPollingReleased: poller.RemoveObligation,
		},
	)
	return &ingestionWatchers{
		stopWatcher:   watcher.stop,
		openDispatch:  watcher.openDispatch,
		pauseDispatch: watcher.pauseDispatch,
		queueRetry:    watcher.queueRetry,
		flushPending:  watcher.flushPending,
		poller:        poller,
	}
}

func (d *daemonIngestion) syncWatchBatch(
	_ context.Context, batch agentsync.WatchBatch,
) error {
	done, ok := d.idleTracker.BeginWork()
	if !ok {
		return context.Canceled
	}
	defer done()
	// The serve ctx reaches watcher-driven syncs so SIGTERM can interrupt
	// database reconciliation before Stop waits for it.
	return syncWatchBatch(d.ctx, d.engine, batch, func() watchRecoveryScope {
		return probeWatchRecoveryScope(d.Config())
	})
}

func engineSourceConfig(cfg config.Config) agentsync.SourceConfig {
	return agentsync.SourceConfig{
		AgentDirs:        cfg.AgentDirs,
		SourceMachines:   cfg.SourceMachines,
		ProviderMetadata: cfg.ProviderMetadata,
		DisabledAgents:   cfg.DisabledAgents,
	}
}
