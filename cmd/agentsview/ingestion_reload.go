package main

import (
	"context"
	"errors"
	"log"
	"reflect"
	"sync"
	"sync/atomic"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

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

	mu           sync.Mutex
	watchers     *ingestionWatchers
	dispatchOpen bool
	stopLive     func()
	pending      *config.Config
	stopped      bool

	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// ingestionWatchers is one generation of change detection for a fixed
// provider set.
type ingestionWatchers struct {
	stopWatcher  func()
	openDispatch func()
	queueRetry   func(agentsync.WatchBatch)
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
		wake:        make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	d.cfg.Store(&cfg)
	d.watchers = d.startWatchers(cfg)
	go d.run()
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
	d.watchers.openDispatch()
}

// QueueWatchRetry hands a batch to the current watcher's retry queue.
func (d *daemonIngestion) QueueWatchRetry(batch agentsync.WatchBatch) {
	d.mu.Lock()
	watchers := d.watchers
	d.mu.Unlock()
	watchers.queueRetry(batch)
}

// StartLiveActivity starts polling live agent processes for the current
// provider set. Reloads restart it with the new set.
func (d *daemonIngestion) StartLiveActivity() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.stopLive != nil {
		return
	}
	d.stopLive = startLiveActivityPoller(
		d.ctx, d.Config(), d.database, d.engine, d.idleTracker,
	)
}

// Reload reads the saved configuration and schedules it to be applied. It
// returns once the configuration loads; the engine and watcher swap runs in
// the background because it waits for any in-flight sync pass.
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
	d.pending = &cfg
	select {
	case d.wake <- struct{}{}:
	default:
	}
	return cfg, nil
}

// Stop stops the reload loop, watchers, polling, and live activity polling.
func (d *daemonIngestion) Stop() {
	d.stopOnce.Do(func() { close(d.stop) })
	<-d.done
	d.mu.Lock()
	d.stopped = true
	watchers, stopLive := d.watchers, d.stopLive
	d.stopLive = nil
	d.mu.Unlock()
	if stopLive != nil {
		stopLive()
	}
	watchers.stop()
}

func (d *daemonIngestion) run() {
	defer close(d.done)
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-d.stop:
			return
		case <-d.wake:
		}
		d.mu.Lock()
		next := d.pending
		d.pending = nil
		d.mu.Unlock()
		if next != nil {
			d.apply(*next)
		}
	}
}

func (d *daemonIngestion) apply(next config.Config) {
	prev := d.Config()
	if reflect.DeepEqual(engineSourceConfig(prev), engineSourceConfig(next)) {
		d.cfg.Store(&next)
		return
	}
	// The engine switches first so the new watcher's first batch is handled
	// by the new provider set.
	d.engine.ReconfigureSources(engineSourceConfig(next))
	d.cfg.Store(&next)

	// Start the replacement before stopping the old watcher so no change
	// falls between them; a change seen by both is synced twice, which is
	// harmless.
	watchers := d.startWatchers(next)
	d.mu.Lock()
	old := d.watchers
	d.watchers = watchers
	if d.dispatchOpen {
		watchers.openDispatch()
	}
	oldLive := d.stopLive
	if oldLive != nil {
		d.stopLive = startLiveActivityPoller(
			d.ctx, next, d.database, d.engine, d.idleTracker,
		)
	}
	d.mu.Unlock()
	old.stop()
	if oldLive != nil {
		oldLive()
	}

	added := addedReconcileScopes(prev, next)
	log.Printf(
		"applied session provider settings: disabled=%v new_root_groups=%d",
		next.DisabledAgents, len(added),
	)
	if len(added) == 0 {
		return
	}
	d.idleTracker.Do(func() {
		err := d.engine.ReconcileProviderRootsGrouped(d.ctx, added)
		if err != nil && d.ctx.Err() == nil {
			log.Printf("sync after session provider settings change: %v", err)
		}
	})
}

func (d *daemonIngestion) startWatchers(cfg config.Config) *ingestionWatchers {
	poller := newUnwatchedPollCoordinator(d.ctx, d.engine, d.idleTracker)
	stopWatcher, openDispatch, _, queueRetry := startFileWatcher(
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

// addedReconcileScopes returns the present roots that next ingests and prev
// did not: every root of a newly enabled provider, plus new roots such as an
// added agent home. Roots that are missing, or overlap a missing root of the
// same provider, are left to the watcher and polling so a partial discovery
// never tombstones sessions.
func addedReconcileScopes(
	prev, next config.Config,
) []agentsync.ProviderRootsGroup {
	type agentRoot struct {
		agent parser.AgentType
		root  string
	}
	previous := make(map[agentRoot]struct{})
	for _, factory := range prev.LocalProviderFactories() {
		agent := factory.Definition().Type
		for _, root := range prev.ResolveDirs(agent) {
			previous[agentRoot{agent, root}] = struct{}{}
		}
	}
	present := presentReconcileScopes(next)
	var groups []agentsync.ProviderRootsGroup
	for _, def := range parser.Registry {
		var roots []string
		for _, root := range present[def.Type] {
			if _, ok := previous[agentRoot{def.Type, root}]; !ok {
				roots = append(roots, root)
			}
		}
		if len(roots) > 0 {
			groups = append(groups, agentsync.ProviderRootsGroup{
				Agent: def.Type, Roots: roots,
			})
		}
	}
	return groups
}
