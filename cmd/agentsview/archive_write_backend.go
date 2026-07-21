package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	stdsync "sync"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/pricingrefresh"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

type archiveWriteBackend interface {
	PGPush(
		ctx context.Context,
		target pgTargetSelection,
		cfg PGPushConfig,
		projects []string,
		excludeProjects []string,
	) (postgres.PushResult, error)
	DuckDBPush(
		ctx context.Context,
		duckCfg config.DuckDBConfig,
		cfg DuckDBPushConfig,
		projects []string,
		excludeProjects []string,
	) (duckdbsync.PushResult, error)
	DuckDBPushWatch(
		ctx context.Context,
		duckCfg config.DuckDBConfig,
		cfg DuckDBPushConfig,
		projects []string,
		excludeProjects []string,
		debounce time.Duration,
		interval time.Duration,
	) error
	PGPushWatch(
		ctx context.Context,
		target pgTargetSelection,
		cfg PGPushConfig,
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
		func(context.Context, pushReason) error,
	) (*pushLoop, func())
	duckDBPush func(
		context.Context, pushReason, bool,
	) (duckdbsync.PushResult, error)
	pgPush func(
		context.Context, pushReason, bool,
	) (postgres.PushResult, error)
	pgStartupSync func(
		context.Context, *syncpkg.Engine, bool,
	) (bool, error)
	newPGPusher        func(*syncpkg.Engine) *pgPusher
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

// newArchivePushUnwatchedPoller builds the pg watch polling owner for
// deferred scopes. The watcher's full recovery and rename promotion defer
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
	push func(context.Context, pushReason) error,
) (*pushLoop, func()) {
	if hooks != nil && hooks.newLoop != nil {
		return hooks.newLoop(label, debounce, interval, push)
	}
	loop, ticker := newPushLoopWithLabel(label, debounce, interval, push)
	return loop, ticker.Stop
}

func completeDuckDBWatchPush(
	res duckdbsync.PushResult, reason pushReason,
) error {
	logDuckDBWatchPushResult(res, reason)
	if res.Errors > 0 {
		return fmt.Errorf("%d session(s) failed to push", res.Errors)
	}
	return nil
}

func completePGWatchPush(res postgres.PushResult, reason pushReason) error {
	logPGWatchPushResult(res, reason)
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
		loop.NotifyDirty()
		return nil
	}
	ack := loop.NotifyDirtyWithAck()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-ack:
		return err
	}
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
	return &localArchiveWriteBackend{
			appCfg:   appCfg,
			database: database,
		}, func() {
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

func (b daemonArchiveWriteBackend) PGPush(
	ctx context.Context,
	target pgTargetSelection,
	cfg PGPushConfig,
	projects []string,
	excludeProjects []string,
) (postgres.PushResult, error) {
	onProgress, finish := daemonPushProgress(
		"PostgreSQL", newPGPushProgressPrinter(),
	)
	defer finish()
	return postDaemonPush[postgres.PushResult](
		ctx, b.tr, b.appCfg.AuthToken, "/api/v1/push/pg",
		daemonPushRequest{
			Full:                   cfg.Full,
			Projects:               projects,
			ExcludeProjects:        excludeProjects,
			PG:                     &target.PG,
			SyncStateTarget:        target.SyncStateTarget,
			MigrateLegacySyncState: target.MigrateLegacySyncState,
			NoVectors:              cfg.NoVectors,
			ScopeVectorsToChangedSessions: cfg.
				ScopeVectorsToChangedSessions,
			LastReconciledVectorGeneration: cfg.
				LastReconciledVectorGeneration,
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
) (duckdbsync.PushResult, error) {
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
	push := func(pctx context.Context, reason pushReason, full bool) error {
		pushCfg := cfg
		pushCfg.Full = full
		// Watch pushes are automatic: a mirror held by a live serve
		// process defers instead of rebuilding the whole archive on
		// every changed batch, and archive-scale diagnostics are
		// skipped. Push ignores the defer behavior when full is set.
		pushCfg.Automatic = true
		var res duckdbsync.PushResult
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
		func(c context.Context, r pushReason) error {
			return push(c, r, false)
		},
	)
	defer stopLoop()

	stopWatcher, openDispatch, unwatchedDirs := startArchivePushWatcher(
		b.watchHooks, b.appCfg, nil,
		func(callbackCtx context.Context, batch syncpkg.WatchBatch) error {
			return notifyPushForWatchBatch(callbackCtx, loop, batch)
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
	initialErr := push(ctx, reasonStartup, cfg.Full)
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
) (duckdbsync.PushResult, error) {
	if err := duckdbsync.ValidatePushTarget(duckCfg); err != nil {
		return duckdbsync.PushResult{}, err
	}
	// Never send a mirror path to the daemon: the daemon pins pushes to its
	// own resolved path and rejects any request naming a different one, and
	// a configured RELATIVE path absolutizes against each process's cwd, so
	// the CLI and daemon can disagree on the absolute form of the same
	// configured path. An empty path defers to the server's pinned path;
	// non-path fields (machine name, filters) still apply.
	duckCfg.Path = ""
	onProgress, finish := daemonPushProgress(
		"DuckDB", func(p duckdbsync.PushProgress) {
			fmt.Printf(
				"\rPushing... %d/%d sessions, %d messages\x1b[K",
				p.SessionsDone, p.SessionsTotal, p.MessagesDone,
			)
		},
	)
	defer finish()
	return postDaemonPush[duckdbsync.PushResult](
		ctx, b.tr, b.appCfg.AuthToken, "/api/v1/push/duckdb",
		daemonPushRequest{
			Full:            cfg.Full,
			Projects:        projects,
			ExcludeProjects: excludeProjects,
			DuckDB:          &duckCfg,
			Automatic:       cfg.Automatic,
		},
		onProgress,
	)
}

func (b daemonArchiveWriteBackend) PGPushWatch(
	ctx context.Context,
	target pgTargetSelection,
	cfg PGPushConfig,
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
	// Daemon-delegated pushes build a fresh postgres.Sync per request,
	// so the vector reconcile bit and the last-reconciled generation id
	// live here, in the long-lived watch process, mirroring pgPusher's
	// local-mode state.
	vectorReconcileNeeded := true
	lastReconciledVectorGeneration := int64(0)
	push := func(pctx context.Context, reason pushReason, full bool) error {
		pushCfg := cfg
		pushCfg.Full = full
		scoped := scopedVectorPush(reason, full, vectorReconcileNeeded)
		pushCfg.ScopeVectorsToChangedSessions = scoped
		pushCfg.LastReconciledVectorGeneration =
			lastReconciledVectorGeneration
		var res postgres.PushResult
		var err error
		if b.watchHooks != nil && b.watchHooks.pgPush != nil {
			res, err = b.watchHooks.pgPush(pctx, reason, full)
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
			res, err = backend.PGPush(
				pctx, target, pushCfg, projects, exclude,
			)
		}
		if err != nil {
			vectorReconcileNeeded = true
			return err
		}
		vectorReconcileNeeded, lastReconciledVectorGeneration =
			nextVectorReconcile(
				vectorReconcileNeeded,
				lastReconciledVectorGeneration, scoped, res,
			)
		return completePGWatchPush(res, reason)
	}
	loop, stopLoop := newArchivePushLoop(
		b.watchHooks, "pg watch", debounce, interval,
		func(c context.Context, r pushReason) error {
			return push(c, r, false)
		},
	)
	defer stopLoop()

	stopWatcher, openDispatch, unwatchedDirs := startArchivePushWatcher(
		b.watchHooks, b.appCfg, nil,
		func(callbackCtx context.Context, batch syncpkg.WatchBatch) error {
			return notifyPushForWatchBatch(callbackCtx, loop, batch)
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
	initialErr := push(ctx, reasonStartup, cfg.Full)
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

func (b *localArchiveWriteBackend) newPGPusher(
	localSync func(context.Context) error,
	connect func() (pgTarget, error),
) *pgPusher {
	return &pgPusher{
		localSync:     localSync,
		ensurePricing: b.ensureCurrentPricing,
		connect:       connect,
		// True until the startup push completes a clean
		// generation-wide vector reconciliation.
		vectorReconcileNeeded: true,
	}
}

func (b *localArchiveWriteBackend) PGPush(
	ctx context.Context,
	target pgTargetSelection,
	cfg PGPushConfig,
	projects []string,
	excludeProjects []string,
) (postgres.PushResult, error) {
	didResync := runLocalSync(ctx, b.appCfg, b.database, cfg.Full)
	if err := ctx.Err(); err != nil {
		return postgres.PushResult{}, err
	}
	if err := b.ensureCurrentPricing(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return postgres.PushResult{}, ctxErr
		}
		log.Printf("warning: pricing refresh failed: %v", err)
	}
	if err := ctx.Err(); err != nil {
		return postgres.PushResult{}, err
	}
	forceFull := cfg.Full || didResync

	fmt.Println("Connecting to PostgreSQL...")
	connectStart := time.Now()
	applyClassifierConfig(b.appCfg)
	vectorSource := pgVectorPushSource(b.appCfg, target, cfg)
	defer closeVectorPushSource(vectorSource)
	ps, err := postgres.New(
		target.PG.URL, target.PG.Schema, b.database,
		target.PG.MachineName, target.PG.AllowInsecure,
		target.syncOptions(projects, excludeProjects, vectorSource),
	)
	if err != nil {
		return postgres.PushResult{}, err
	}
	defer ps.Close()
	fmt.Printf(
		"Connected to PostgreSQL in %s\n",
		time.Since(connectStart).Round(time.Millisecond),
	)

	fmt.Println("Preparing PostgreSQL schema...")
	schemaStart := time.Now()
	if err := ps.EnsureSchema(ctx); err != nil {
		return postgres.PushResult{}, fmt.Errorf("schema: %w", err)
	}
	fmt.Printf(
		"PostgreSQL schema ready in %s\n",
		time.Since(schemaStart).Round(time.Millisecond),
	)
	fmt.Println("Starting PostgreSQL push...")
	result, err := ps.PushWithOptions(ctx, postgres.PushOptions{
		Full: forceFull,
		ScopeVectorsToChangedSessions: cfg.
			ScopeVectorsToChangedSessions,
		LastReconciledVectorGeneration: cfg.
			LastReconciledVectorGeneration,
	}, newPGPushProgressPrinter())
	fmt.Print("\r\033[K")
	if err != nil {
		return postgres.PushResult{}, err
	}
	return result, nil
}

func (b *localArchiveWriteBackend) DuckDBPush(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
) (duckdbsync.PushResult, error) {
	return b.duckDBPush(ctx, duckCfg, cfg, projects, excludeProjects)
}

func (b *localArchiveWriteBackend) duckDBPush(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
) (duckdbsync.PushResult, error) {
	if err := duckdbsync.ValidatePushTarget(duckCfg); err != nil {
		return duckdbsync.PushResult{}, err
	}
	didResync, err := runLocalSyncAuthoritative(
		ctx, b.appCfg, b.database, cfg.Full,
	)
	if err != nil {
		return duckdbsync.PushResult{}, err
	}
	forceFull := cfg.Full || didResync

	fmt.Println("Starting DuckDB push...")
	opts := duckdbsync.SyncOptions{
		Projects:        projects,
		ExcludeProjects: excludeProjects,
		Automatic:       cfg.Automatic,
	}
	result, err := duckdbsync.Push(
		ctx, duckCfg.Path, b.database, duckCfg.MachineName, opts, forceFull,
		func(p duckdbsync.PushProgress) {
			fmt.Printf(
				"\rPushing... %d/%d sessions, %d messages\x1b[K",
				p.SessionsDone, p.SessionsTotal, p.MessagesDone,
			)
		},
	)
	fmt.Print("\r\033[K")
	if err != nil {
		return duckdbsync.PushResult{}, err
	}
	return result, nil
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
	push := func(pctx context.Context, reason pushReason, full bool) error {
		pushCfg := cfg
		pushCfg.Full = full
		// Watch pushes are automatic: a mirror held by a live serve
		// process defers instead of rebuilding the whole archive on
		// every changed batch, and archive-scale diagnostics are
		// skipped. Push ignores the defer behavior when full is set.
		pushCfg.Automatic = true
		var res duckdbsync.PushResult
		var err error
		if b.watchHooks != nil && b.watchHooks.duckDBPush != nil {
			res, err = b.watchHooks.duckDBPush(pctx, reason, full)
		} else {
			res, err = b.DuckDBPush(
				pctx, duckCfg, pushCfg, projects, exclude,
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
		func(c context.Context, r pushReason) error {
			return push(c, r, false)
		},
	)
	defer stopLoop()

	stopWatcher, openDispatch, unwatchedDirs := startArchivePushWatcher(
		b.watchHooks, b.appCfg, nil,
		func(callbackCtx context.Context, batch syncpkg.WatchBatch) error {
			return notifyPushForWatchBatch(callbackCtx, loop, batch)
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
	initialErr := push(ctx, reasonStartup, cfg.Full)
	if initialErr != nil {
		log.Printf("duckdb watch: initial push failed: %v", initialErr)
	}
	completePushWatchStartup(ctx, initialErr, loop, openDispatch)

	loop.Run(ctx)
	return nil
}

func logDuckDBWatchPushResult(res duckdbsync.PushResult, reason pushReason) {
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

func (b *localArchiveWriteBackend) PGPushWatch(
	ctx context.Context,
	target pgTargetSelection,
	cfg PGPushConfig,
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

	engine := syncpkg.NewEngine(b.database, syncpkg.EngineConfig{
		AgentDirs:               b.appCfg.AgentDirs,
		IncludeCwdPrefixes:      b.appCfg.SyncIncludeCwdPrefixes,
		Machine:                 b.appCfg.LocalMachineName,
		BlockedResultCategories: b.appCfg.ResultContentBlockedCategories,
	})
	defer engine.Close()

	var pusher *pgPusher
	if b.watchHooks != nil && b.watchHooks.newPGPusher != nil {
		pusher = b.watchHooks.newPGPusher(engine)
	} else {
		// One vectors.db adapter for the watch loop's lifetime: connect runs on
		// every reconnect, and a fresh source per reconnect would leak the
		// previous one's memoized read-only handle (postgres.Sync never closes
		// its source). The adapter is designed for reuse — it reopens lazily
		// after transient failures.
		vectorSource := pgVectorPushSource(b.appCfg, target, cfg)
		defer closeVectorPushSource(vectorSource)
		pusher = b.newPGPusher(
			func(c context.Context) error {
				stats := engine.SyncAll(c, nil)
				if err := c.Err(); err != nil {
					return err
				}
				if !stats.AuthoritativeDiscoveryComplete() {
					return errors.New("local sync discovery incomplete")
				}
				// The push scans SQLite rows right after this returns;
				// flush deferred signal recomputes so pushed sessions
				// carry current signal/secret fields.
				engine.FlushSignals()
				return nil
			},
			func() (pgTarget, error) {
				applyClassifierConfig(b.appCfg)
				s, cErr := postgres.New(
					target.PG.URL, target.PG.Schema, b.database,
					target.PG.MachineName, target.PG.AllowInsecure,
					target.syncOptions(projects, exclude, vectorSource),
				)
				if cErr != nil {
					return nil, cErr
				}
				return s, nil
			},
		)
	}
	defer pusher.reset()

	fmt.Printf(
		"agentsview pg watch: pushing to PostgreSQL as %q "+
			"(debounce %s, floor %s)\n",
		target.PG.MachineName, debounce, interval,
	)

	loop, stopLoop := newArchivePushLoop(
		b.watchHooks, "pg watch", debounce, interval,
		func(c context.Context, r pushReason) error {
			return pusher.push(c, r, false)
		},
	)
	defer stopLoop()

	poller := newArchivePushUnwatchedPoller(ctx, b.watchHooks, engine)
	defer poller.Stop()

	stopWatcher, openDispatch, unwatchedDirs := startArchivePushWatcher(
		b.watchHooks, b.appCfg, engine,
		func(callbackCtx context.Context, batch syncpkg.WatchBatch) error {
			scope := func() watchRecoveryScope {
				return probeWatchRecoveryScope(b.appCfg)
			}
			if err := syncWatchBatch(callbackCtx, engine, batch, scope); err != nil {
				return err
			}
			return notifyPushForWatchBatch(callbackCtx, loop, batch)
		},
		syncpkg.WatcherOptions{
			OnCoverageDegraded: func(roots []string) error {
				// Degraded coverage needs both owners: the poller reconciles
				// the affected roots authoritatively (including tombstoning
				// missed deletions) and the loop re-pushes the refreshed
				// archive on its floor.
				if err := poller.AddObligation(pollingObligation{
					Key: "watcher-fallback", Roots: roots,
				}); err != nil {
					return err
				}
				return loop.NotifyCoverageDegraded(roots)
			},
			OnPollingRequired: func(obligation syncpkg.PollingObligation) error {
				return poller.AddObligation(pollingObligation{
					Key:   obligation.Key,
					Roots: obligation.Roots,
					Probe: obligation.Probe,
				})
			},
			OnPollingReleased: poller.RemoveObligation,
		},
	)
	defer stopWatcher()
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"pg watch: %d root(s) not watched; polling every %s",
			len(unwatchedDirs), unwatchedPollInterval,
		)
	}

	startupSync := runPGWatchStartupSync
	if b.watchHooks != nil && b.watchHooks.pgStartupSync != nil {
		startupSync = b.watchHooks.pgStartupSync
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
	return didResync, nil
}
