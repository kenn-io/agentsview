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
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/archivepush"
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
	startupSync func(
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
	notifyCoverageDegraded func([]string) error, poller unwatchedRootPoller,
) syncpkg.WatcherOptions {
	return syncpkg.WatcherOptions{
		OnCoverageDegraded: func(roots []string) error {
			// Degraded coverage needs both owners: the poller reconciles
			// the affected roots authoritatively (including tombstoning
			// missed deletions) and the notifier gets the refreshed archive
			// pushed.
			scopes := make([]pollingScope, 0, len(roots))
			for _, r := range roots {
				scopes = append(scopes, pollingScope{Root: r})
			}
			if err := poller.AddObligation(pollingObligation{
				Key: "watcher-fallback", Scopes: scopes,
			}); err != nil {
				return err
			}
			return notifyCoverageDegraded(roots)
		},
		OnPollingRequired: func(obligation syncpkg.PollingObligation) error {
			return poller.AddObligation(syncObligationToPoller(obligation))
		},
		OnPollingReleased: poller.RemoveObligation,
	}
}

func completeDuckDBWatchPush(
	res storage.MirrorPushResult, reason pushReason, full bool,
) error {
	logDuckDBWatchPushResult(res, reason)
	if full && res.Diagnostics.Deferred && !res.ArchiveRebuilt {
		// A deferred push wrote nothing, so the requested full push is
		// still owed. After a daemon rebuilt the archive the mirror pushes
		// in full anyway, and asking again would rebuild the archive again.
		return storage.PushNotStartedError{
			Err: fmt.Errorf("full push deferred: %s", res.Diagnostics.DeferredReason),
		}
	}
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
		log.Printf("%s: %d session(s) failed to push; will retry", label, res.Errors)
		return fmt.Errorf("%d session(s) failed to push", res.Errors)
	}
	return nil
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

var errNoWritableDaemon = errors.New("no writable agentsview daemon is running; start the agentsview collector or server")

func resolveArchiveWriteBackend(
	ctx context.Context,
	appCfg config.Config,
	intent transportIntent,
) (archiveWriteBackend, func(), error) {
	tr, err := ensureTransportContext(
		ctx, &appCfg, intent, 0,
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
	if appCfg.RequireDaemon {
		if tr.Mode == transportHTTP {
			return nil, nil, fmt.Errorf("daemon at %s is read-only: %w", tr.URL, db.ErrReadOnly)
		}
		return nil, nil, errNoWritableDaemon
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

// requireDaemonSourceSync refuses a daemon-backed watch before it watches
// receiving-host source roots. Archive-only mode is permanent, so retrying
// the daemon's rejection could never succeed.
func requireDaemonSourceSync(ctx context.Context, cfg config.Config) error {
	archiveOnly, err := db.ArchiveOnlyAt(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("reading archive mode: %w", err)
	}
	if archiveOnly {
		return db.ErrArchiveOnly
	}
	return nil
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
// progress event silences it for good so heartbeat lines never interleave
// with the in-place progress line. A push that waits for another push prints
// one line saying so and keeps the heartbeat. The report that the archive
// was rebuilt also keeps it, since schema migration may follow without
// progress events. A push that waits for a local sync prints a line each time
// the daemon reports the wait. The returned finish func stops the heartbeat
// (if no event ever arrived) and clears the in-place line.
func daemonPushProgress[P any](
	label string, phase func(P) string, render func(P),
) (onProgress func(P), finish func()) {
	stop := startDaemonPushHeartbeat(label)
	var once, waitOnce stdsync.Once
	stopHeartbeat := func() { once.Do(stop) }
	onProgress = func(p P) {
		switch phase(p) {
		case storage.PushPhaseWaiting:
			waitOnce.Do(func() {
				fmt.Println("Waiting for another push on the daemon to finish...")
			})
			return
		case storage.PushPhaseRebuilt:
			return
		case storage.PushPhaseWaitingForSync:
			fmt.Print("\r\033[K")
			fmt.Println("Waiting for a local sync on the daemon to finish...")
			return
		}
		stopHeartbeat()
		render(p)
	}
	return onProgress, func() {
		stopHeartbeat()
		fmt.Print("\r\033[K")
	}
}

// pushProgressPhase and mirrorPushProgressPhase read a progress report's
// phase for daemonPushProgress.
func pushProgressPhase(p storage.PushProgress) string { return p.Phase }

func mirrorPushProgressPhase(p storage.MirrorPushProgress) string { return p.Phase }

func (b daemonArchiveWriteBackend) ReplicaPush(
	ctx context.Context,
	backend storage.Replica,
	target storage.ConfiguredReplica,
	cfg ReplicaPushConfig,
	projects []string,
	excludeProjects []string,
) (storage.PushResult, error) {
	if cfg.Embed {
		return storage.PushResult{}, errEmbedNeedsLocalArchive
	}
	operation, err := replicaPushOperation(backend.Name())
	if err != nil {
		return storage.PushResult{}, err
	}
	onProgress, finish := daemonPushProgress(
		backend.DisplayName(), pushProgressPhase, newReplicaPushProgressPrinter(),
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
				PushBatchSize: new(int64(target.Target.PushBatchSize)),
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
	if err := requireDaemonSourceSync(ctx, b.appCfg); err != nil {
		return err
	}
	debounce, interval = watchTimings(debounce, interval)
	pushWriter := b.watchPushWriter()
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
			writer, cleanup, werr := pushWriter(pctx)
			if werr != nil {
				return storage.PushNotStartedError{Err: werr}
			}
			defer cleanup()
			res, err = writer.DuckDBPush(
				pctx, duckCfg, pushCfg, projects, excludeProjects,
			)
		}
		if err != nil {
			return err
		}
		return completeDuckDBWatchPush(res, reason, full)
	}
	return archivePushWatch{
		hooks:    b.watchHooks,
		appCfg:   b.appCfg,
		label:    "duckdb watch",
		debounce: debounce,
		interval: interval,
		full:     cfg.Full,
		push:     push,
	}.run(ctx)
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
		mirrorBackend.DisplayName(), mirrorPushProgressPhase, func(p storage.MirrorPushProgress) {
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
	if cfg.Embed {
		return errEmbedNeedsLocalArchive
	}
	if err := requireDaemonSourceSync(ctx, b.appCfg); err != nil {
		return err
	}
	debounce, interval = watchTimings(debounce, interval)
	pushWriter := b.watchPushWriter()
	label := backend.Name() + " watch"
	vectors := newVectorReconcile()
	push := func(
		pctx context.Context, reason pushReason, full bool,
		batch *syncpkg.WatchBatch,
	) error {
		return vectors.push(
			label, backend.DisplayName(), reason, full,
			func(opts storage.PushOptions) (storage.PushResult, error) {
				pushCfg := cfg
				pushCfg.Full = full
				pushCfg.WatchBatch = batch
				pushCfg.WatchRecovery = watchRecoveryForBatch(b.appCfg, batch)
				pushCfg.ScopeVectorsToChangedSessions = opts.ScopeVectorsToChangedSessions
				pushCfg.LastReconciledVectorGeneration = opts.LastReconciledVectorGeneration
				if b.watchHooks != nil && b.watchHooks.replicaPush != nil {
					return b.watchHooks.replicaPush(pctx, reason, pushCfg)
				}
				writer, cleanup, err := pushWriter(pctx)
				if err != nil {
					return storage.PushResult{}, storage.PushNotStartedError{Err: err}
				}
				defer cleanup()
				return writer.ReplicaPush(
					pctx, backend, target, pushCfg, projects, exclude,
				)
			},
		)
	}
	return archivePushWatch{
		hooks:    b.watchHooks,
		appCfg:   b.appCfg,
		label:    label,
		debounce: debounce,
		interval: interval,
		full:     cfg.Full,
		push:     push,
		wake:     cfg.LifecycleWake,
	}.run(ctx)
}

// watchPushWriter returns the writer source for one daemon watch. The first
// push reuses the connection the watch started with. Every later push,
// including a retry of a failed startup push, resolves the archive owner
// again, because the daemon may have restarted on another address.
func (b daemonArchiveWriteBackend) watchPushWriter() func(
	context.Context,
) (archiveWriteBackend, func(), error) {
	var used atomic.Bool
	return func(ctx context.Context) (archiveWriteBackend, func(), error) {
		if used.CompareAndSwap(false, true) {
			return b, func() {}, nil
		}
		return resolveArchiveWriteBackend(ctx, b.appCfg, transportIntentLongLived)
	}
}

// watchTimings applies the default debounce and floor interval.
func watchTimings(debounce, interval time.Duration) (time.Duration, time.Duration) {
	if debounce <= 0 {
		debounce = defaultWatchDebounce
	}
	if interval <= 0 {
		interval = defaultWatchInterval
	}
	return debounce, interval
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
	connect func(context.Context) (storage.Pusher, error),
) *replicaPusher {
	return &replicaPusher{
		label:       backend.Name() + " watch",
		displayName: backend.DisplayName(),
		session: archivepush.ReplicaSession{
			Connect:       connect,
			EnsurePricing: b.ensureCurrentPricing,
		},
		vectors: newVectorReconcile(),
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
	if err := b.database.RequireSourceSync(ctx); err != nil {
		return storage.PushResult{}, err
	}
	var embedder *replicaEmbedder
	if cfg.Embed {
		var err error
		if embedder, err = newReplicaEmbedder(b.appCfg, backend, target, b.database); err != nil {
			return storage.PushResult{}, err
		}
		embedder.buildOnPrepare = true
		embedder.out = os.Stdout
		defer closeVectorPushSource(embedder)
		// Adopt the published recipe before the local sync, so a missing
		// or ambiguous recipe fails before any session work.
		if err := embedder.resolve(ctx); err != nil {
			return storage.PushResult{}, err
		}
	}
	didResync, err := runLocalSyncAuthoritative(
		ctx, b.appCfg, b.database, cfg.Full,
	)
	if err != nil {
		return storage.PushResult{}, err
	}
	if storage.StoresFriction(backend) {
		backfillFrictionBeforePush(ctx, b.appCfg, b.database)
	}
	if err := ctx.Err(); err != nil {
		return storage.PushResult{}, err
	}

	var vectorSource storage.VectorPushSource
	if embedder != nil {
		vectorSource = embedder
	} else {
		vectorSource = replicaVectorPushSource(b.appCfg, target, cfg)
		defer closeVectorPushSource(vectorSource)
	}
	session := archivepush.ReplicaSession{
		Connect: func(c context.Context) (storage.Pusher, error) {
			applyClassifierConfig(b.appCfg)
			return backend.NewPusher(
				c, target.Target, b.database,
				replicaPusherOptions(target, projects, excludeProjects, vectorSource),
			)
		},
		EnsurePricing: b.ensureCurrentPricing,
		OnStage:       printReplicaPushStage(backend.DisplayName()),
	}
	defer session.Close()
	result, err := session.Push(ctx, storage.PushOptions{
		Full:                           cfg.Full || didResync,
		ScopeVectorsToChangedSessions:  cfg.ScopeVectorsToChangedSessions,
		LastReconciledVectorGeneration: cfg.LastReconciledVectorGeneration,
	}, newReplicaPushProgressPrinter())
	fmt.Print("\r\033[K")
	return result, err
}

// printReplicaPushStage reports each replica push stage on stdout, with the
// time the connect and schema stages took.
func printReplicaPushStage(display string) func(archivepush.Stage) func() {
	return func(stage archivepush.Stage) func() {
		start := time.Now()
		elapsed := func() time.Duration {
			return time.Since(start).Round(time.Millisecond)
		}
		switch stage {
		case archivepush.StageConnect:
			fmt.Printf("Connecting to %s...\n", display)
			return func() {
				fmt.Printf("Connected to %s in %s\n", display, elapsed())
			}
		case archivepush.StageSchema:
			fmt.Printf("Preparing %s schema...\n", display)
			return func() {
				fmt.Printf("%s schema ready in %s\n", display, elapsed())
			}
		case archivepush.StagePush:
			fmt.Printf("Starting %s push...\n", display)
		}
		return nil
	}
}

func (b *localArchiveWriteBackend) DuckDBPush(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
) (storage.MirrorPushResult, error) {
	if err := b.database.RequireSourceSync(ctx); err != nil {
		return storage.MirrorPushResult{}, err
	}
	if err := mirrorBackend.ValidatePushTarget(duckCfg); err != nil {
		return storage.MirrorPushResult{}, err
	}
	didResync, err := runLocalSyncAuthoritative(
		ctx, b.appCfg, b.database, cfg.Full,
	)
	if err != nil {
		return storage.MirrorPushResult{}, err
	}

	fmt.Println("Starting DuckDB push...")
	return b.duckDBMirrorPush(
		ctx, duckCfg, cfg, projects, excludeProjects, cfg.Full || didResync,
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
	result, err := archivepush.MirrorPush{
		Mirror:  mirrorBackend,
		Config:  duckCfg,
		Archive: b.database,
		Options: storage.MirrorPushOptions{
			Projects:        projects,
			ExcludeProjects: excludeProjects,
			Automatic:       cfg.Automatic,
		},
		EnsurePricing: b.ensureCurrentPricing,
	}.Push(ctx, forceFull, func(p storage.MirrorPushProgress) {
		fmt.Printf(
			"\rPushing... %d/%d sessions, %d messages\x1b[K",
			p.SessionsDone, p.SessionsTotal, p.MessagesDone,
		)
	})
	fmt.Print("\r\033[K")
	if err != nil {
		return storage.MirrorPushResult{}, err
	}
	return result, nil
}

func (b *localArchiveWriteBackend) newDuckDBPusher(
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects, exclude []string,
) *duckDBPusher {
	pushCfg := cfg
	pushCfg.Automatic = true
	return &duckDBPusher{
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
	engine, err := b.startWatchEngine(ctx, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer engine.Close()

	var pusher *duckDBPusher
	if b.watchHooks != nil && b.watchHooks.newDuckDBPusher != nil {
		pusher = b.watchHooks.newDuckDBPusher(engine)
	} else {
		pusher = b.newDuckDBPusher(duckCfg, cfg, projects, exclude)
	}

	debounce, interval = watchTimings(debounce, interval)
	return archivePushWatch{
		hooks:    b.watchHooks,
		appCfg:   b.appCfg,
		engine:   engine,
		database: b.database,
		label:    "duckdb watch",
		debounce: debounce,
		interval: interval,
		full:     cfg.Full,
		push:     ignoreWatchBatch(pusher.push),
	}.run(ctx)
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
	debounce, interval = watchTimings(debounce, interval)
	var embedder *replicaEmbedder
	var emitter syncpkg.Emitter
	if cfg.Embed {
		var err error
		if embedder, err = newReplicaEmbedder(b.appCfg, backend, target, b.database); err != nil {
			return err
		}
		embedder.resolveRetry = interval
		emitter = embedder
	}
	engine, err := b.startWatchEngine(ctx, emitter)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer engine.Close()

	name := backend.Name()
	var pusher *replicaPusher
	if b.watchHooks != nil && b.watchHooks.newReplicaPusher != nil {
		pusher = b.watchHooks.newReplicaPusher(engine)
		if embedder != nil {
			defer closeVectorPushSource(embedder)
		}
	} else {
		// One vectors.db adapter for the watch loop's lifetime: connect runs on
		// every reconnect, and a fresh source per reconnect would leak the
		// previous one's memoized read-only handle (the pusher never closes
		// its source). The adapter is designed for reuse — it reopens lazily
		// after transient failures.
		var vectorSource storage.VectorPushSource
		if embedder != nil {
			vectorSource = embedder
		} else {
			vectorSource = replicaVectorPushSource(b.appCfg, target, cfg)
		}
		defer closeVectorPushSource(vectorSource)
		pusher = b.newReplicaPusher(
			backend,
			func(c context.Context) (storage.Pusher, error) {
				applyClassifierConfig(b.appCfg)
				return backend.NewPusher(
					c, target.Target, b.database,
					replicaPusherOptions(target, projects, exclude, vectorSource),
				)
			},
		)
	}
	if storage.StoresFriction(backend) {
		pusher.session.FrictionReview = engine
	}
	pusher.vectors.fullPending = cfg.Full
	defer pusher.session.Close()

	fmt.Printf(
		"agentsview %s watch: pushing to %s as %q "+
			"(debounce %s, floor %s)\n",
		name, backend.DisplayName(), target.Target.MachineName, debounce, interval,
	)

	push := func(c context.Context, r pushReason, full bool) error {
		if embedder != nil && r != reasonShutdown {
			// The embedder's scheduler state belongs to the push loop, which
			// runs every push, so it starts the scheduler there.
			embedder.ensureScheduler(ctx)
		}
		err := pusher.push(c, r, full)
		if err == nil && cfg.OnPushSuccess != nil {
			cfg.OnPushSuccess(c)
		}
		return err
	}
	return archivePushWatch{
		hooks:    b.watchHooks,
		appCfg:   b.appCfg,
		engine:   engine,
		database: b.database,
		label:    name + " watch",
		debounce: debounce,
		interval: interval,
		full:     cfg.Full,
		push:     ignoreWatchBatch(push),
		wake:     cfg.LifecycleWake,
	}.run(ctx)
}

// startWatchEngine prepares a watch that owns the archive and returns the sync
// engine that writes it.
func (b *localArchiveWriteBackend) startWatchEngine(
	ctx context.Context, emitter syncpkg.Emitter,
) (*syncpkg.Engine, error) {
	if err := b.database.RequireSourceSync(ctx); err != nil {
		return nil, err
	}
	for _, def := range parser.Registry {
		if !b.appCfg.IsUserConfigured(def.Type) {
			continue
		}
		warnMissingDirs(b.appCfg.ResolveDirs(def.Type), string(def.Type))
	}
	cleanResyncTemp(b.appCfg.DBPath)
	return syncpkg.NewEngine(ctx, b.database, syncpkg.EngineConfig{
		Emitter:                 emitter,
		AgentDirs:               b.appCfg.AgentDirs,
		SourceMachines:          b.appCfg.SourceMachines,
		ProviderMetadata:        b.appCfg.ProviderMetadata,
		DisabledAgents:          b.appCfg.DisabledAgents,
		IncludeCwdPrefixes:      b.appCfg.SyncIncludeCwdPrefixes,
		ScanProtectedPaths:      b.appCfg.ScanProtectedPaths,
		Machine:                 b.appCfg.InstallationID,
		BlockedResultCategories: b.appCfg.ResultContentBlockedCategories,
		ArchiveContent:          b.appCfg.ArchiveContent,
		FrictionEnabled:         b.appCfg.Friction.Enabled,
	}), nil
}

// ignoreWatchBatch adapts a local owner's pusher to the watch loop. A local
// owner syncs each watcher batch before it queues a push, so the push needs
// only the archive.
func ignoreWatchBatch(
	push func(context.Context, pushReason, bool) error,
) func(context.Context, pushReason, bool, *syncpkg.WatchBatch) error {
	return func(ctx context.Context, reason pushReason, full bool, _ *syncpkg.WatchBatch) error {
		return push(ctx, reason, full)
	}
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
