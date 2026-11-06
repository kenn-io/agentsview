package main

import (
	"context"
	"errors"
	"log"
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
	// attached drains configured archive sources during an idle catch-up.
	attached *archivesource.Manager

	// applyMu serializes reloads. Each reload reads the saved configuration
	// under it, so the last one to run applies the latest settings.
	applyMu sync.Mutex
	// mu guards the fields below.
	mu              sync.Mutex
	watchers        *ingestionWatchers
	dispatchOpen    bool
	stopLive        func()
	stopped         bool
	applying        sync.WaitGroup
	pending         bool
	catchup         chan struct{}
	catchupDeadline time.Time
	liveEnabled     bool
}

// ingestionWatchers is one generation of change detection for a fixed
// provider set.
type ingestionWatchers struct {
	stopWatcher  func()
	openDispatch func()
	queueRetry   func(agentsync.WatchBatch)
	flushPending func(context.Context) error
	poller       *sharedUnwatchedPollCoordinator
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
	if d.watchers != nil {
		d.watchers.openDispatch()
	}
}

// QueueWatchRetry hands a batch to the current watcher's retry queue.
func (d *daemonIngestion) QueueWatchRetry(batch agentsync.WatchBatch) {
	d.mu.Lock()
	watchers := d.watchers
	d.mu.Unlock()
	// The wake full pass covers batches dropped while collection is suspended.
	if watchers != nil {
		watchers.queueRetry(batch)
	}
}

// FlushPending acknowledges fresh work from the current watcher generation.
func (d *daemonIngestion) FlushPending(ctx context.Context) error {
	d.mu.Lock()
	watchers, stopped := d.watchers, d.stopped
	d.mu.Unlock()
	if stopped {
		return errIngestionStopped
	}
	if watchers == nil {
		return nil
	}
	return watchers.flushPending(ctx)
}

// StartLiveActivity starts polling live agent processes for the current
// provider set. Reloads restart it with the new set.
func (d *daemonIngestion) StartLiveActivity() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.liveEnabled = true
	if d.stopped || d.watchers == nil || d.stopLive != nil {
		return
	}
	d.stopLive = startLiveActivityPoller(
		d.ctx, d.Config(), d.database, d.engine, d.idleTracker,
	)
}

// Active admits scheduled work while collection is installed.
func (d *daemonIngestion) Active() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.stopped && d.watchers != nil
}

func (d *daemonIngestion) Pending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pending
}

// Suspend marks the archive pending before releasing collection resources;
// an in-flight catch-up keeps them.
func (d *daemonIngestion) Suspend() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.watchers == nil || d.catchup != nil {
		return
	}
	d.pending = true
	watchers, stopLive := d.watchers, d.stopLive
	d.watchers, d.stopLive = nil, nil
	d.applying.Go(func() {
		if stopLive != nil {
			stopLive()
		}
		watchers.stop()
	})
}

// EnsureFresh shares one catch-up and its wait deadline across readers.
func (d *daemonIngestion) EnsureFresh(ctx context.Context) bool {
	finish, ok := d.idleTracker.BeginWork()
	if !ok {
		return false
	}
	d.mu.Lock()
	if !d.pending || d.stopped {
		fresh := !d.pending && !d.stopped
		d.mu.Unlock()
		finish()
		return fresh
	}
	done, started := d.catchup, d.catchup == nil
	if started {
		done = d.registerCatchupLocked()
		d.applying.Go(func() { d.resume(done, finish) })
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

func (d *daemonIngestion) restoreCollection() error {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	d.mu.Lock()
	active, stopped := d.watchers != nil, d.stopped
	d.mu.Unlock()
	if stopped {
		return errIngestionStopped
	}
	if !active {
		watchers := d.startWatchers(d.Config())
		d.mu.Lock()
		if d.stopped {
			d.mu.Unlock()
			watchers.stop()
			return errIngestionStopped
		}
		d.installWatchersLocked(watchers, d.liveEnabled)
		d.mu.Unlock()
	}
	return nil
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
		if d.pending && d.catchup == nil && !d.stopped {
			done := d.registerCatchupLocked()
			defer d.finishCatchup(done)
		}
		d.mu.Unlock()
		if err := d.restoreCollection(); err != nil {
			return agentsync.SyncStats{}, err
		}
		stats, err := runner(ctx, progress)
		if err == nil {
			d.mu.Lock()
			d.pending = false
			d.mu.Unlock()
		}
		return stats, err
	}
}

func (d *daemonIngestion) resume(done chan struct{}, finish func()) {
	defer finish()
	err := d.restoreCollection()
	if err == nil {
		stats := syncAllReleasingStartupMaintenance(d.ctx, d.engine, nil)
		if !stats.ProcessingComplete() {
			log.Printf("idle catch-up incomplete: %d failed, aborted=%v", stats.Failed, stats.Aborted)
		}
		if d.attached != nil {
			_, err = d.attached.DrainScheduled(d.ctx)
		}
	}
	if err != nil {
		log.Printf("idle catch-up incomplete: %v", err)
	}
	d.mu.Lock()
	d.pending = false
	d.mu.Unlock()
	d.finishCatchup(done)
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
	watchers, stopLive := d.watchers, d.stopLive
	d.stopLive = nil
	d.mu.Unlock()
	if stopLive != nil {
		stopLive()
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
	active := d.watchers != nil && !d.stopped
	d.mu.Unlock()
	var watchers *ingestionWatchers
	if active {
		watchers = d.startWatchers(next)
	}
	d.engine.ReconfigureSources(engineSourceConfig(next))
	d.mu.Lock()
	old, oldLive := d.watchers, d.stopLive
	if watchers != nil && old != nil && !d.stopped {
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
		old.stop()
	}
	if oldLive != nil {
		oldLive()
	}

	log.Printf("applied session provider settings: disabled=%v",
		next.DisabledAgents)
}

// installWatchersLocked makes watchers current. Dispatch opens only once
// startup has opened it, so a replacement never delivers batches early.
func (d *daemonIngestion) installWatchersLocked(watchers *ingestionWatchers, live bool) {
	d.watchers = watchers
	if d.dispatchOpen {
		watchers.openDispatch()
	}
	if live {
		d.stopLive = startLiveActivityPoller(d.ctx, d.Config(), d.database, d.engine, d.idleTracker)
	}
}

func (d *daemonIngestion) startWatchers(cfg config.Config) *ingestionWatchers {
	poller := newUnwatchedPollCoordinator(d.ctx, d.engine, d.idleTracker)
	stopWatcher, openDispatch, _, queueRetry, flushPending := startFileWatcher(
		cfg, d.engine, d.syncWatchBatch,
		agentsync.WatcherOptions{
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
		stopWatcher:  stopWatcher,
		openDispatch: openDispatch,
		queueRetry:   queueRetry,
		flushPending: flushPending,
		poller:       poller,
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
