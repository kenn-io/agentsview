package main

import (
	"context"
	"errors"
	"log"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/poller"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

const (
	ingestionWaitBudget   = 2 * time.Second
	ingestionCatchupLimit = 5 * time.Minute
)

var errIngestionStopped = errors.New("daemon ingestion is shutting down")

// daemonIngestion owns the daemon parts that follow the configured session
// providers: the sync engine's provider set, the file watcher, unwatched-root
// polling, and the live activity poller. Reload applies newly saved provider
// settings to all of them without restarting the daemon.
type daemonIngestion struct {
	ctx         context.Context
	cancel      context.CancelFunc
	engine      *agentsync.Engine
	database    *db.DB
	idleTracker *server.IdleTracker
	// load rebuilds the configuration the same way daemon startup and the
	// sync workers do: defaults, config file, environment, then serve flags.
	load func() (config.Config, error)

	cfg atomic.Pointer[config.Config]

	// applyMu serializes reloads. Each reload reads the saved configuration
	// under it, so the last one to run applies the latest settings.
	applyMu sync.Mutex
	// mu guards the fields below.
	mu             sync.Mutex
	generation     *ingestionGeneration
	dispatchOpen   bool
	stopped        bool
	applying       sync.WaitGroup
	pending        bool
	resumed        chan struct{}
	wake           chan struct{}
	liveEnabled    bool
	catchupImports func(context.Context) error
}

type ingestionGeneration struct {
	ctx      context.Context
	cancel   context.CancelFunc
	watchers *ingestionWatchers
	stopLive func()
	recovery *poller.Scheduler
	attempt  chan struct{} // guarded by daemonIngestion.mu; nil during retry cooldown
}

func (g *ingestionGeneration) stop() {
	g.cancel()
	if g.stopLive != nil {
		g.stopLive()
	}
	g.watchers.stop()
	if g.recovery != nil {
		g.recovery.Wait()
	}
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
	ctx, cancel := context.WithCancel(ctx)
	d := &daemonIngestion{
		cancel:      cancel,
		ctx:         ctx,
		engine:      engine,
		database:    database,
		idleTracker: idleTracker,
		load:        load,
	}
	d.cfg.Store(&cfg)
	d.generation = d.newGeneration(cfg)
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
	if d.generation != nil {
		d.generation.watchers.openDispatch()
	}
}

// QueueWatchRetry hands a batch to the current watcher's retry queue.
func (d *daemonIngestion) QueueWatchRetry(batch agentsync.WatchBatch) {
	d.mu.Lock()
	generation := d.generation
	d.mu.Unlock()
	if generation != nil {
		generation.watchers.queueRetry(batch)
	}
}

// FlushPending acknowledges fresh work from the current watcher generation.
func (d *daemonIngestion) FlushPending(ctx context.Context) error {
	d.mu.Lock()
	generation, stopped := d.generation, d.stopped
	d.mu.Unlock()
	if stopped {
		return errIngestionStopped
	}
	if generation == nil {
		return nil
	}
	return generation.watchers.flushPending(ctx)
}

// StartLiveActivity starts polling live agent processes for the current
// provider set. Reloads restart it with the new set.
func (d *daemonIngestion) StartLiveActivity() {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.liveEnabled = true
	if g := d.generation; g != nil && !d.stopped && g.stopLive == nil {
		g.stopLive = startLiveActivityPoller(g.ctx, d.Config(), d.database, d.engine, d.idleTracker)
	}
}

// ActiveContext admits scheduled work to the current collection generation.
func (d *daemonIngestion) ActiveContext() context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.generation == nil || d.stopped {
		return nil
	}
	return d.generation.ctx
}

func (d *daemonIngestion) Pending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pending
}

// Suspend marks the obligation before the idle tracker admits another request.
// Joining canceled workers happens asynchronously so requests keep their budget.
func (d *daemonIngestion) Suspend() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.generation == nil {
		return
	}
	d.pending = true
	d.resumed = make(chan struct{})
	old := d.generation
	old.cancel()
	d.generation = nil
	d.applying.Go(func() { d.stopGeneration(old) })
}

// WaitActive preserves audit deadlines while ingestion is suspended.
func (d *daemonIngestion) WaitActive(ctx context.Context) bool {
	d.mu.Lock()
	paused, resumed := d.generation == nil, d.resumed
	d.mu.Unlock()
	if !paused {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-resumed:
		return true
	}
}

// EnsureFresh joins one owner-bound catch-up. A caller owns only its wait.
func (d *daemonIngestion) EnsureFresh(ctx context.Context) bool {
	waitCtx, cancel := context.WithTimeout(ctx, ingestionWaitBudget)
	defer cancel()
	d.mu.Lock()
	if !d.pending || d.stopped {
		fresh := !d.pending && !d.stopped
		d.mu.Unlock()
		return fresh
	}
	var done chan struct{}
	if d.generation != nil {
		done = d.generation.attempt
	} else {
		if d.wake == nil {
			d.wake = make(chan struct{})
			d.applying.Go(d.resume)
		}
		done = d.wake
	}
	d.mu.Unlock()
	if done == nil {
		return false
	}
	select {
	case <-waitCtx.Done():
		return false
	case <-done:
		return !d.Pending()
	}
}

func (d *daemonIngestion) resume() {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	d.mu.Lock()
	done := d.wake
	if d.stopped {
		d.wake = nil
		close(done)
		d.mu.Unlock()
		return
	}
	d.mu.Unlock()
	g := d.newGeneration(d.Config())
	g.attempt = done
	d.mu.Lock()
	d.wake = nil
	if d.stopped {
		d.mu.Unlock()
		d.stopGeneration(g)
		return
	}
	d.installGeneration(g)
	d.mu.Unlock()
}

func (d *daemonIngestion) recover(g *ingestionGeneration) error {
	d.applyMu.Lock()
	defer d.applyMu.Unlock()
	d.mu.Lock()
	if d.stopped || d.generation != g || !d.pending {
		d.mu.Unlock()
		return nil
	}
	if g.attempt == nil {
		g.attempt = make(chan struct{})
	}
	done := g.attempt
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		close(done)
		g.attempt = nil
		d.mu.Unlock()
	}()
	finish, ok := d.idleTracker.BeginWork()
	if !ok {
		return context.Canceled
	}
	defer finish()
	ctx, cancel := context.WithTimeout(g.ctx, ingestionCatchupLimit)
	defer cancel()
	scope := probeWatchRecoveryScope(d.Config())
	err := syncWatchBatch(ctx, d.engine, agentsync.WatchBatch{FullSync: true}, func() watchRecoveryScope { return scope })
	g.watchers.openDispatch()
	err = errors.Join(err, g.watchers.flushPending(ctx))
	if d.catchupImports != nil {
		err = errors.Join(err, d.catchupImports(ctx))
	}
	found, lookupErr := d.engine.HasActiveSessionSourceInRoots(ctx, scope.deferred)
	if found {
		lookupErr = errors.Join(lookupErr, errors.New("archived session source is unavailable"))
	}
	err = errors.Join(err, lookupErr, ctx.Err())
	if err == nil {
		d.mu.Lock()
		if d.generation == g && !d.stopped {
			d.pending = false
		}
		d.mu.Unlock()
	}
	return err
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

func (d *daemonIngestion) stopGeneration(g *ingestionGeneration) {
	g.stop()
	d.mu.Lock()
	if g.attempt != nil {
		close(g.attempt)
		g.attempt = nil
	}
	d.mu.Unlock()
}

// Stop cancels and joins every owned generation and reload.
func (d *daemonIngestion) Stop() {
	d.mu.Lock()
	d.stopped = true
	d.cancel()
	old := d.generation
	d.generation = nil
	d.mu.Unlock()
	d.applying.Wait()
	if old != nil {
		d.stopGeneration(old)
	}
}

// applySaved switches the engine, watchers, and polling to the saved
// configuration.
func (d *daemonIngestion) applySaved() {
	d.applyMu.Lock()
	var old *ingestionGeneration
	defer func() {
		d.applyMu.Unlock()
		if old != nil {
			d.stopGeneration(old)
		}
	}()
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
	d.mu.Lock()
	active := !d.stopped && d.generation != nil
	d.mu.Unlock()
	var g *ingestionGeneration
	if active {
		// Collect before switching providers so new roots cannot lose writes.
		g = d.newGeneration(next)
	}
	d.engine.ReconfigureSources(engineSourceConfig(next))
	d.mu.Lock()
	if g != nil && !d.stopped && d.generation != nil {
		old = d.generation
		old.cancel()
		d.installGeneration(g)
		g = nil
	}
	d.mu.Unlock()
	if g != nil {
		d.stopGeneration(g)
	}
	log.Printf("applied session provider settings: disabled=%v", next.DisabledAgents)
}

func (d *daemonIngestion) newGeneration(cfg config.Config) *ingestionGeneration {
	ctx, cancel := context.WithCancel(d.ctx)
	return &ingestionGeneration{ctx: ctx, cancel: cancel, watchers: d.startWatchers(ctx, cfg)}
}

// installGeneration runs under mu and applyMu, before any recovery I/O.
func (d *daemonIngestion) installGeneration(g *ingestionGeneration) {
	waking := d.generation == nil
	d.generation = g
	if d.liveEnabled {
		g.stopLive = startLiveActivityPoller(g.ctx, d.Config(), d.database, d.engine, d.idleTracker)
	}
	if d.pending {
		g.recovery = poller.Start(g.ctx, poller.Job{
			Name: "idle-catchup", Interval: unwatchedPollInterval, RunAtStart: true,
			Run: func(context.Context) error { return d.recover(g) },
		})
	} else if d.dispatchOpen {
		g.watchers.openDispatch()
	}
	if waking {
		close(d.resumed)
	}
}

func (d *daemonIngestion) startWatchers(ctx context.Context, cfg config.Config) *ingestionWatchers {
	poller := newUnwatchedPollCoordinator(ctx, d.engine, d.idleTracker)
	stopWatcher, openDispatch, _, queueRetry, flushPending := startFileWatcher(
		cfg, d.engine, func(_ context.Context, batch agentsync.WatchBatch) error { return d.syncWatchBatch(ctx, batch) },
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
	ctx context.Context, batch agentsync.WatchBatch,
) error {
	done, ok := d.idleTracker.BeginWork()
	if !ok {
		return context.Canceled
	}
	defer done()
	// The serve ctx reaches watcher-driven syncs so SIGTERM can interrupt
	// database reconciliation before Stop waits for it.
	return syncWatchBatch(ctx, d.engine, batch, func() watchRecoveryScope {
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
