package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	stdsync "sync"
	"time"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/pricingrefresh"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// archiveWriteBackend runs pushes from the SQLite archive: in process when
// this CLI owns the archive, or delegated to the daemon that does.
type archiveWriteBackend interface {
	// ReplicaPush runs one push from the archive into target through backend.
	ReplicaPush(
		ctx context.Context,
		backend storage.Replica,
		target storage.ConfiguredReplica,
		cfg ReplicaPushConfig,
		projects []string,
		excludeProjects []string,
	) (storage.PushResult, error)
	DuckDBPush(
		ctx context.Context,
		duckCfg config.DuckDBConfig,
		cfg DuckDBPushConfig,
		projects []string,
		excludeProjects []string,
	) (storage.MirrorPushResult, error)
	DuckDBPushWatch(
		ctx context.Context,
		duckCfg config.DuckDBConfig,
		cfg DuckDBPushConfig,
		projects []string,
		excludeProjects []string,
		debounce time.Duration,
		interval time.Duration,
	) error
	// ReplicaPushWatch runs the long-lived watch loop that pushes into
	// target through backend on change and on a periodic floor.
	ReplicaPushWatch(
		ctx context.Context,
		backend storage.Replica,
		target storage.ConfiguredReplica,
		cfg ReplicaPushConfig,
		projects []string,
		excludeProjects []string,
		debounce time.Duration,
		interval time.Duration,
	) error
}

// archivePushWatchHooks exposes only the slow/nondeterministic owner boundaries
// needed to verify production startup ordering. Nil hooks use the real watcher,
// timers, push implementations, and local startup sync.
type archivePushWatchHooks struct {
	startWatcher func(
		config.Config, *syncpkg.Engine, syncpkg.WatchCallback, syncpkg.WatcherOptions,
	) (func(), func(), []string)
	newLoop func(
		string, time.Duration, time.Duration,
		func(context.Context, pushReason, *syncpkg.WatchBatch) error,
	) (*pushLoop, func())
	duckDBPush func(
		context.Context, pushReason, bool,
	) (storage.MirrorPushResult, error)
	replicaPush func(
		context.Context, pushReason, ReplicaPushConfig,
	) (storage.PushResult, error)
	replicaStartupSync func(
		context.Context, *syncpkg.Engine, bool,
	) (bool, error)
	duckDBStartupSync func(
		context.Context, *syncpkg.Engine, bool,
	) (bool, error)
	newReplicaPusher   func(*syncpkg.Engine) *replicaPusher
	newDuckDBPusher    func(*syncpkg.Engine) *duckDBPusher
	newUnwatchedPoller func(context.Context, unwatchedPollSyncer) unwatchedRootPoller
}

// unwatchedRootPoller owns probe-gated authoritative polling for scopes the
// watcher cannot cover: roots missing at startup, coverage lost at runtime,
// and persistent polling dirs. sharedUnwatchedPollCoordinator implements it.
type unwatchedRootPoller interface {
	AddObligation(pollingObligation) error
	RemoveObligation(string) error
	Stop()
}

// newArchivePushUnwatchedPoller builds the archive push-watch polling owner
// for deferred scopes. The watcher's full recovery and rename promotion defer
// unavailable scopes to their polling probes, and the interval push runs a
// plain SyncAll that never tombstones missed deletions, so without this owner
// a deletion lost while a root was unavailable would stay active in the
// archive (and every pushed mirror) indefinitely after the root returns.
func newArchivePushUnwatchedPoller(
	ctx context.Context,
	hooks *archivePushWatchHooks,
	engine unwatchedPollSyncer,
) unwatchedRootPoller {
	if hooks != nil && hooks.newUnwatchedPoller != nil {
		return hooks.newUnwatchedPoller(ctx, engine)
	}
	ticker := time.NewTicker(unwatchedPollInterval)
	return newUnwatchedPollCoordinatorWithTicks(
		ctx, engine, ticker.C, ticker.Stop, func(work func()) { work() }, nil,
		time.Now, time.After,
	)
}

func startArchivePushWatcher(
	hooks *archivePushWatchHooks,
	cfg config.Config,
	engine *syncpkg.Engine,
	callback syncpkg.WatchCallback,
	options syncpkg.WatcherOptions,
) (func(), func(), []string) {
	if hooks != nil && hooks.startWatcher != nil {
		return hooks.startWatcher(cfg, engine, callback, options)
	}
	stop, open, unwatched, _ := startFileWatcher(cfg, engine, callback, options)
	return stop, open, unwatched
}

func newArchivePushLoop(
	hooks *archivePushWatchHooks,
	label string,
	debounce, interval time.Duration,
	push func(context.Context, pushReason, *syncpkg.WatchBatch) error,
) (*pushLoop, func()) {
	if hooks != nil && hooks.newLoop != nil {
		return hooks.newLoop(label, debounce, interval, push)
	}
	loop, ticker := newPushLoopWithLabel(label, debounce, interval, push)
	return loop, ticker.Stop
}

func archivePushWatchWatcherOptions(
	loop *pushLoop, poller unwatchedRootPoller,
) syncpkg.WatcherOptions {
	return syncpkg.WatcherOptions{
		OnCoverageDegraded: func(roots []string) error {
			// Degraded coverage needs both owners: the poller reconciles
			// the affected roots authoritatively (including tombstoning
			// missed deletions) and the loop re-pushes the refreshed
			// archive on its floor.
			scopes := make([]pollingScope, 0, len(roots))
			for _, r := range roots {
				scopes = append(scopes, pollingScope{Root: r})
			}
			if err := poller.AddObligation(pollingObligation{
				Key: "watcher-fallback", Scopes: scopes,
			}); err != nil {
				return err
			}
			return loop.NotifyCoverageDegraded(roots)
		},
		OnPollingRequired: func(obligation syncpkg.PollingObligation) error {
			scopes := make([]pollingScope, 0, len(obligation.Scopes))
			for _, s := range obligation.Scopes {
				scopes = append(scopes, pollingScope{
					Agent: parser.AgentType(s.Agent),
					Root:  s.Root,
				})
			}
			return poller.AddObligation(pollingObligation{
				Key:    obligation.Key,
				Scopes: scopes,
				Probe:  obligation.Probe,
			})
		},
		OnPollingReleased: poller.RemoveObligation,
	}
}

func archivePushWatchBatchCallback(
	appCfg config.Config,
	loop *pushLoop,
) syncpkg.WatchCallback {
	return func(callbackCtx context.Context, batch syncpkg.WatchBatch) error {
		return notifyPushForWatchBatchWithConfig(
			callbackCtx, loop, appCfg, batch,
		)
	}
}

func completeDuckDBWatchPush(
	res storage.MirrorPushResult, reason pushReason,
) error {
	logDuckDBWatchPushResult(res, reason)
	if res.Errors > 0 {
		return fmt.Errorf("%d session(s) failed to push", res.Errors)
	}
	return nil
}

func completeReplicaWatchPush(
	label, displayName string, res storage.PushResult, reason pushReason,
) error {
	logReplicaWatchPushResult(label, displayName, res, reason)
	if res.Errors > 0 {
		return fmt.Errorf("%d session(s) failed to push", res.Errors)
	}
	return nil
}

func completePushWatchStartup(
	ctx context.Context, initialErr error, loop *pushLoop, openDispatch func(),
) {
	if initialErr == nil {
		if ctx.Err() == nil {
			openDispatch()
		}
		return
	}
	ack := loop.NotifyDirtyWithAck()
	go func() {
		select {
		case <-ctx.Done():
			return
		case err := <-ack:
			if err == nil && ctx.Err() == nil {
				openDispatch()
			}
		}
	}()
}

func notifyPushForWatchBatch(
	ctx context.Context, loop *pushLoop, batch syncpkg.WatchBatch,
) error {
	if !watchBatchNeedsPushAck(batch) {
		loop.NotifyBatch(batch)
		return nil
	}
	ack := loop.NotifyBatchWithAck(batch)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-ack:
		return err
	}
}

func notifyPushForWatchBatchWithConfig(
	ctx context.Context,
	loop *pushLoop,
	cfg config.Config,
	batch syncpkg.WatchBatch,
) error {
	if watchBatchIsAffirmativelyNonData(ctx, cfg, batch) {
		return nil
	}
	return notifyPushForWatchBatch(ctx, loop, batch)
}

type watchPathRelevanceProvider struct {
	provider           parser.Provider
	root               string
	relevanceSupported bool
}

func watchBatchIsAffirmativelyNonData(
	ctx context.Context,
	cfg config.Config,
	batch syncpkg.WatchBatch,
) bool {
	if ctx.Err() != nil || len(batch.Paths) == 0 || batch.FullSync ||
		batch.LostEvents || len(batch.ReconcileRoots) > 0 ||
		len(batch.Renames) > 0 {
		return false
	}

	providers := configuredWatchPathRelevanceProviders(cfg)
	if len(providers) == 0 {
		return false
	}
	for _, path := range batch.Paths {
		if path == "" {
			return false
		}
		path = absRootPath(path)
		matched := false
		for _, candidate := range providers {
			if !pathWithinRoot(path, candidate.root) {
				continue
			}
			matched = true
			if !candidate.relevanceSupported {
				return false
			}
			relevance, err := parser.ResolveChangedPathRelevance(
				ctx, candidate.provider, parser.ChangedPathRequest{
					Path: path, WatchRoot: candidate.root,
				},
			)
			if err != nil || relevance != parser.ChangedPathNonData {
				return false
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func configuredWatchPathRelevanceProviders(
	cfg config.Config,
) []watchPathRelevanceProvider {
	var providers []watchPathRelevanceProvider
	for _, factory := range cfg.LocalProviderFactories() {
		relevanceSupported := factory.Capabilities().Source.ChangedPathRelevance ==
			parser.CapabilitySupported
		roots := cfg.ResolveDirs(factory.Definition().Type)
		for _, root := range roots {
			if root == "" {
				continue
			}
			root = absRootPath(root)
			var provider parser.Provider
			if relevanceSupported {
				provider = factory.NewProvider(parser.ProviderConfig{
					Roots: []string{root},
				})
			}
			providers = append(providers, watchPathRelevanceProvider{
				provider:           provider,
				root:               root,
				relevanceSupported: relevanceSupported,
			})
		}
	}
	return providers
}

func watchBatchNeedsPushAck(batch syncpkg.WatchBatch) bool {
	if batch.FullSync || len(batch.ReconcileRoots) > 0 {
		return true
	}
	for _, rename := range batch.Renames {
		if rename.ItemType != syncpkg.ItemIsFile {
			return true
		}
	}
	return false
}

func watchRecoveryForBatch(
	cfg config.Config, batch *syncpkg.WatchBatch,
) *syncpkg.WatchRecoveryScope {
	if batch == nil || (!batch.FullSync && len(batch.Renames) == 0) {
		return nil
	}
	probed := probeWatchRecoveryScope(cfg)
	deferred := make([]string, 0, len(probed.deferred))
	for root := range probed.deferred {
		deferred = append(deferred, root)
	}
	sort.Strings(deferred)
	return &syncpkg.WatchRecoveryScope{
		AvailableRoots: append([]string(nil), probed.available...),
		DeferredRoots:  deferred,
	}
}

func resolveArchiveWriteBackend(
	ctx context.Context,
	appCfg config.Config,
) (archiveWriteBackend, func(), error) {
	tr, err := ensureTransportContext(
		ctx, &appCfg, transportIntentArchiveWrite, 0,
	)
	if err != nil {
		return nil, nil, err
	}
	if tr.Mode == transportHTTP && !tr.ReadOnly {
		if tr.Runtime != nil && tr.Runtime.NoSync {
			appCfg.NoSync = true
		}
		return daemonArchiveWriteBackend{
			appCfg: appCfg,
			tr:     tr,
		}, func() {}, nil
	}
	if tr.Mode != transportHTTP && tr.DirectReadOnly {
		return nil, nil, errors.New(
			"local daemon owns the SQLite archive but is not " +
				"responding; refusing to write directly",
		)
	}

	database, writeLock, err := openWriteDB(ctx, appCfg)
	if err != nil {
		return nil, nil, err
	}
	backend := &localArchiveWriteBackend{
		appCfg:   appCfg,
		database: database,
	}
	return backend, func() {
		closeWriteDB(database, writeLock)
	}, nil
}

type daemonArchiveWriteBackend struct {
	appCfg     config.Config
	tr         transport
	watchHooks *archivePushWatchHooks
}

// daemonPushHeartbeatInterval bounds how often the daemon-delegated push
// prints an elapsed-time line while waiting. A package var so tests can
// shrink it.
var daemonPushHeartbeatInterval = 30 * time.Second

// startDaemonPushHeartbeat announces that the push runs inside the daemon
// and then prints an elapsed-time line every interval until the returned
// stop func is called. The daemon streams per-session progress once its
// push loop starts, but the phases before it — the daemon-side local sync
// and the remote schema migration — produce no progress events, so without
// a heartbeat a long first push would look hung until the first session
// lands. The caller stops the heartbeat on the first streamed progress
// event; stop is idempotent-unsafe, so wrap it (see daemonPushProgress).
func startDaemonPushHeartbeat(label string) func() {
	return startDaemonPushHeartbeatTo(os.Stdout, label)
}

func startDaemonPushHeartbeatTo(w io.Writer, label string) func() {
	fmt.Fprintf(w, "Pushing to %s via the local daemon...\n", label)
	start := time.Now()
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(daemonPushHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				fmt.Fprintf(w,
					"still pushing to %s via the daemon (%s elapsed)\n",
					label, time.Since(start).Round(time.Second),
				)
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// daemonPushProgress pairs a running heartbeat with a streamed-progress
// renderer: the heartbeat covers the daemon-side phases that emit no
// progress events (local sync, schema migration), and the first streamed
// event silences it for good so heartbeat lines never interleave with the
// in-place progress line. The returned finish func stops the heartbeat (if
// no event ever arrived) and clears the in-place line.
func daemonPushProgress[P any](
	label string, render func(P),
) (onProgress func(P), finish func()) {
	stop := startDaemonPushHeartbeat(label)
	var once stdsync.Once
	stopHeartbeat := func() { once.Do(stop) }
	onProgress = func(p P) {
		stopHeartbeat()
		render(p)
	}
	return onProgress, func() {
		stopHeartbeat()
		fmt.Print("\r\033[K")
	}
}

func (b daemonArchiveWriteBackend) ReplicaPush(
	ctx context.Context,
	backend storage.Replica,
	target storage.ConfiguredReplica,
	cfg ReplicaPushConfig,
	projects []string,
	excludeProjects []string,
) (storage.PushResult, error) {
	operation, err := replicaPushOperation(backend.Name())
	if err != nil {
		return storage.PushResult{}, err
	}
	onProgress, finish := daemonPushProgress(
		backend.DisplayName(), newReplicaPushProgressPrinter(),
	)
	defer finish()
	return postDaemonPush[storage.PushResult](
		ctx, b.tr, b.appCfg.AuthToken, operation,
		apiclient.DaemonPushRequest{
			Full:            cfg.Full,
			Projects:        projects,
			ExcludeProjects: excludeProjects,
			Replica: &apiclient.DaemonReplicaTarget{
				URL:           target.Target.URL,
				Schema:        new(target.Target.Schema),
				MachineName:   target.Target.MachineName,
				AllowInsecure: new(target.Target.AllowInsecure),
				PushVectors:   new(target.Target.PushVectors),
			},
			SyncStateTarget:                new(target.SyncStateTarget()),
			MigrateLegacySyncState:         new(target.MigrateLegacySyncState()),
			NoVectors:                      new(cfg.NoVectors),
			ScopeVectorsToChangedSessions:  new(cfg.ScopeVectorsToChangedSessions),
			LastReconciledVectorGeneration: new(cfg.LastReconciledVectorGeneration),
			WatchBatch:                     generatedWatchBatch(cfg.WatchBatch),
			WatchRecovery:                  generatedWatchRecovery(cfg.WatchRecovery),
		},
		onProgress,
	)
}

func (b daemonArchiveWriteBackend) DuckDBPush(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
) (storage.MirrorPushResult, error) {
	return b.duckDBPush(ctx, duckCfg, cfg, projects, excludeProjects)
}

func (b daemonArchiveWriteBackend) DuckDBPushWatch(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
	debounce time.Duration,
	interval time.Duration,
) error {
	if interval <= 0 {
		interval = defaultWatchInterval
	}
	if debounce <= 0 {
		debounce = defaultWatchDebounce
	}
	push := func(
		pctx context.Context, reason pushReason, full bool,
		_ *syncpkg.WatchBatch,
	) error {
		pushCfg := cfg
		pushCfg.Full = full
		// Watch pushes are automatic: a mirror held by a live serve
		// process defers instead of rebuilding the whole archive on
		// every changed batch, and archive-scale diagnostics are
		// skipped. Push ignores the defer behavior when full is set.
		pushCfg.Automatic = true
		var res storage.MirrorPushResult
		var err error
		if b.watchHooks != nil && b.watchHooks.duckDBPush != nil {
			res, err = b.watchHooks.duckDBPush(pctx, reason, full)
		} else {
			backend := archiveWriteBackend(b)
			cleanup := func() {}
			if reason != reasonStartup {
				backend, cleanup, err = resolveArchiveWriteBackend(
					pctx, b.appCfg,
				)
				if err != nil {
					return err
				}
			}
			defer cleanup()
			res, err = backend.DuckDBPush(
				pctx, duckCfg, pushCfg, projects, excludeProjects,
			)
		}
		if err != nil {
			return err
		}
		return completeDuckDBWatchPush(res, reason)
	}
	loop, stopLoop := newArchivePushLoop(
		b.watchHooks,
		"duckdb watch", debounce, interval,
		func(c context.Context, r pushReason, batch *syncpkg.WatchBatch) error {
			return push(c, r, false, batch)
		},
	)
	defer stopLoop()

	stopWatcher, openDispatch, unwatchedDirs := startArchivePushWatcher(
		b.watchHooks, b.appCfg, nil,
		func(callbackCtx context.Context, batch syncpkg.WatchBatch) error {
			return notifyPushForWatchBatchWithConfig(
				callbackCtx, loop, b.appCfg, batch,
			)
		},
		syncpkg.WatcherOptions{OnCoverageDegraded: loop.NotifyCoverageDegraded},
	)
	defer stopWatcher()
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"duckdb watch: %d root(s) not watched; relying on the %s floor for coverage",
			len(unwatchedDirs), interval,
		)
	}
	initialErr := push(ctx, reasonStartup, cfg.Full, nil)
	if initialErr != nil {
		log.Printf("duckdb watch: initial daemon push failed: %v", initialErr)
	}
	completePushWatchStartup(ctx, initialErr, loop, openDispatch)

	loop.Run(ctx)
	return nil
}

func (b daemonArchiveWriteBackend) duckDBPush(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
) (storage.MirrorPushResult, error) {
	if err := mirrorBackend.ValidatePushTarget(duckCfg); err != nil {
		return storage.MirrorPushResult{}, err
	}
	// Never send a mirror path to the daemon: the daemon pins pushes to its
	// own resolved path and rejects any request naming a different one, and
	// a configured RELATIVE path absolutizes against each process's cwd, so
	// the CLI and daemon can disagree on the absolute form of the same
	// configured path. An empty path defers to the server's pinned path;
	// non-path fields (machine name, filters) still apply.
	duckCfg.Path = ""
	onProgress, finish := daemonPushProgress(
		mirrorBackend.DisplayName(), func(p storage.MirrorPushProgress) {
			fmt.Printf(
				"\rPushing... %d/%d sessions, %d messages\x1b[K",
				p.SessionsDone, p.SessionsTotal, p.MessagesDone,
			)
		},
	)
	defer finish()
	return postDaemonPush[storage.MirrorPushResult](
		ctx, b.tr, b.appCfg.AuthToken, mirrorPushOperation,
		apiclient.DaemonPushRequest{
			Full:            cfg.Full,
			Projects:        projects,
			ExcludeProjects: excludeProjects,
			Duckdb: &apiclient.ConfigDuckDBConfig{
				Path:            duckCfg.Path,
				URL:             duckCfg.URL,
				Token:           new(duckCfg.Token),
				MachineName:     duckCfg.MachineName,
				AllowInsecure:   duckCfg.AllowInsecure,
				AttachTimeout:   new(int64(duckCfg.AttachTimeout)),
				Projects:        duckCfg.Projects,
				ExcludeProjects: duckCfg.ExcludeProjects,
			},
			Automatic: new(cfg.Automatic),
		},
		onProgress,
	)
}

func (b daemonArchiveWriteBackend) ReplicaPushWatch(
	ctx context.Context,
	backend storage.Replica,
	target storage.ConfiguredReplica,
	cfg ReplicaPushConfig,
	projects []string,
	exclude []string,
	debounce time.Duration,
	interval time.Duration,
) error {
	if interval <= 0 {
		interval = defaultWatchInterval
	}
	if debounce <= 0 {
		debounce = defaultWatchDebounce
	}
	// Daemon-delegated pushes build a fresh pusher per request, so the
	// vector reconcile bit and the last-reconciled generation id live
	// here, in the long-lived watch process, mirroring replicaPusher's
	// local-mode state.
	label := backend.Name() + " watch"
	vectorReconcileNeeded := true
	lastReconciledVectorGeneration := int64(0)
	push := func(
		pctx context.Context, reason pushReason, full bool,
		batch *syncpkg.WatchBatch,
	) error {
		pushCfg := cfg
		pushCfg.Full = full
		pushCfg.WatchBatch = batch
		pushCfg.WatchRecovery = watchRecoveryForBatch(b.appCfg, batch)
		scoped := scopedVectorPush(reason, full, vectorReconcileNeeded)
		pushCfg.ScopeVectorsToChangedSessions = scoped
		pushCfg.LastReconciledVectorGeneration = lastReconciledVectorGeneration
		var res storage.PushResult
		var err error
		if b.watchHooks != nil && b.watchHooks.replicaPush != nil {
			res, err = b.watchHooks.replicaPush(pctx, reason, pushCfg)
		} else {
			writer := archiveWriteBackend(b)
			cleanup := func() {}
			if reason != reasonStartup {
				writer, cleanup, err = resolveArchiveWriteBackend(
					pctx, b.appCfg,
				)
				if err != nil {
					return err
				}
			}
			defer cleanup()
			res, err = writer.ReplicaPush(
				pctx, backend, target, pushCfg, projects, exclude,
			)
		}
		if err != nil {
			vectorReconcileNeeded = true
			return err
		}
		vectorReconcileNeeded, lastReconciledVectorGeneration = nextVectorReconcile(
			vectorReconcileNeeded,
			lastReconciledVectorGeneration, scoped, res,
		)
		return completeReplicaWatchPush(label, backend.DisplayName(), res, reason)
	}
	loop, stopLoop := newArchivePushLoop(
		b.watchHooks, label, debounce, interval,
		func(c context.Context, r pushReason, batch *syncpkg.WatchBatch) error {
			return push(c, r, false, batch)
		},
	)
	defer stopLoop()

	stopWatcher, openDispatch, unwatchedDirs := startArchivePushWatcher(
		b.watchHooks, b.appCfg, nil,
		func(callbackCtx context.Context, batch syncpkg.WatchBatch) error {
			return notifyPushForWatchBatchWithConfig(
				callbackCtx, loop, b.appCfg, batch,
			)
		},
		syncpkg.WatcherOptions{OnCoverageDegraded: loop.NotifyCoverageDegraded},
	)
	defer stopWatcher()
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"pg watch: %d root(s) not watched; relying on the %s floor for coverage",
			len(unwatchedDirs), interval,
		)
	}
	initialErr := push(ctx, reasonStartup, cfg.Full, nil)
	if initialErr != nil {
		log.Printf("pg watch: initial daemon push failed: %v", initialErr)
	}
	completePushWatchStartup(ctx, initialErr, loop, openDispatch)

	loop.Run(ctx)
	return nil
}

type localArchiveWriteBackend struct {
	appCfg        config.Config
	database      *db.DB
	ensurePricing func(context.Context, *db.DB) error
	watchHooks    *archivePushWatchHooks
}

func (b *localArchiveWriteBackend) ensureCurrentPricing(
	ctx context.Context,
) error {
	if b.ensurePricing != nil {
		return b.ensurePricing(ctx, b.database)
	}
	return pricingrefresh.EnsureCurrent(ctx, b.database)
}

func (b *localArchiveWriteBackend) newReplicaPusher(
	backend storage.Replica,
	localSync func(context.Context) error,
	connect func(context.Context) (storage.Pusher, error),
) *replicaPusher {
	return &replicaPusher{
		label:         backend.Name() + " watch",
		displayName:   backend.DisplayName(),
		localSync:     localSync,
		ensurePricing: b.ensureCurrentPricing,
		connect:       connect,
		// True until the startup push completes a clean
		// generation-wide vector reconciliation.
		vectorReconcileNeeded: true,
	}
}

func (b *localArchiveWriteBackend) ReplicaPush(
	ctx context.Context,
	backend storage.Replica,
	target storage.ConfiguredReplica,
	cfg ReplicaPushConfig,
	projects []string,
	excludeProjects []string,
) (storage.PushResult, error) {
	display := backend.DisplayName()
	didResync, err := runLocalSyncAuthoritative(
		ctx, b.appCfg, b.database, cfg.Full,
	)
	if err != nil {
		return storage.PushResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return storage.PushResult{}, err
	}
	if err := b.ensureCurrentPricing(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return storage.PushResult{}, ctxErr
		}
		log.Printf("warning: pricing refresh failed: %v", err)
	}
	if err := ctx.Err(); err != nil {
		return storage.PushResult{}, err
	}
	forceFull := cfg.Full || didResync

	fmt.Printf("Connecting to %s...\n", display)
	connectStart := time.Now()
	applyClassifierConfig(b.appCfg)
	vectorSource := replicaVectorPushSource(b.appCfg, target, cfg)
	defer closeVectorPushSource(vectorSource)
	ps, err := backend.NewPusher(
		ctx, target.Target, b.database,
		replicaPusherOptions(target, projects, excludeProjects, vectorSource),
	)
	if err != nil {
		return storage.PushResult{}, err
	}
	defer ps.Close()
	fmt.Printf(
		"Connected to %s in %s\n", display,
		time.Since(connectStart).Round(time.Millisecond),
	)

	fmt.Printf("Preparing %s schema...\n", display)
	schemaStart := time.Now()
	if err := ps.EnsureSchema(ctx); err != nil {
		return storage.PushResult{}, fmt.Errorf("schema: %w", err)
	}
	fmt.Printf(
		"%s schema ready in %s\n", display,
		time.Since(schemaStart).Round(time.Millisecond),
	)
	fmt.Printf("Starting %s push...\n", display)
	result, err := ps.PushWithOptions(ctx, storage.PushOptions{
		Full: forceFull,
		ScopeVectorsToChangedSessions: cfg.
			ScopeVectorsToChangedSessions,
		LastReconciledVectorGeneration: cfg.
			LastReconciledVectorGeneration,
	}, newReplicaPushProgressPrinter())
	fmt.Print("\r\033[K")
	if err != nil {
		return storage.PushResult{}, err
	}
	return result, nil
}

func (b *localArchiveWriteBackend) DuckDBPush(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
) (storage.MirrorPushResult, error) {
	return b.duckDBPush(ctx, duckCfg, cfg, projects, excludeProjects)
}

func (b *localArchiveWriteBackend) duckDBPush(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
) (storage.MirrorPushResult, error) {
	if err := mirrorBackend.ValidatePushTarget(duckCfg); err != nil {
		return storage.MirrorPushResult{}, err
	}
	didResync, err := runLocalSyncAuthoritative(
		ctx, b.appCfg, b.database, cfg.Full,
	)
	if err != nil {
		return storage.MirrorPushResult{}, err
	}
	forceFull := cfg.Full || didResync

	fmt.Println("Starting DuckDB push...")
	return b.duckDBMirrorPush(
		ctx, duckCfg, cfg, projects, excludeProjects, forceFull,
	)
}

func (b *localArchiveWriteBackend) duckDBMirrorPush(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
	forceFull bool,
) (storage.MirrorPushResult, error) {
	if err := mirrorBackend.ValidatePushTarget(duckCfg); err != nil {
		return storage.MirrorPushResult{}, err
	}
	opts := storage.MirrorPushOptions{
		Projects:        projects,
		ExcludeProjects: excludeProjects,
		Automatic:       cfg.Automatic,
	}
	result, err := mirrorBackend.Push(
		ctx, duckCfg, b.database, opts, forceFull,
		func(p storage.MirrorPushProgress) {
			fmt.Printf(
				"\rPushing... %d/%d sessions, %d messages\x1b[K",
				p.SessionsDone, p.SessionsTotal, p.MessagesDone,
			)
		},
	)
	fmt.Print("\r\033[K")
	if err != nil {
		return storage.MirrorPushResult{}, err
	}
	return result, nil
}

func (b *localArchiveWriteBackend) newDuckDBPusher(
	engine *syncpkg.Engine,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects, exclude []string,
) *duckDBPusher {
	pushCfg := cfg
	pushCfg.Automatic = true
	return &duckDBPusher{
		localSync: func(c context.Context) error {
			stats := engine.SyncAll(c, nil)
			if err := c.Err(); err != nil {
				return err
			}
			if !stats.AuthoritativeDiscoveryComplete() {
				return errors.New("local sync discovery incomplete")
			}
			if !stats.ProcessingComplete() {
				return errors.New("local sync processing incomplete")
			}
			engine.FlushSignals()
			return nil
		},
		ensurePricing: b.ensureCurrentPricing,
		mirrorPush: func(c context.Context, forceFull bool) (
			storage.MirrorPushResult, error,
		) {
			return b.duckDBMirrorPush(
				c, duckCfg, pushCfg, projects, exclude, forceFull,
			)
		},
	}
}

func (b *localArchiveWriteBackend) DuckDBPushWatch(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	exclude []string,
	debounce time.Duration,
	interval time.Duration,
) error {
	if interval <= 0 {
		interval = defaultWatchInterval
	}
	if debounce <= 0 {
		debounce = defaultWatchDebounce
	}
	for _, def := range parser.Registry {
		if !b.appCfg.IsUserConfigured(def.Type) {
			continue
		}
		warnMissingDirs(b.appCfg.ResolveDirs(def.Type), string(def.Type))
	}
	cleanResyncTemp(b.appCfg.DBPath)

	engine := syncpkg.NewEngine(ctx, b.database, syncpkg.EngineConfig{
		AgentDirs:               b.appCfg.AgentDirs,
		SourceMachines:          b.appCfg.SourceMachines,
		ProviderMetadata:        b.appCfg.ProviderMetadata,
		DisabledAgents:          b.appCfg.DisabledAgents,
		IncludeCwdPrefixes:      b.appCfg.SyncIncludeCwdPrefixes,
		ScanProtectedPaths:      b.appCfg.ScanProtectedPaths,
		Machine:                 b.appCfg.InstallationID,
		BlockedResultCategories: b.appCfg.ResultContentBlockedCategories,
		ArchiveContent:          b.appCfg.ArchiveContent,
	})
	defer engine.Close()

	var pusher *duckDBPusher
	if b.watchHooks != nil && b.watchHooks.newDuckDBPusher != nil {
		pusher = b.watchHooks.newDuckDBPusher(engine)
	} else {
		pusher = b.newDuckDBPusher(engine, duckCfg, cfg, projects, exclude)
	}
	pusher.scopedSync = func(
		c context.Context,
		batch syncpkg.WatchBatch,
		recovery *syncpkg.WatchRecoveryScope,
		work func() error,
	) error {
		_, err := engine.SyncWatchBatchThenRun(c, batch, recovery, work)
		return err
	}

	loop, stopLoop := newArchivePushLoop(
		b.watchHooks,
		"duckdb watch", debounce, interval,
		func(c context.Context, r pushReason, batch *syncpkg.WatchBatch) error {
			return pusher.pushBatch(
				c, r, false, batch, watchRecoveryForBatch(b.appCfg, batch),
			)
		},
	)
	defer stopLoop()

	poller := newArchivePushUnwatchedPoller(ctx, b.watchHooks, engine)
	defer poller.Stop()

	stopWatcher, openDispatch, unwatchedDirs := startArchivePushWatcher(
		b.watchHooks, b.appCfg, engine,
		archivePushWatchBatchCallback(b.appCfg, loop),
		archivePushWatchWatcherOptions(loop, poller),
	)
	defer stopWatcher()
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"duckdb watch: %d root(s) not watched; polling every %s",
			len(unwatchedDirs), unwatchedPollInterval,
		)
	}

	startupSync := runPGWatchStartupSync
	if b.watchHooks != nil && b.watchHooks.duckDBStartupSync != nil {
		startupSync = b.watchHooks.duckDBStartupSync
	}
	didResync, startupErr := startupSync(ctx, engine, cfg.Full)
	if startupErr != nil && errors.Is(startupErr, context.Canceled) {
		return nil
	}
	initialErr := startupErr
	if initialErr == nil {
		initialErr = pusher.push(ctx, reasonStartup, didResync)
	}
	if initialErr != nil {
		if errors.Is(initialErr, context.Canceled) && ctx.Err() != nil {
			return nil
		}
		log.Printf("duckdb watch: initial push failed: %v", initialErr)
	}
	completePushWatchStartup(ctx, initialErr, loop, openDispatch)

	loop.Run(ctx)
	return nil
}

func logDuckDBWatchPushResult(res storage.MirrorPushResult, reason pushReason) {
	if res.Diagnostics.Deferred {
		log.Printf(
			"duckdb watch: push deferred: %s (%s)",
			res.Diagnostics.DeferredReason, reason,
		)
		return
	}
	if res.Diagnostics.Cutoff != "" {
		log.Printf(
			"duckdb watch: source %s; wrote sessions %s, messages %d (%s)",
			formatDuckDBPushSource(res.Diagnostics),
			formatDuckDBPushSessionCounts(res.Diagnostics.PushedSessions),
			res.MessagesPushed,
			reason,
		)
	}
	if res.Errors > 0 {
		log.Printf(
			"duckdb watch: pushed %d sessions, %d messages, %d errors (%s)",
			res.SessionsPushed, res.MessagesPushed, res.Errors, reason,
		)
		return
	}
	log.Printf(
		"duckdb watch: pushed %d sessions, %d messages (%s)",
		res.SessionsPushed, res.MessagesPushed, reason,
	)
}

func (b *localArchiveWriteBackend) ReplicaPushWatch(
	ctx context.Context,
	backend storage.Replica,
	target storage.ConfiguredReplica,
	cfg ReplicaPushConfig,
	projects []string,
	exclude []string,
	debounce time.Duration,
	interval time.Duration,
) error {
	if interval <= 0 {
		interval = defaultWatchInterval
	}
	if debounce <= 0 {
		debounce = defaultWatchDebounce
	}
	for _, def := range parser.Registry {
		if !b.appCfg.IsUserConfigured(def.Type) {
			continue
		}
		warnMissingDirs(b.appCfg.ResolveDirs(def.Type), string(def.Type))
	}
	cleanResyncTemp(b.appCfg.DBPath)

	engine := syncpkg.NewEngine(ctx, b.database, syncpkg.EngineConfig{
		AgentDirs:               b.appCfg.AgentDirs,
		SourceMachines:          b.appCfg.SourceMachines,
		ProviderMetadata:        b.appCfg.ProviderMetadata,
		DisabledAgents:          b.appCfg.DisabledAgents,
		IncludeCwdPrefixes:      b.appCfg.SyncIncludeCwdPrefixes,
		ScanProtectedPaths:      b.appCfg.ScanProtectedPaths,
		Machine:                 b.appCfg.InstallationID,
		BlockedResultCategories: b.appCfg.ResultContentBlockedCategories,
		ArchiveContent:          b.appCfg.ArchiveContent,
	})
	defer engine.Close()

	name := backend.Name()
	var pusher *replicaPusher
	if b.watchHooks != nil && b.watchHooks.newReplicaPusher != nil {
		pusher = b.watchHooks.newReplicaPusher(engine)
	} else {
		// One vectors.db adapter for the watch loop's lifetime: connect runs on
		// every reconnect, and a fresh source per reconnect would leak the
		// previous one's memoized read-only handle (the pusher never closes
		// its source). The adapter is designed for reuse — it reopens lazily
		// after transient failures.
		vectorSource := replicaVectorPushSource(b.appCfg, target, cfg)
		defer closeVectorPushSource(vectorSource)
		pusher = b.newReplicaPusher(
			backend,
			func(c context.Context) error {
				stats := engine.SyncAll(c, nil)
				if err := c.Err(); err != nil {
					return err
				}
				if !stats.AuthoritativeDiscoveryComplete() {
					return errors.New("local sync discovery incomplete")
				}
				if !stats.ProcessingComplete() {
					return errors.New("local sync processing incomplete")
				}
				// The push scans SQLite rows right after this returns;
				// flush deferred signal recomputes so pushed sessions
				// carry current signal/secret fields.
				engine.FlushSignals()
				return nil
			},
			func(c context.Context) (storage.Pusher, error) {
				applyClassifierConfig(b.appCfg)
				return backend.NewPusher(
					c, target.Target, b.database,
					replicaPusherOptions(target, projects, exclude, vectorSource),
				)
			},
		)
	}
	pusher.scopedSync = func(
		c context.Context,
		batch syncpkg.WatchBatch,
		recovery *syncpkg.WatchRecoveryScope,
		work func() error,
	) error {
		_, err := engine.SyncWatchBatchThenRun(c, batch, recovery, work)
		return err
	}
	defer pusher.reset()

	fmt.Printf(
		"agentsview %s watch: pushing to %s as %q "+
			"(debounce %s, floor %s)\n",
		name, backend.DisplayName(), target.Target.MachineName, debounce, interval,
	)

	loop, stopLoop := newArchivePushLoop(
		b.watchHooks, name+" watch", debounce, interval,
		func(c context.Context, r pushReason, batch *syncpkg.WatchBatch) error {
			return pusher.pushBatch(
				c, r, false, batch, watchRecoveryForBatch(b.appCfg, batch),
			)
		},
	)
	defer stopLoop()

	poller := newArchivePushUnwatchedPoller(ctx, b.watchHooks, engine)
	defer poller.Stop()

	stopWatcher, openDispatch, unwatchedDirs := startArchivePushWatcher(
		b.watchHooks, b.appCfg, engine,
		archivePushWatchBatchCallback(b.appCfg, loop),
		archivePushWatchWatcherOptions(loop, poller),
	)
	defer stopWatcher()
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"pg watch: %d root(s) not watched; polling every %s",
			len(unwatchedDirs), unwatchedPollInterval,
		)
	}

	startupSync := runPGWatchStartupSync
	if b.watchHooks != nil && b.watchHooks.replicaStartupSync != nil {
		startupSync = b.watchHooks.replicaStartupSync
	}
	didResync, startupErr := startupSync(ctx, engine, cfg.Full)
	if startupErr != nil && errors.Is(startupErr, context.Canceled) {
		return nil
	}
	initialErr := startupErr
	if initialErr == nil {
		initialErr = pusher.push(ctx, reasonStartup, didResync)
	}
	if initialErr != nil {
		if errors.Is(initialErr, context.Canceled) && ctx.Err() != nil {
			return nil
		}
		log.Printf("pg watch: initial push failed: %v", initialErr)
	}
	completePushWatchStartup(ctx, initialErr, loop, openDispatch)

	loop.Run(ctx)
	return nil
}

func runPGWatchStartupSync(
	ctx context.Context,
	engine *syncpkg.Engine,
	full bool,
) (bool, error) {
	didResync := false
	stats, err := engine.SyncThenRun(ctx, full, nil,
		func(forceFull bool) error {
			didResync = forceFull
			return nil
		})
	if err != nil {
		return false, err
	}
	if !stats.AuthoritativeDiscoveryComplete() {
		return didResync, errors.New("startup sync discovery incomplete")
	}
	if !stats.ProcessingComplete() {
		return didResync, errors.New("startup sync processing incomplete")
	}
	return didResync, nil
}

func generatedWatchBatch(batch *syncpkg.WatchBatch) *apiclient.SyncWatchBatch {
	if batch == nil {
		return nil
	}
	out := &apiclient.SyncWatchBatch{Paths: batch.Paths, ReconcileRoots: batch.ReconcileRoots, FullSync: new(batch.FullSync), LostEvents: new(batch.LostEvents)}
	for _, rename := range batch.Renames {
		out.Renames = append(out.Renames, apiclient.SyncWatchRename{Path: rename.Path, Root: new(rename.Root), Agent: new(rename.Agent), ItemType: new(int32(rename.ItemType))})
	}
	return out
}

func generatedWatchRecovery(scope *syncpkg.WatchRecoveryScope) *apiclient.SyncWatchRecoveryScope {
	if scope == nil {
		return nil
	}
	return &apiclient.SyncWatchRecoveryScope{AvailableRoots: scope.AvailableRoots, DeferredRoots: scope.DeferredRoots}
}

// replicaPusherOptions scopes one push session to the target's sync-state
// keys and the effective project filters.
func replicaPusherOptions(
	target storage.ConfiguredReplica,
	projects, excludeProjects []string,
	vectorSource storage.VectorPushSource,
) storage.PusherOptions {
	return storage.PusherOptions{
		Projects:               projects,
		ExcludeProjects:        excludeProjects,
		SyncStateTarget:        target.SyncStateTarget(),
		MigrateLegacySyncState: target.MigrateLegacySyncState(),
		VectorSource:           vectorSource,
	}
}
