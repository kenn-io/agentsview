package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/signals"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/telemetry"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = ""
)

const (
	periodicSyncInterval           = 15 * time.Minute
	telemetryPingInterval          = 24 * time.Hour
	unwatchedPollInterval          = 2 * time.Minute
	watcherBatchDelay              = 500 * time.Millisecond
	watcherSyncMinInterval         = 5 * time.Second
	deferredStartupSyncGracePeriod = 30 * time.Second
	recursiveWatchBudget           = 8192
)

const (
	// archiveAuditInterval is the daily cadence for the full-archive audit: a
	// rare safety net for silent watcher event loss now that the unscoped
	// 15-minute reconcile is gone.
	archiveAuditInterval = 24 * time.Hour
	// archiveAuditRetryInitial is the first retry delay after a failed audit. It
	// doubles on each subsequent failure and is capped at archiveAuditInterval.
	archiveAuditRetryInitial = time.Hour
)

func main() {
	// Turn on the agentsview-test-fixture deny-list before any scan
	// runs. The secrets package keeps the filter off by default so unit
	// tests in this repo (which use the same random-looking fixtures
	// production scans would suppress) can assert positive rule paths;
	// the binary always wants the filter on.
	secrets.EnableFixtureDeny()

	if err := executeCLI(); err != nil {
		code := exitCodeFromError(err)
		if !isSilentExitError(err) {
			fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		}
		os.Exit(code)
	}
}

// warnMissingDirs prints a warning to stderr for each
// configured directory that does not exist or is
// inaccessible.
func warnMissingDirs(dirs []string, label string) {
	for _, d := range dirs {
		if strings.HasPrefix(d, "s3://") {
			continue // remote source has no local path
		}
		_, err := os.Stat(d)
		if err == nil {
			continue
		}
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr,
				"warning: %s directory not found: %s\n",
				label, d,
			)
		} else {
			fmt.Fprintf(os.Stderr,
				"warning: %s directory inaccessible: %v\n",
				label, err,
			)
		}
	}
}

type serveOptions struct {
	ReplaceDaemon   bool
	NoSyncExplicit  bool
	SkipInitialSync bool
	Pprof           bool
}

// serveMemoryLimitBytes is the default Go soft memory limit for the
// long-running serve daemon. Full parses of multi-megabyte transcripts
// otherwise balloon the heap to several hundred megabytes; the pages
// are freed afterwards but macOS keeps them in the process RSS
// indefinitely, so the daemon's reported memory ratchets to the
// largest transient since startup. A soft limit makes the GC hold the
// heap near the cap during those bursts instead. CGO memory (SQLite)
// is outside the limit, so total RSS settles somewhat above it.
const serveMemoryLimitBytes = 256 << 20

// applyServeMemoryLimit installs the default soft memory limit unless
// the operator already set one via GOMEMLIMIT. Only the serve daemon
// is capped: one-shot CLI commands exit immediately, and the resync
// worker child returns all memory to the OS when it exits, so a cap
// would only slow its full-archive rebuild down.
func applyServeMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	debug.SetMemoryLimit(serveMemoryLimitBytes)
}

func runServe(cfg config.Config, opts serveOptions) {
	start := time.Now()
	setupLogFile(cfg.DataDir)
	applyServeMemoryLimit()

	if err := validateServeConfig(cfg); err != nil {
		fatal("invalid serve config: %v", err)
	}

	// Remote sync archive endpoints always require bearer auth, even when
	// general API auth is disabled. Ensure a token exists before publishing
	// startup state so daemon probes and remote collectors share one token.
	if err := ensureServeAuthToken(&cfg); err != nil {
		log.Fatalf("Failed to generate auth token: %v", err)
	}
	if cfg.RequireAuth {
		// Startup output may be captured by service managers or log files,
		// so never write the bearer token itself.
		if cfg.AuthToken != "" && !runningAsBackgroundChild() {
			fmt.Println("Auth enabled. Token is configured.")
		}
	}

	cont, releaseForegroundServeLaunch, err := prepareForegroundServeDaemon(
		&cfg,
		serveReplacementOptions{
			Replace:        opts.ReplaceDaemon,
			NoSyncExplicit: opts.NoSyncExplicit,
		},
	)
	if err != nil {
		fatal("%v", err)
	}
	if !cont {
		return
	}
	defer releaseForegroundServeLaunch()

	// Acquire the daemon start lock immediately after config setup,
	// before opening the DB, so token-use never sees a window
	// with no lock and no runtime record during startup.
	MarkDaemonStarting(cfg.DataDir)
	defer UnmarkDaemonStarting(cfg.DataDir)
	startupProgress := newStartupStateWriter(cfg.DataDir, time.Now)

	// The signal context is created before the startup worker so SIGTERM can
	// interrupt the worker pass. The server is fully drained by
	// waitForServerRuntime before defers unwind, so registering stop here
	// (rather than after the DB defer) does not affect shutdown ordering.
	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	// Run the archive-scale startup sync in a short-lived worker process before
	// taking the write lock, so its allocation high-water returns to the OS
	// instead of pinning the daemon. The daemon then opens the DB, starts the
	// watcher, and closes the worker-to-watcher event gap below. The worker
	// acquires the write lock the daemon has not taken yet; on any failure the
	// daemon falls back to its in-process initial sync.
	var workerStartupResult workerResult
	workerSyncDone := false
	if !opts.SkipInitialSync && !cfg.NoSync && !testing.Testing() {
		startupProgress.SetPhase("initial sync")
		result, syncErr := runStartupSyncViaWorker(ctx, cfg, startupProgress)
		workerStartupResult, workerSyncDone = startupWorkerOutcome(result, syncErr)
		switch {
		case syncErr == nil:
		case errors.Is(syncErr, errWorkerSpawn):
			log.Printf(
				"startup sync worker spawn failed: %v "+
					"(falling back to in-process initial sync)", syncErr,
			)
		default:
			// The worker ran but reported a non-authoritative terminal result.
			// Do NOT re-run the archive-scale sync in process: carry the
			// aborted result forward so the bounded gap reconciliation and
			// RecordStartupReconciled still open watcher dispatch with the
			// incomplete stats.
			log.Printf(
				"ERROR: startup sync worker ran but did not complete: %v "+
					"(surfacing incomplete pass; not re-syncing in process)",
				syncErr,
			)
		}
	}

	startupProgress.SetPhase("opening database")
	database, writeLock := mustOpenWriteDB(context.Background(), cfg)
	runtimeRecordDataDir := ""
	defer func() {
		closeWriteDB(database, writeLock)
		if runtimeRecordDataDir != "" {
			RemoveDaemonRuntime(runtimeRecordDataDir)
		}
	}()

	if n := len(db.UserAutomationPrefixes()); n > 0 {
		log.Printf("loaded %d user automation prefix(es) from config", n)
	}

	for _, def := range parser.Registry {
		if !cfg.IsUserConfigured(def.Type) {
			continue
		}
		warnMissingDirs(
			cfg.ResolveDirs(def.Type),
			string(def.Type),
		)
	}

	// Remove stale temp DB from a prior crashed resync.
	cleanResyncTemp(cfg.DBPath)

	idleTracker := newDaemonIdleTracker(cfg, stop)

	telemetryReporter := telemetry.NewReporterOrDisabled(telemetry.Options{
		DataDir: cfg.DataDir,
		Version: version,
		Commit:  commit,
	})
	defer func() {
		if err := telemetryReporter.Close(); err != nil {
			log.Printf("close telemetry: %v", err)
		}
	}()

	broadcaster := server.NewBroadcaster(cfg.EventsCoalesceInterval)

	vectorServe, err := setupVectorServing(ctx, cfg, database)
	if err != nil {
		fatal("setting up vector index: %v", err)
	}
	if vectorServe.Close != nil {
		defer func() {
			if cerr := vectorServe.Close(); cerr != nil {
				log.Printf("close vectors.db: %v", cerr)
			}
		}()
	}

	var emitter sync.Emitter = broadcaster
	if vectorServe.Scheduler != nil {
		emitter = teeEmitter{
			primary:      broadcaster,
			scheduler:    vectorServe.Scheduler,
			runAfterSync: cfg.Vector.Embed.RunAfterSyncEnabled(),
		}
	}

	extractSched, err := setupRecallExtraction(cfg, database, idleTracker)
	if err != nil {
		fatal("setting up recall extraction: %v", err)
	}
	if extractSched != nil {
		emitter = extractTeeEmitter{primary: emitter, scheduler: extractSched}
	}

	var engine *sync.Engine
	var stopWatcher func()
	var openWatcherDispatch func()
	var queueWatchRetry func(sync.WatchBatch)
	var unwatchedPoller *sharedUnwatchedPollCoordinator
	var completeWorkerStartup func()
	if !cfg.NoSync {
		var onStartupReconciled func(sync.SyncStats, error)
		engine = sync.NewEngine(database, sync.EngineConfig{
			AgentDirs:               cfg.AgentDirs,
			IncludeCwdPrefixes:      cfg.SyncIncludeCwdPrefixes,
			Machine:                 cfg.LocalMachineName,
			BlockedResultCategories: cfg.ResultContentBlockedCategories,
			Emitter:                 emitter,
			DeferStartupMaintenance: deferStartupMaintenance(
				opts.SkipInitialSync, workerSyncDone,
			),
			OnStartupReconciled: func(stats sync.SyncStats, err error) {
				onStartupReconciled(stats, err)
			},
		})
		defer engine.Close()
		unwatchedPoller = newUnwatchedPollCoordinator(ctx, engine, idleTracker)
		defer unwatchedPoller.Stop()
		stopWatcher, openWatcherDispatch, _, queueWatchRetry = startFileWatcher(
			cfg, engine, func(_ context.Context, batch sync.WatchBatch) error {
				done, ok := idleTracker.BeginWork()
				if !ok {
					return context.Canceled
				}
				defer done()
				// The serve ctx reaches watcher-driven syncs so SIGTERM can
				// interrupt database reconciliation before Stop waits for it.
				return syncWatchBatch(ctx, engine, batch, func() watchRecoveryScope {
					return probeWatchRecoveryScope(cfg)
				})
			},
			sync.WatcherOptions{
				OnCoverageDegraded: func(roots []string) error {
					return unwatchedPoller.AddObligation(pollingObligation{
						Key: "watcher-fallback", Roots: roots,
					})
				},
				OnPollingRequired: func(obligation sync.PollingObligation) error {
					return unwatchedPoller.AddObligation(pollingObligation{
						Key:   obligation.Key,
						Roots: obligation.Roots,
						Probe: obligation.Probe,
					})
				},
				OnPollingReleased: unwatchedPoller.RemoveObligation,
			},
		)
		defer stopWatcher()
		onStartupReconciled = newStartupReconciliationHandler(
			ctx,
			database.CheckpointWALTruncateWithRetry,
			openWatcherDispatch,
		)

		if !opts.SkipInitialSync {
			if workerSyncDone {
				// The worker already ran the full startup pass out of process.
				// Close the worker-to-watcher event gap with a bounded streaming
				// reconciliation over the currently available watch roots (warm:
				// the worker persisted skip state into the archive, which the
				// engine loaded at init), then acknowledge startup. Unavailable
				// scopes are deferred to their probes rather than reconciled as
				// empty. Without this the daemon's engine never runs an
				// in-process sync, so OnStartupReconciled would never fire and
				// the watcher would stay in collecting mode forever.
				//
				// The reconciliation runs after the HTTP server is listening
				// and the startup banner is printed: it can take a while on a
				// large archive, and the archive the worker just synced is
				// fully servable. The watcher keeps collecting events and
				// startup maintenance keeps waiting until
				// RecordStartupReconciled fires.
				completeWorkerStartup = func() {
					var gapErr error
					if gapRoots := reconcileRootPaths(cfg); len(gapRoots) > 0 {
						gapErr = engine.ReconcileWatchRoots(ctx, gapRoots, false)
					}
					if gapErr != nil && ctx.Err() == nil {
						// Hand the failed gap reconciliation to the watcher's
						// retry queue before dispatch opens, so the affected
						// roots are re-reconciled with backoff instead of
						// staying undiscovered until another event, manual
						// sync, or the daily audit.
						queueWatchRetry(gapReconciliationRetryBatch(gapErr))
					}
					engine.RecordStartupReconciled(
						statsFromWorkerResult(workerStartupResult), gapErr,
					)
				}
			} else if database.NeedsResync() {
				startupProgress.SetPhase("full resync")
				signalsCovered, _ := runInitialResync(ctx, engine, startupProgress)
				if ctx.Err() == nil {
					finishInitialResync(database, signalsCovered)
				}
			} else {
				startupProgress.SetPhase("initial sync")
				runInitialSync(ctx, engine, startupProgress)
			}
			if ctx.Err() != nil {
				return
			}
		}

		// Backfill runs in the background. On a large DB (e.g.
		// after copying tens of thousands of orphaned sessions
		// during a resync), walking every row to recompute
		// signals would otherwise block the HTTP server from
		// listening for minutes. Startup maintenance waits for
		// a deferred foreground sync and shares its lock with
		// later sync/resync database swaps.
		go idleTracker.Do(func() {
			err := engine.RunStartupMaintenance(ctx, func() error {
				return database.BackfillSignals(
					ctx,
					engine.BackfillSignalComputer(),
				)
			})
			if err != nil && ctx.Err() == nil {
				log.Printf("signals backfill: %v", err)
			}
		})
		validRemotes := true
		if err := cfg.ValidateRemoteHosts(); err != nil {
			log.Printf("warning: remote_hosts config invalid, skipping periodic remote sync: %v", err)
			validRemotes = false
		}
		go startPeriodicSync(
			ctx, cfg, engine, database, writeLock, idleTracker, validRemotes, emitter,
		)
	}

	identityBackfillEngine := engine
	if identityBackfillEngine == nil {
		identityBackfillEngine = sync.NewEngine(database, sync.EngineConfig{
			Machine: cfg.LocalMachineName,
		})
	}
	go idleTracker.Do(func() {
		err := identityBackfillEngine.RunStartupMaintenance(ctx, func() error {
			return identityBackfillEngine.BackfillProjectIdentitySnapshots(ctx)
		})
		if err != nil && ctx.Err() == nil {
			log.Printf("project identity backfill: %v", err)
		}
	})

	// Seed model_pricing so a fresh database (first run, or a
	// resync whose pricing copy failed) is populated before
	// the dashboard starts answering requests. Resyncs also
	// copy pricing across the swap themselves, since this seed
	// only runs once per daemon lifetime. Synchronous fallback
	// upsert so the first usage page load does not observe an
	// empty table; background LiteLLM refresh follows
	// immediately.
	seedPricing(database)

	rtOpts := serveRuntimeOptions{
		Mode:          "serve",
		RequestedPort: cfg.Port,
	}
	preparedCfg, prepErr := prepareServeRuntimeConfig(cfg, rtOpts)
	if prepErr != nil {
		fatal("%v", prepErr)
	}
	cfg = preparedCfg

	srvOpts := []server.Option{
		server.WithVersion(server.VersionInfo{
			Version:   version,
			Commit:    commit,
			BuildDate: buildDate,
		}),
		server.WithDataDir(cfg.DataDir),
		server.WithBaseContext(ctx),
		server.WithBroadcaster(broadcaster),
		server.WithIdleTracker(idleTracker),
		server.WithHTTPRemoteCleanupRegistry(httpRemoteCleanupRegistry),
		server.WithPprof(opts.Pprof),
	}
	srvOpts = append(srvOpts, vectorServe.ServerOpts...)
	if src := newVectorPushSource(cfg); src != nil {
		srvOpts = append(srvOpts, server.WithVectorPushSource(src))
	}
	if extractSched != nil {
		// Trash, restore, and permanent-delete routes change extraction
		// eligibility; the retraction pass must hear about them even when
		// no sync activity follows. Notify never blocks.
		srvOpts = append(srvOpts,
			server.WithSessionMutationNotifier(extractSched.Notify))
	}
	if engine != nil {
		srvOpts = append(srvOpts, server.WithLocalSyncRunner(
			newForegroundSyncRunner(ctx, cfg, engine, database, writeLock),
		))
		srvOpts = append(srvOpts, server.WithLocalResyncRunner(
			newForegroundResyncRunner(ctx, cfg, engine, database),
		))
	}
	srv := server.New(cfg, database, engine, srvOpts...)

	startupProgress.SetPhase("starting HTTP server")
	rt, err := startServerWithOptionalCaddy(ctx, cfg, srv, rtOpts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fatal("%v", err)
	}

	// Server is ready — write the definitive kit runtime record with the
	// final port and release the start lock. If the runtime record
	// write fails, keep the start lock as a fallback "server
	// is active" marker so token-use doesn't start a competing
	// on-demand sync against our live DB.
	if _, sfErr := writeDaemonRuntimeWithAuthAndNoSync(
		rt.Cfg.DataDir, rt.Cfg.Host, rt.Cfg.Port, version, false,
		rt.Cfg.RequireAuth, rt.Cfg.NoSync,
		rt.Caddy.Pid(),
	); sfErr != nil {
		reportRuntimeRecordWrite(
			os.Stdout, sfErr, "keeping start lock as fallback",
			"To fix permissions, run: icacls <dir> /setowner <user>",
		)
	} else {
		runtimeRecordDataDir = rt.Cfg.DataDir
		UnmarkDaemonStarting(rt.Cfg.DataDir)
	}
	releaseForegroundServeLaunch()
	if idleTracker != nil {
		idleTracker.Touch()
		go idleTracker.Run(ctx)
	}
	if engine != nil && opts.SkipInitialSync {
		go func() {
			timer := time.NewTimer(deferredStartupSyncGracePeriod)
			defer timer.Stop()
			ran, fallbackErr := runDeferredStartupSyncFallback(
				ctx, cfg, engine, database, writeLock, idleTracker, timer.C,
			)
			if fallbackErr != nil && ctx.Err() == nil {
				log.Printf("deferred startup sync: %v", fallbackErr)
			} else if ran {
				log.Printf(
					"deferred startup sync completed after no foreground request arrived",
				)
			}
		}()
	}

	if rt.PublicURL == rt.LocalURL {
		fmt.Printf(
			"agentsview %s listening at %s (started in %s)\n",
			version, rt.LocalURL,
			time.Since(start).Round(time.Millisecond),
		)
	} else {
		fmt.Printf(
			"agentsview %s backend at %s, public at %s (started in %s)\n",
			version, rt.LocalURL, rt.PublicURL,
			time.Since(start).Round(time.Millisecond),
		)
	}
	fmt.Printf("Database: %s\n", cfg.DBPath)

	startTelemetryPings(ctx, telemetryReporter)

	if vectorServe.Scheduler != nil {
		go vectorServe.Scheduler.Run(ctx)
		// Registered after the vectors.db Close defer above, so LIFO
		// unwind order runs Stop (which waits for any in-flight
		// TryBuild to return) before vectors.db is closed.
		defer vectorServe.Scheduler.Stop()
	}

	if extractSched != nil {
		go extractSched.Run(ctx)
		// Stop waits for any in-flight extraction pass, so the archive
		// is never closed under one.
		defer extractSched.Stop()
	}

	// Run the deferred worker-startup gap reconciliation now that the URL
	// banner is out and every background service is started. It holds the
	// engine's sync mutex, not this goroutine's defers, so a SIGTERM during
	// a long reconciliation still unwinds through waitForServerRuntime.
	if completeWorkerStartup != nil {
		completeWorkerStartup()
	}

	if err := waitForServerRuntime(ctx, srv, rt); err != nil {
		fatal("%v", err)
	}
}

func runDeferredStartupSyncFallback(
	ctx context.Context,
	cfg config.Config,
	engine *sync.Engine,
	database *db.DB,
	lock *writeOwnerLock,
	idleTracker *server.IdleTracker,
	timeout <-chan time.Time,
) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timeout:
	}

	done, ok := idleTracker.BeginWork()
	if !ok {
		return false, nil
	}
	defer done()

	// A foreground `agentsview sync` may have already driven startup
	// reconciliation through newForegroundSyncRunner; skip the redundant worker
	// pass in that case. This is only the fast path: a foreground worker still
	// in flight records reconciliation before releasing the exclusive lock, and
	// runWorkerSyncPass rechecks the gate while holding that lock, so the timer
	// firing mid-pass cannot launch a second archive-scale worker.
	if engine.StartupReconciled() {
		return false, nil
	}

	// Route the skipped startup sync through the worker so it does not run at
	// archive scale in the daemon. Only a failure in which no worker ran — a
	// spawn or pre-launch handoff failure — (or a test binary) falls back to
	// the in-process path; a worker that ran and reported failure is surfaced
	// without re-running. Both paths acknowledge startup so the watcher leaves
	// collecting mode.
	if !testing.Testing() {
		_, ran, err := runWorkerSyncPass(
			ctx, ctx, cfg, engine, database, lock, true, nil,
		)
		if err == nil || !workerNeverRan(err) {
			return ran, err
		}
		log.Printf(
			"deferred startup sync worker did not run: %v "+
				"(falling back in-process)", err,
		)
	}

	_, ran, err := engine.RunStartupSyncFallback(ctx, nil)
	return ran, err
}

// runStartupSyncViaWorker runs the daemon's startup sync in a short-lived
// worker process, relaying its progress phases into the startup-state writer so
// `serve status` reports live phases, and to the console so a foreground serve
// shows the same "Running initial sync..." progress the in-process path prints.
// It must run before the daemon takes the write lock so the worker can acquire
// it. It returns the worker's terminal result (used to acknowledge startup) and
// any launch/protocol error.
//
// The brief specifies a plain error return; it returns the result too so the
// caller can pass the worker's discovery outcome to RecordStartupReconciled.
func runStartupSyncViaWorker(
	ctx context.Context, cfg config.Config, progress *startupStateWriter,
) (workerResult, error) {
	fmt.Println("Running initial sync...")
	t := time.Now()
	resyncAnnounced := false
	progressShown := false
	onLine := func(l workerLine) {
		if l.Progress == nil {
			return
		}
		p := *l.Progress
		// Resync progress maps onto the "full resync" phase; the plain initial
		// sync keeps the "initial sync" phase set before this call.
		if p.Resync {
			progress.SetPhase("full resync")
			if !resyncAnnounced {
				resyncAnnounced = true
				fmt.Println("Data version changed, running full resync...")
			}
		}
		printSyncProgress(p)
		progressShown = true
		progress.SetDetail(startupProgressDetail(p))
	}
	result, err := launchSyncWorker(ctx, cfg, "startup", onLine)
	if err == nil && result.Stats != nil {
		printSyncSummary(*result.Stats, t)
	} else if progressShown {
		// Terminate the in-place \r progress line so the next console
		// output (fallback messages, the startup banner) starts clean.
		fmt.Println()
	}
	return result, err
}

// startupWorkerOutcome decides how runServe proceeds after the startup worker.
// It discriminates a spawn failure (the worker never ran) from a ran-and-failed
// worker (a valid terminal result reporting incomplete discovery).
//
//   - err == nil: the worker completed; carry its result, done = true.
//   - errors.Is(err, errWorkerSpawn): the worker never ran; done = false so the
//     caller runs the in-process initial sync fallback.
//   - any other err: the worker ran but did not complete; done = true with an
//     aborted result (synthesized when the terminal record is unusable) so the
//     caller surfaces the incomplete pass through gap reconciliation rather than
//     re-running the archive-scale sync in process.
func startupWorkerOutcome(result workerResult, err error) (workerResult, bool) {
	switch {
	case err == nil:
		return result, true
	case errors.Is(err, errWorkerSpawn):
		return workerResult{}, false
	default:
		if result.Status == "" {
			result.Status = "aborted"
		}
		result.DiscoveryComplete = false
		return result, true
	}
}

// deferStartupMaintenance reports whether archive-wide startup backfills must
// wait for RecordStartupReconciled instead of starting immediately. Two paths
// defer them: a daemon launched with the initial sync skipped (a foreground
// client drives startup, released by its sync or the bounded fallback), and
// the worker path, where the gap reconciliation runs only after the HTTP
// server is up — maintenance released at construction would grab the sync
// mutex first and stall that reconciliation, keeping watcher dispatch in
// collecting mode. RecordStartupReconciled releases the gate in both cases.
func deferStartupMaintenance(skipInitialSync, workerSyncDone bool) bool {
	return skipInitialSync || workerSyncDone
}

// statsFromWorkerResult maps a worker terminal result onto SyncStats. The
// worker carries the complete public SyncStats payload so /sync and /resync
// responses keep parity with in-process passes; the summary counters are the
// fallback for a terminal record without one. Either way, Aborted mirrors the
// worker's DiscoveryComplete verdict: providerFailures does not cross the
// worker protocol, and AuthoritativeDiscoveryComplete() must stay accurate on
// the daemon side.
func statsFromWorkerResult(r workerResult) sync.SyncStats {
	if r.Stats != nil {
		stats := *r.Stats
		stats.Aborted = !r.DiscoveryComplete
		return stats
	}
	return sync.SyncStats{
		Synced:  r.Synced,
		Skipped: r.Skipped,
		Failed:  r.Failed,
		Aborted: !r.DiscoveryComplete,
	}
}

// reconcileRootPaths returns the scope for the startup gap reconciliation,
// the archive audit, and the watcher's full recovery: every watch-root path
// whose physical probe is currently present, plus present unwatched polling
// dirs. Scopes gated by an unavailable
// physical path are deferred, mirroring the watcher's polling-obligation
// probes: an unmounted volume or a missing nested session subtree (e.g.
// Gemini's <root>/tmp) streams an empty discovery without error, and an
// authoritative pass over it would tombstone every active session beneath it.
func reconcileRootPaths(cfg config.Config) []string {
	return probeWatchRecoveryScope(cfg).available
}

// watchRecoveryScope is one probed availability snapshot of the configured
// watch scope: the currently available reconciliation paths plus the
// configured dirs whose physical scope is missing and therefore deferred to
// their polling probes.
type watchRecoveryScope struct {
	available []string
	deferred  map[string]struct{}
}

// coversProviderRoot reports whether a configured provider root is currently
// available for authoritative reconciliation: no deferred scope overlaps the
// root's engine-side expansion, and at least one probed available path lies
// at or under the root.
func (s watchRecoveryScope) coversProviderRoot(root string) bool {
	root = filepath.Clean(root)
	if overlapsDeferredScope(root, s.deferred) {
		return false
	}
	for _, path := range s.available {
		if path == root || pathWithinRoot(path, root) {
			return true
		}
	}
	return false
}

// probeWatchRecoveryScope computes the probed reconciliation scope backing
// reconcileRootPaths; see that function for the deferral semantics.
func probeWatchRecoveryScope(cfg config.Config) watchRecoveryScope {
	roots, unwatchedDirs, symlinkGatedDirs := collectWatchRoots(cfg)
	deferred := make(map[string]struct{})
	// A recursive symlink root never joins the watch roots, so its exact
	// availability probe is the symlink target itself: os.Stat follows the
	// link and fails while the target is gone, deferring the configured
	// scope before an overlapping present path could expand into it.
	for symRoot, dirs := range symlinkGatedDirs {
		if _, err := os.Stat(symRoot); err == nil {
			continue
		}
		for _, dir := range dirs {
			deferred[filepath.Clean(dir)] = struct{}{}
		}
	}
	for _, r := range roots {
		if r.exists {
			continue
		}
		for _, dir := range r.pendingPollingDirs {
			deferred[filepath.Clean(dir)] = struct{}{}
		}
	}
	seen := make(map[string]struct{})
	var paths []string
	add := func(path string) {
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	for _, r := range roots {
		if !r.exists {
			continue
		}
		// A present root sharing a configured dir with a missing physical
		// subtree (Gemini's shallow <root> next to a missing <root>/tmp,
		// a provider parent dir over missing session dirs) would expand to
		// that dir's full provider scope; defer it with the subtree.
		if slices.ContainsFunc(r.syncDirs(), func(dir string) bool {
			_, gated := deferred[filepath.Clean(dir)]
			return gated
		}) {
			continue
		}
		if overlapsDeferredScope(r.path, deferred) {
			continue
		}
		add(r.path)
	}
	for _, dir := range unwatchedDirs {
		if overlapsDeferredScope(filepath.Clean(dir), deferred) {
			continue
		}
		if _, err := os.Stat(dir); err == nil {
			add(dir)
		}
	}
	return watchRecoveryScope{available: paths, deferred: deferred}
}

// overlapsDeferredScope reports whether the engine-side expansion of a
// present reconciliation path would pull a deferred configured scope back
// into an authoritative pass. The engine expands a requested root to every
// configured dir that is its ancestor or descendant, and the tombstone pass
// sweeps stored paths by root prefix, so a present path related to a deferred
// dir in either direction (an outer root over an unmounted nested root, or
// Codex's shallow parent probe over a sibling provider's missing dir) would
// read that scope as an empty discovery and tombstone every baselined
// session beneath it. Both sides are normalized to absolute form here because
// the engine expands roots against configured dirs after filepath.Abs
// (cleanRootPath): a scope configured relative and a path configured absolute
// still overlap on the engine side, so they must overlap here too.
func overlapsDeferredScope(path string, deferred map[string]struct{}) bool {
	path = absRootPath(path)
	for dir := range deferred {
		dir = absRootPath(dir)
		if path == dir || pathWithinRoot(path, dir) || pathWithinRoot(dir, path) {
			return true
		}
	}
	return false
}

// absRootPath mirrors the engine's cleanRootPath so daemon-side scope-overlap
// checks compare the same path form the engine's root expansion uses.
func absRootPath(path string) string {
	cleaned := filepath.Clean(path)
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return cleaned
	}
	return abs
}

// pathWithinRoot reports whether the cleaned path lies strictly below the
// cleaned root. A cleaned filesystem root ("/" on Unix, a volume root on
// Windows) already ends in the separator, so the descendant prefix is only
// suffixed when missing.
func pathWithinRoot(path, root string) bool {
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return path != root && strings.HasPrefix(path, prefix)
}

// newForegroundSyncRunner builds the daemon's foreground local-sync runner used
// by the sync HTTP handler. It routes through the worker so a foreground
// `agentsview sync` never runs the archive-scale pass in the daemon. A failure
// in which no worker ran — a spawn or pre-launch handoff failure — (or a test
// binary) falls back to the in-process SyncThenRun; a worker that ran and
// reported failure is surfaced without re-running.
func newForegroundSyncRunner(
	daemonCtx context.Context,
	cfg config.Config, engine *sync.Engine, database *db.DB, lock *writeOwnerLock,
) server.LocalSyncRunner {
	return func(
		ctx context.Context, progress func(sync.Progress),
	) (sync.SyncStats, error) {
		if !testing.Testing() {
			onLine := func(l workerLine) {
				if l.Progress != nil && progress != nil {
					progress(*l.Progress)
				}
			}
			// runWorkerSyncPass records completion with SyncThenRun parity:
			// startup acknowledgement (watcher dispatch, startup maintenance)
			// closes before the exclusive lock is released, and last-sync
			// state plus the "sync" emit fire after it, so /sync/status and
			// SSE subscribers observe the worker-backed pass.
			stats, _, err := runWorkerSyncPass(
				ctx, daemonCtx, cfg, engine, database, lock, false, onLine,
			)
			if err == nil || !workerNeverRan(err) {
				return stats, err
			}
			log.Printf(
				"foreground sync worker did not run: %v "+
					"(falling back in-process)", err,
			)
		}
		return engine.SyncThenRun(
			ctx, false, progress, func(bool) error { return nil },
		)
	}
}

// newForegroundResyncRunner builds the daemon's foreground resync runner used by
// the resync HTTP handler. It builds the replacement archive in a worker process
// behind the write barrier, then swaps it in and resets caches. A spawn failure
// (or a test binary) falls back to the in-process resync; a worker that ran and
// reported failure is surfaced without re-running.
func newForegroundResyncRunner(
	daemonCtx context.Context,
	cfg config.Config, engine *sync.Engine, database *db.DB,
) server.LocalResyncRunner {
	return func(
		ctx context.Context, progress func(sync.Progress),
	) (sync.SyncStats, error) {
		if !testing.Testing() {
			result, err, spawnFailed := runWorkerResyncBuild(
				ctx, daemonCtx, cfg, engine, database, progress,
			)
			if !spawnFailed {
				if result.Status == "aborted" && ctx.Err() == nil {
					// Mirror syncThenRunLocked: a safety-aborted resync still
					// catches up incrementally so safely applicable updates
					// land instead of surfacing the bare abort. The worker
					// "sync" mode refuses stale archives, so this warm,
					// skip-cache-bounded pass runs in process like the
					// pre-worker path it preserves. Only the worker's explicit
					// "aborted" verdict takes this path: operational build
					// failures report "failed" and must surface their error
					// rather than masquerade as a successful incremental sync.
					return syncAllReleasingStartupMaintenance(
						ctx, engine, progress,
					), nil
				}
				return statsFromWorkerResult(result), err
			}
			log.Printf(
				"foreground resync worker spawn failed: %v "+
					"(falling back in-process)", err,
			)
		}
		// SyncThenRun, not ResyncAll: it keeps the abort-to-incremental
		// fallback for a safely aborted in-process resync and releases
		// startup maintenance on success, matching the handler's no-runner
		// arm (runResyncWithFallback) and the sync runner's fallback above.
		return engine.SyncThenRun(
			ctx, true, progress, func(bool) error { return nil },
		)
	}
}

// syncAllReleasingStartupMaintenance runs an incremental pass and, mirroring
// SyncThenRun, releases startup maintenance when the pass was not cancelled.
// SyncAll records startup reconciliation on its own but never releases the
// maintenance gate; without the explicit release, a skip-initial-sync daemon
// whose foreground resync took this arm would keep archive-wide backfills
// gated until shutdown, because the deferred startup fallback observes the
// closed reconciliation gate and returns without releasing. A cancelled pass
// that never reconciled leaves the gate closed so the deferred fallback still
// owns recovery; once startup is reconciled the release is owed regardless of
// ctx — cancellation can land between SyncAll recording reconciliation and
// this check, and the deferred fallback skips on the closed reconciliation
// gate without ever releasing.
func syncAllReleasingStartupMaintenance(
	ctx context.Context, engine *sync.Engine, progress func(sync.Progress),
) sync.SyncStats {
	stats := engine.SyncAll(ctx, progress)
	if ctx.Err() == nil || engine.StartupReconciled() {
		engine.ReleaseStartupMaintenance()
	}
	return stats
}

// runWorkerResyncBuild builds a resync replacement in a worker process behind the
// write barrier, then swaps it in and resets caches. It closes the writer for the
// whole build-and-swap window (readers keep serving, direct writes fail with
// ErrWriterClosed) without releasing the write-owner flock: the worker never
// opens the live archive writable. The worker's terminal result is returned so
// the caller can distinguish an explicit safety abort (Status "aborted") from
// an operational failure. spawnFailed is true only when the worker could not
// be launched, so the caller falls back in process.
func runWorkerResyncBuild(
	ctx context.Context,
	recoveryCtx context.Context,
	cfg config.Config,
	engine *sync.Engine,
	database *db.DB,
	progress func(sync.Progress),
) (workerResult, error, bool) {
	relay := func(l workerLine) {
		if l.Progress != nil && progress != nil {
			progress(*l.Progress)
		}
	}
	var result workerResult
	var launchErr error
	var doneStats sync.SyncStats
	barrierErr := engine.RunExclusive(func() error {
		if cerr := closeWriterForPass(
			recoveryCtx, database, "resync build",
		); cerr != nil {
			return cerr
		}
		result, launchErr = launchSyncWorker(ctx, cfg, "resync-build", relay)
		if launchErr != nil {
			// The worker never swapped; restore the writer the barrier closed.
			// Restoration is mandatory — abandoning it would leave every write
			// endpoint failing until restart.
			if rerr := restoreArchiveAccess(
				recoveryCtx,
				"reopen writer after failed resync build",
				database.ReopenWriter,
			); rerr != nil {
				launchErr = errors.Join(launchErr, rerr)
			}
			return launchErr
		}
		if serr := engine.SwapResyncDatabase(engine.ResyncTempPath()); serr != nil {
			// Swap failures happen at or after CloseConnections closed the
			// reader pool, so when the swap's own recovery did not restore
			// the archive only a full Reopen brings reads back; ReopenWriter
			// alone would leave every read on a closed pool until restart.
			if database.WriterClosed() {
				if rerr := restoreArchiveAccess(
					recoveryCtx,
					"reopen archive after failed resync swap",
					database.Reopen,
				); rerr != nil {
					serr = errors.Join(
						serr, fmt.Errorf("recovery reopen: %w", rerr),
					)
				}
			}
			return fmt.Errorf("swap resync database: %w", serr)
		}
		// The swap's reopen restored the writer and cleared the barrier;
		// re-baseline the caches that referenced the replaced database.
		if cerr := engine.ResetCachesAfterSwap(); cerr != nil {
			return cerr
		}
		// Record the completed resync with ResyncAll parity before the
		// exclusive lock is released: last-sync state feeds /sync/status
		// hydration, and the closed startup gate keeps the deferred startup
		// fallback from launching another archive-scale pass. The emit and
		// startup callback fire after the lock below.
		doneStats = statsFromWorkerResult(result)
		engine.RecordStartupReconciledExclusive(doneStats, nil)
		return nil
	})
	if barrierErr != nil {
		if errors.Is(barrierErr, errWorkerSpawn) {
			return workerResult{}, barrierErr, true
		}
		return result, barrierErr, false
	}
	engine.FinishStartupReconciled(doneStats)
	return result, nil, false
}

func newStartupReconciliationHandler(
	ctx context.Context,
	checkpoint func(context.Context) error,
	openDispatch func(),
) func(sync.SyncStats, error) {
	return func(_ sync.SyncStats, reconciliationErr error) {
		if ctx.Err() != nil {
			return
		}
		if reconciliationErr == nil {
			err := checkpoint(ctx)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				log.Printf("post-sync wal checkpoint: %v", err)
			}
		} else {
			log.Printf(
				"startup sync incomplete; opening watcher dispatch with retry coverage: %v",
				reconciliationErr,
			)
		}
		openDispatch()
	}
}

func ensureServeAuthToken(cfg *config.Config) error {
	if cfg == nil || cfg.AuthToken != "" {
		return nil
	}
	return cfg.EnsureAuthToken()
}

func newDaemonIdleTracker(cfg config.Config, stop context.CancelFunc) *server.IdleTracker {
	if !runningAsBackgroundChild() {
		return nil
	}
	timeout := cfg.DaemonIdleTimeout
	if raw := os.Getenv("AGENTSVIEW_DAEMON_IDLE_TIMEOUT"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			log.Printf(
				"invalid AGENTSVIEW_DAEMON_IDLE_TIMEOUT %q: %v",
				raw, err,
			)
		} else {
			timeout = parsed
		}
	}
	if timeout <= 0 {
		return nil
	}
	return server.NewIdleTracker(timeout, func() {
		log.Printf("idle timeout elapsed; shutting down daemon")
		stop()
	})
}

func startTelemetryPings(ctx context.Context, reporter *telemetry.Reporter) {
	if reporter == nil || !reporter.Enabled() {
		return
	}
	captureTelemetryPing(ctx, reporter)
	go func() {
		ticker := time.NewTicker(telemetryPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				captureTelemetryPing(ctx, reporter)
			}
		}
	}()
}

func captureTelemetryPing(ctx context.Context, reporter *telemetry.Reporter) {
	if err := reporter.CaptureDaemonActive(ctx); err != nil && ctx.Err() == nil {
		log.Printf("capture telemetry event: %v", err)
	}
}

func mustLoadConfig(cmd *cobra.Command) config.Config {
	cfg, err := config.LoadPFlags(cmd.Flags())
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	return cfg
}

// maxLogSize is the threshold at which the debug log file is
// truncated on startup to prevent unbounded growth.
const maxLogSize = 10 * 1024 * 1024 // 10 MB

func setupLogFile(dataDir string) {
	setupLogFileNamed(dataDir, "debug.log")
}

// setupLogFileNamed redirects the standard logger to the named file
// in dataDir, truncating it first if it exceeds maxLogSize.
func setupLogFileNamed(dataDir, name string) {
	logPath := filepath.Join(dataDir, name)
	truncateLogFile(logPath, maxLogSize)
	f, err := os.OpenFile(
		logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644,
	)
	if err != nil {
		log.Printf("warning: cannot open log file: %v", err)
		return
	}
	log.SetOutput(f)
}

// truncateLogFile truncates the log file if it exceeds limit
// bytes. Symlinks are skipped to avoid truncating unrelated
// files. Errors are silently ignored since logging is
// best-effort.
func truncateLogFile(path string, limit int64) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	if info.Size() <= limit {
		return
	}
	_ = os.Truncate(path, 0)
}

func openDB(cfg config.Config) (*db.DB, error) {
	applyClassifierConfig(cfg)
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	applyCustomPricing(database, cfg)
	return database, nil
}

func openReadOnlyDB(cfg config.Config) (*db.DB, error) {
	applyClassifierConfig(cfg)
	database, err := db.OpenReadOnly(cfg.DBPath)
	if err != nil {
		return nil, schemaUpgradeHint(err)
	}
	applyCustomPricing(database, cfg)
	if err := applyCursorSecret(database, cfg); err != nil {
		database.Close()
		return nil, err
	}
	return database, nil
}

// schemaUpgradeHint augments a read-only open failure with actionable guidance
// when the archive is simply older than this binary. The pending migration only
// runs on a writable open, which read-only commands never perform, so the user
// must let the daemon (re)start to upgrade the archive. Without this, the raw
// "schema missing tool_calls.file_path" error leaves upgraders with no path
// forward; it is the failure reported in issue #929 after a version bump while
// an older daemon still owned the archive.
func schemaUpgradeHint(err error) error {
	if !db.IsSchemaUpgradeRequired(err) {
		return err
	}
	return appendDaemonRestartUpgradeHint(err)
}

func appendDaemonRestartUpgradeHint(err error) error {
	return fmt.Errorf("%w\n\n%s", err, daemonRestartUpgradeHint())
}

func daemonRestartUpgradeHint() string {
	return "This database was written by an older agentsview version and " +
		"must be upgraded before it can be read. The upgrade runs when a " +
		"writable daemon starts, so restart the daemon to let it run:\n" +
		"  - desktop app: quit and relaunch it\n" +
		"  - CLI: run `agentsview daemon restart`"
}

func openWriteDB(
	ctx context.Context,
	cfg config.Config,
) (*db.DB, *writeOwnerLock, error) {
	if err := rejectLiveWritableDaemonBeforeDirectWrite(cfg); err != nil {
		return nil, nil, err
	}
	lock, err := acquireWriteOwnerLock(ctx, writeLockDataDir(cfg))
	if err != nil {
		return nil, nil, err
	}
	database, err := openDB(cfg)
	if err != nil {
		_ = lock.Close()
		return nil, nil, err
	}
	if err := applyCursorSecret(database, cfg); err != nil {
		database.Close()
		_ = lock.Close()
		return nil, nil, err
	}
	return database, lock, nil
}

func rejectLiveWritableDaemonBeforeDirectWrite(cfg config.Config) error {
	if runningAsSyncWorker() {
		// The daemon spawned this worker after closing its writer and
		// releasing the write lock for the pass, so its still-live runtime
		// record does not mean it owns the archive. The write-owner flock
		// acquired next is the real guard: if the daemon has not actually
		// yielded it, acquireWriteOwnerLock refuses.
		return nil
	}
	dataDir := writeLockDataDir(cfg)
	if isExternalDaemonStarting(dataDir) || isLegacyDaemonStarting(dataDir) {
		return fmt.Errorf(
			"local daemon is starting and owns the SQLite archive; " +
				"refusing to write directly. Retry once it is ready",
		)
	}
	if isBackgroundLaunchActive(dataDir) &&
		!ownsForegroundServeLaunchLock(dataDir) &&
		!runningAsBackgroundChild() {
		return fmt.Errorf(
			"local daemon launch is in progress and owns the SQLite archive; " +
				"refusing to write directly. Retry once it is ready",
		)
	}
	if !hasLiveWritableDaemonRuntime(dataDir, cfg.AuthToken) {
		return nil
	}
	// hasLiveWritableDaemonRuntime intentionally ignores API/data
	// compatibility so direct writers still refuse when any live local
	// writable daemon owns the archive. FindDaemonRuntime returns only
	// compatible daemons; incompatible ones fall through to the detailed
	// error below.
	if rt := FindDaemonRuntime(dataDir, cfg.AuthToken); rt != nil && !rt.ReadOnly {
		return fmt.Errorf(
			"local daemon at %s owns the SQLite archive; refusing "+
				"to write directly. Retry through the daemon or run "+
				"`agentsview daemon stop` first",
			urlFromDaemonRuntime(rt),
		)
	}
	reason := errLocalDaemonUnreachable.Error()
	if _, err := FindIncompatibleDaemonRuntime(dataDir, cfg.AuthToken); err != nil {
		reason = err.Error()
	}
	return fmt.Errorf(
		"%s; refusing to write directly. Retry through the daemon or "+
			"run `agentsview daemon stop` first",
		reason,
	)
}

func writeLockDataDir(cfg config.Config) string {
	if cfg.DataDir != "" {
		return cfg.DataDir
	}
	if cfg.DBPath != "" {
		return filepath.Dir(cfg.DBPath)
	}
	return "."
}

func closeWriteDB(database *db.DB, lock *writeOwnerLock) {
	if database != nil {
		if err := database.Close(); err != nil {
			// A failed close means a connection may still hold the SQLite
			// file. Keep the write-owner flock so another process cannot
			// acquire writer ownership alongside the surviving connection;
			// process exit releases both the connection and the flock.
			log.Printf(
				"close sqlite database: %v; keeping write-owner lock", err,
			)
			return
		}
	}
	if lock != nil {
		if err := lock.Close(); err != nil {
			log.Printf("release sqlite write-owner lock: %v", err)
		}
	}
}

func mustOpenWriteDB(
	ctx context.Context,
	cfg config.Config,
) (*db.DB, *writeOwnerLock) {
	database, lock, err := openWriteDB(ctx, cfg)
	if err != nil {
		fatal("opening writable database: %v", err)
	}
	return database, lock
}

func applyCursorSecret(database *db.DB, cfg config.Config) error {
	if cfg.CursorSecret != "" {
		secret, err := base64.StdEncoding.DecodeString(cfg.CursorSecret)
		if err != nil {
			return fmt.Errorf("invalid cursor secret: %w", err)
		}
		database.SetCursorSecret(secret)
	}
	return nil
}

// fatal prints a formatted error to stderr and exits.
// Use instead of log.Fatalf after setupLogFile redirects
// log output to the debug log file.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", args...)
	os.Exit(1)
}

// cleanResyncTemp removes leftover temp database files from
// a prior crashed resync.
func cleanResyncTemp(dbPath string) {
	tempPath := dbPath + "-resync"
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(tempPath + suffix)
	}
}

func runInitialSync(
	ctx context.Context, engine *sync.Engine,
	startupProgress *startupStateWriter,
) sync.SyncStats {
	fmt.Println("Running initial sync...")
	t := time.Now()
	stats := engine.SyncAll(ctx, func(p sync.Progress) {
		printSyncProgress(p)
		startupProgress.SetDetail(startupProgressDetail(p))
	})
	printSyncSummary(stats, t)
	return stats
}

// runInitialResync runs ResyncAll, falling back to incremental
// sync when the resync aborts. Returns true only when every
// session in the resulting DB went through the inline signal
// path -- see resyncCoversSignals.
func runInitialResync(
	ctx context.Context, engine *sync.Engine,
	startupProgress *startupStateWriter,
) (bool, sync.SyncStats) {
	fmt.Println("Data version changed, running full resync...")
	t := time.Now()
	progress := newResyncProgressPrinter(os.Stdout, time.Now)
	stats := engine.ResyncAll(ctx, func(p sync.Progress) {
		progress.Print(p)
		startupProgress.SetDetail(startupProgressDetail(p))
	})
	progress.Finish()
	printSyncSummary(stats, t)
	resyncStats := stats

	fellBack := false
	if stats.Aborted && ctx.Err() == nil {
		fmt.Println("Resync incomplete, running incremental sync...")
		t = time.Now()
		stats = engine.SyncAll(ctx, func(p sync.Progress) {
			printSyncProgress(p)
			startupProgress.SetDetail(startupProgressDetail(p))
		})
		printSyncSummary(stats, t)
		fellBack = true
	}

	if ctx.Err() != nil {
		return false, stats
	}
	return resyncCoversSignals(resyncStats, fellBack), stats
}

type signalsBackfillMarker interface {
	MarkSignalsBackfillDone() error
}

func finishInitialResync(
	marker signalsBackfillMarker, signalsCovered bool,
) {
	// Only short-circuit BackfillSignals when resync rewrote every
	// session through the inline signal path. Aborted resyncs fall
	// back to incremental sync (existing rows untouched) and orphans
	// are copied as-is from the previous DB without recompute -- both
	// leave sessions that still need backfill.
	if !signalsCovered {
		return
	}
	if err := marker.MarkSignalsBackfillDone(); err != nil {
		log.Printf("mark signals backfill done: %v", err)
	}
}

// resyncCoversSignals returns true only when every session in
// the resulting DB went through the inline signal path:
//   - resync completed cleanly (no abort fallback to incremental
//     sync, which leaves existing rows untouched), AND
//   - no orphaned sessions were copied from the previous DB
//     (CopyOrphanedDataFrom carries existing signal columns
//     verbatim, which may be stale or missing).
//
// When false, the caller must run BackfillSignals.
func resyncCoversSignals(
	stats sync.SyncStats, fellBack bool,
) bool {
	if fellBack {
		return false
	}
	if stats.OrphanedCopied > 0 {
		return false
	}
	return true
}

func printSyncSummary(stats sync.SyncStats, t time.Time) {
	summary := fmt.Sprintf(
		"\nSync complete: %d sessions synced",
		stats.Synced,
	)
	if stats.OrphanedCopied > 0 {
		summary += fmt.Sprintf(
			", %d archived sessions preserved",
			stats.OrphanedCopied,
		)
	}
	if stats.Failed > 0 {
		summary += fmt.Sprintf(", %d failed", stats.Failed)
	}
	summary += fmt.Sprintf(
		" in %s\n", time.Since(t).Round(time.Millisecond),
	)
	summary += formatAnomalySummary(stats.Anomalies)
	fmt.Print(summary)
	for _, w := range stats.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
}

type resyncProgressPrinter struct {
	w        io.Writer
	now      func() time.Time
	label    string
	started  time.Time
	inPlace  bool
	finished bool
}

func newResyncProgressPrinter(
	w io.Writer, now func() time.Time,
) *resyncProgressPrinter {
	return &resyncProgressPrinter{w: w, now: now}
}

func (p *resyncProgressPrinter) Print(progress sync.Progress) {
	if p.finished {
		return
	}
	if progress.Phase == sync.PhaseDone {
		p.printFinalInPlaceProgress(progress)
		p.finishCurrent()
		return
	}
	label := resyncProgressLabel(progress)
	if label == "" {
		return
	}

	if progress.Phase == sync.PhaseSyncing && progress.SessionsTotal > 0 {
		if p.label != progress.Detail {
			p.finishCurrent()
			p.label = progress.Detail
			p.started = p.now()
		}
		p.inPlace = true
		fmt.Fprintf(p.w, "\r  %s\x1b[K", formatSyncProgress(progress))
		return
	}

	if p.label == label {
		return
	}
	p.finishCurrent()
	p.label = label
	p.started = p.now()
	p.inPlace = false
	fmt.Fprintf(
		p.w, "  %s...\n",
		strings.TrimSuffix(resyncProgressDisplayLabel(progress), "."),
	)
}

func (p *resyncProgressPrinter) printFinalInPlaceProgress(progress sync.Progress) {
	if !p.inPlace || p.label == "" || progress.SessionsTotal == 0 {
		return
	}
	if progress.Detail == "" {
		progress.Detail = p.label
	}
	fmt.Fprintf(p.w, "\r  %s\x1b[K", formatSyncProgress(progress))
}

func (p *resyncProgressPrinter) Finish() {
	p.finished = true
	p.finishCurrent()
}

func (p *resyncProgressPrinter) finishCurrent() {
	if p.label == "" {
		return
	}
	if p.inPlace {
		fmt.Fprint(p.w, "\n")
	}
	elapsed := p.now().Sub(p.started).Round(time.Millisecond)
	fmt.Fprintf(p.w, "  %s completed in %s\n", p.label, elapsed)
	p.label = ""
	p.started = time.Time{}
	p.inPlace = false
}

func resyncProgressLabel(p sync.Progress) string {
	return p.Detail
}

func resyncProgressDisplayLabel(p sync.Progress) string {
	if p.Detail == "" {
		return ""
	}
	if p.Hint == "" {
		return p.Detail
	}
	return p.Detail + " - " + p.Hint
}

// formatAnomalySummary renders the parser/sanitizer anomaly section of a
// sync summary. It returns an empty string on a clean run so the section
// is omitted entirely; otherwise it returns a concise, indented block
// listing per-agent parser malformed lines and the central-validation fix
// counts observed during the run.
func formatAnomalySummary(a sync.AnomalyStats) string {
	if a.IsZero() {
		return ""
	}
	var b strings.Builder
	b.WriteString("Parser anomalies (this run):\n")
	if a.MalformedLinesTotal > 0 {
		fmt.Fprintf(&b,
			"  malformed lines: %d total\n", a.MalformedLinesTotal,
		)
		for _, agent := range slices.Sorted(
			maps.Keys(a.MalformedLinesByAgent),
		) {
			fmt.Fprintf(&b,
				"    %s: %d\n", agent, a.MalformedLinesByAgent[agent],
			)
		}
	}
	if a.UnknownSchemaSessionsTotal > 0 {
		fmt.Fprintf(&b,
			"  unrecognized schema sessions: %d total\n",
			a.UnknownSchemaSessionsTotal,
		)
		for _, agent := range slices.Sorted(
			maps.Keys(a.UnknownSchemaSessionsByAgent),
		) {
			fmt.Fprintf(&b,
				"    %s: %d\n", agent, a.UnknownSchemaSessionsByAgent[agent],
			)
		}
	}
	if a.GenMetadataWithoutUsageTotal > 0 {
		fmt.Fprintf(&b,
			"  gen_metadata without usage: %d total\n",
			a.GenMetadataWithoutUsageTotal,
		)
		for _, agent := range slices.Sorted(
			maps.Keys(a.GenMetadataWithoutUsageByAgent),
		) {
			fmt.Fprintf(&b,
				"    %s: %d\n", agent, a.GenMetadataWithoutUsageByAgent[agent],
			)
		}
	}
	if !a.Sanitize.IsZero() {
		fmt.Fprintf(&b,
			"  sanitized fields: %d total\n", a.Sanitize.Total(),
		)
		for _, line := range sanitizeBreakdownLines(a.Sanitize) {
			b.WriteString("    " + line + "\n")
		}
	}
	return b.String()
}

// sanitizeBreakdownLines returns the non-zero per-category sanitize counts
// as "label: n" lines in a fixed, deterministic order.
func sanitizeBreakdownLines(s sync.SanitizeStats) []string {
	cats := []struct {
		label string
		count int
	}{
		{"control chars stripped", s.ControlCharsStripped},
		{"model clamped", s.ModelClamped},
		{"tokens clamped", s.TokensClamped},
		{"role coerced", s.RoleCoerced},
		{"timestamps blanked", s.TimestampsBlanked},
	}
	var out []string
	for _, c := range cats {
		if c.count > 0 {
			out = append(out, fmt.Sprintf("%s: %d", c.label, c.count))
		}
	}
	return out
}

// startupProgressDetail renders a one-line sync progress snapshot for
// the startup state file: the counted progress line when available,
// otherwise the bare resync step label.
func startupProgressDetail(p sync.Progress) string {
	if detail := formatSyncProgress(p); detail != "" {
		return detail
	}
	return resyncProgressDisplayLabel(p)
}

func printSyncProgress(p sync.Progress) {
	if detail := formatSyncProgress(p); detail != "" {
		fmt.Printf("\r  %s\x1b[K", detail)
		return
	}
}

func formatSyncProgress(p sync.Progress) string {
	if p.Detail != "" {
		detail := p.Detail
		if p.BytesDone > 0 || p.BytesTotal > 0 {
			detail = fmt.Sprintf("%s: %s", detail, formatByteProgress(p))
		}
		if p.SessionsTotal > 0 {
			detail = fmt.Sprintf(
				"%s: %d/%d sessions (%.0f%%) · %d messages",
				detail, p.SessionsDone, p.SessionsTotal,
				p.Percent(), p.MessagesIndexed,
			)
		}
		if p.Hint != "" {
			detail += " - " + p.Hint
		}
		return detail
	}
	if p.SessionsTotal > 0 {
		return fmt.Sprintf(
			"%d/%d sessions (%.0f%%) · %d messages",
			p.SessionsDone, p.SessionsTotal,
			p.Percent(), p.MessagesIndexed,
		)
	}
	return ""
}

func formatByteProgress(p sync.Progress) string {
	if p.BytesTotal > 0 {
		return fmt.Sprintf(
			"%s/%s (%.0f%%)",
			formatBytes(p.BytesDone), formatBytes(p.BytesTotal),
			float64(p.BytesDone)/float64(p.BytesTotal)*100,
		)
	}
	return formatBytes(p.BytesDone)
}

func startFileWatcher(
	cfg config.Config, engine *sync.Engine, onChange sync.WatchCallback,
	options sync.WatcherOptions,
) (
	stopWatcher func(), openDispatch func(), unwatchedDirs []string,
	queueRetry func(sync.WatchBatch),
) {
	t := time.Now()
	roots, unwatchedDirs, symlinkGatedDirs := collectWatchRoots(cfg)
	watcher, err := sync.NewWatcherWithCallback(
		watcherBatchDelay,
		watcherSyncMinInterval,
		onChange,
		cfg.WatchExcludePatterns,
		options,
	)
	if err != nil {
		for _, root := range roots {
			unwatchedDirs = appendUniqueStrings(unwatchedDirs, root.syncDirs()...)
		}
		if coverageErr := registerWatcherUnavailableObligations(
			options, roots, unwatchedDirs, symlinkGatedDirs,
		); coverageErr != nil {
			err = errors.Join(err, coverageErr)
		}
		log.Printf(
			"warning: file watcher unavailable: %v"+
				"; will poll every %s",
			err, unwatchedPollInterval,
		)
		return func() {}, func() {}, []string{"all"}, func(sync.WatchBatch) {}
	}

	var totalWatched int
	var shallowWatched int
	registeredRoots := make([]sync.WatchRoot, 0, len(roots))
	for _, root := range roots {
		registeredRoots = append(registeredRoots, root.registeredRoot())
	}
	results := watcher.RegisterRoots(registeredRoots, recursiveWatchBudget)
	unwatchedDirs = accountRegisteredWatchRoots(unwatchedDirs, roots, results)
	for i, r := range roots {
		result := results[i]
		if !r.exists {
			continue
		}
		totalWatched += result.Watched
		if !r.recursive {
			if result.Err == nil {
				shallowWatched++
			} else {
				unwatchedDirs = appendUniqueStrings(unwatchedDirs, r.syncDirs()...)
			}
			continue
		}
		if result.Unwatched > 0 || result.BudgetExhausted ||
			result.ResourceExhausted || result.Err != nil {
			unwatchedDirs = appendUniqueStrings(unwatchedDirs, r.syncDirs()...)
			log.Printf(
				"Couldn't watch %d directories under %s, will poll every %s",
				result.Unwatched, r.path, unwatchedPollInterval,
			)
			if result.Err != nil {
				log.Printf("watching %s: %v", r.path, result.Err)
			}
		}
	}
	if options.OnPollingRequired != nil {
		obligations := watchPollingObligations(roots, results, unwatchedDirs)
		obligations = append(obligations, symlinkPollingObligations(symlinkGatedDirs)...)
		for _, obligation := range obligations {
			if err := options.OnPollingRequired(obligation); err != nil {
				log.Printf("register polling obligation %q: %v", obligation.Key, err)
			}
		}
	}

	if shallowWatched > 0 {
		fmt.Printf(
			"Watching %d directories for changes (%d shallow) (%s)\n",
			totalWatched, shallowWatched, time.Since(t).Round(time.Millisecond),
		)
	} else {
		fmt.Printf(
			"Watching %d directories for changes (%s)\n",
			totalWatched, time.Since(t).Round(time.Millisecond),
		)
	}
	if len(unwatchedDirs) > 0 {
		fmt.Printf(
			"Polling %d roots every %s for changes\n",
			len(unwatchedDirs), unwatchedPollInterval,
		)
	}
	if err := watcher.StartCollecting(); err != nil {
		log.Printf("warning: file watcher startup failed: %v", err)
	}
	return watcher.Stop, watcher.OpenDispatch, unwatchedDirs,
		watcher.QueueRetryBatch
}

func watchPollingObligations(
	roots []watchRoot,
	results []sync.RecursiveWatchResult,
	unwatchedDirs []string,
) []sync.PollingObligation {
	byKey := make(map[string][]string)
	probes := make(map[string]string)
	represented := make(map[string]struct{})
	// The probe is the physical path whose availability gates the
	// obligation's reconciliation roots: the watch root's own path for
	// root-keyed groups, the dir itself for persistent dirs.
	add := func(key, probe string, roots ...string) {
		if key == "" {
			return
		}
		probes[key] = filepath.Clean(probe)
		for _, root := range roots {
			if root == "" {
				continue
			}
			root = filepath.Clean(root)
			byKey[key] = appendUniqueString(byKey[key], root)
			represented[root] = struct{}{}
		}
	}
	for i, root := range roots {
		var result sync.RecursiveWatchResult
		if i < len(results) {
			result = results[i]
		}
		if !result.MissingRootLifecycleOwned {
			add(root.path, root.path, root.pendingPollingDirs...)
		}
		for _, dir := range root.persistentPollingDirs {
			add("persistent:"+filepath.Clean(dir), dir, dir)
		}
		if i >= len(results) {
			// No registration result exists for this root: the watcher was
			// never constructed, so nothing covers the physical root. Gate
			// its sync scopes on the root itself, or a root that disappears
			// after the obligations are installed leaves its configured dir
			// pollable and the fallback poll reconciles it as an
			// authoritative empty discovery.
			add(root.path, root.path, root.syncDirs()...)
			continue
		}
		if result.Unwatched > 0 || result.BudgetExhausted ||
			result.ResourceExhausted || result.Err != nil {
			add(root.path, root.path, root.syncDirs()...)
		}
	}
	for _, dir := range unwatchedDirs {
		dir = filepath.Clean(dir)
		if _, ok := represented[dir]; !ok {
			add("persistent:"+dir, dir, dir)
		}
	}
	obligations := make([]sync.PollingObligation, 0, len(byKey))
	for key, roots := range byKey {
		slices.Sort(roots)
		obligations = append(obligations, sync.PollingObligation{
			Key: key, Roots: roots, Probe: probes[key],
		})
	}
	slices.SortFunc(obligations, func(a, b sync.PollingObligation) int {
		return strings.Compare(a.Key, b.Key)
	})
	return obligations
}

// registerWatcherUnavailableObligations installs the polling obligations for
// a daemon whose file watcher could not be constructed: the coverage-degraded
// fallback poll over every sync dir plus the same probe gates the success
// path registers, from watchPollingObligations (root-keyed gates on every
// physical watch root and persistent dirs) and symlinkPollingObligations
// (symlink target gates). Without the gates, the fallback would poll a
// configured dir that still exists while the nested root or symlink target
// holding every session is gone, and authoritative reconciliation would
// tombstone every baselined session beneath it. watchPollingObligations gets
// nil results because no watcher registered any roots: no per-root
// registration outcome exists, no missing-root lifecycle can be natively
// owned, and every root therefore gates its sync scopes on its own path.
// The probe gates are registered before the ungated coverage fallback: the
// poll coordinator's ticker is already live, so a tick landing between two
// synchronous registrations polls whatever is installed at that instant, and
// a fallback installed first would reconcile a scope whose gate has not
// landed yet. Returns the coverage callback's error so the caller can join
// it with the construction failure.
func registerWatcherUnavailableObligations(
	options sync.WatcherOptions,
	roots []watchRoot,
	unwatchedDirs []string,
	symlinkGatedDirs map[string][]string,
) error {
	if options.OnPollingRequired != nil {
		obligations := watchPollingObligations(roots, nil, unwatchedDirs)
		obligations = append(
			obligations, symlinkPollingObligations(symlinkGatedDirs)...,
		)
		for _, obligation := range obligations {
			if err := options.OnPollingRequired(obligation); err != nil {
				log.Printf(
					"register polling obligation %q: %v", obligation.Key, err,
				)
			}
		}
	}
	if options.OnCoverageDegraded == nil {
		return nil
	}
	return options.OnCoverageDegraded(unwatchedDirs)
}

// symlinkPollingObligations gates persistent polling of dirs whose recursive
// provider root is a symlink on the symlink target itself. The persistent
// obligation's probe is the configured dir, which can still exist while the
// symlink target holding every session is gone; polling the dir then would
// read the broken link as an empty discovery and tombstone every baselined
// session beneath it. The poll coordinator blocks a root when any obligation
// referencing it has a missing probe, so this composes with the dir's own
// persistent obligation.
func symlinkPollingObligations(
	symlinkGatedDirs map[string][]string,
) []sync.PollingObligation {
	obligations := make([]sync.PollingObligation, 0, len(symlinkGatedDirs))
	for symRoot, dirs := range symlinkGatedDirs {
		roots := make([]string, 0, len(dirs))
		for _, dir := range dirs {
			roots = appendUniqueString(roots, filepath.Clean(dir))
		}
		slices.Sort(roots)
		obligations = append(obligations, sync.PollingObligation{
			Key:   "symlink:" + filepath.Clean(symRoot),
			Roots: roots,
			Probe: filepath.Clean(symRoot),
		})
	}
	slices.SortFunc(obligations, func(a, b sync.PollingObligation) int {
		return strings.Compare(a.Key, b.Key)
	})
	return obligations
}

func accountRegisteredWatchRoots(
	unwatchedDirs []string,
	roots []watchRoot,
	results []sync.RecursiveWatchResult,
) []string {
	persistent := make(map[string]bool)
	for _, root := range roots {
		for _, dir := range root.persistentPollingDirs {
			persistent[dir] = true
		}
	}
	covered := make(map[string]bool)
	seen := make(map[string]bool)
	for i, root := range roots {
		if root.exists {
			continue
		}
		pollingDirs := root.pendingPollingDirs
		if len(pollingDirs) == 0 {
			// Preserve the helper's historical behavior for callers that construct
			// watch roots directly without collection metadata.
			pollingDirs = root.syncDirs()
		}
		owned := i < len(results) && results[i].MissingRootLifecycleOwned
		for _, dir := range pollingDirs {
			if !seen[dir] {
				seen[dir] = true
				covered[dir] = owned
			} else {
				covered[dir] = covered[dir] && owned
			}
		}
	}
	return slices.DeleteFunc(unwatchedDirs, func(dir string) bool {
		return covered[dir] && !persistent[dir]
	})
}

type watchSyncer interface {
	SyncPathsContext(context.Context, []string) error
	HasActiveSessionSourceBelow(agent, path string) (bool, error)
	ReconciliationRootsForAgent(agent string) []string
	ReconcileWatchRoots(context.Context, []string, bool) error
	ReconcileWatchRootsAfterLostEvents(context.Context, []string, bool) error
}

type watchReconciliationError struct {
	cause error
	retry sync.WatchBatch
}

func newWatchReconciliationError(
	cause error, roots []string, full, lostEvents bool,
) error {
	var scoped interface{ ReconciliationRetryRoots() []string }
	if errors.As(cause, &scoped) {
		if failedRoots := deduplicateStrings(scoped.ReconciliationRetryRoots()); len(failedRoots) > 0 {
			return &watchReconciliationError{
				cause: cause,
				retry: sync.WatchBatch{
					ReconcileRoots: failedRoots,
					LostEvents:     lostEvents,
				},
			}
		}
	}
	retry := sync.WatchBatch{FullSync: full}
	retry.LostEvents = lostEvents
	if !full {
		retry.ReconcileRoots = append([]string(nil), roots...)
	}
	return &watchReconciliationError{cause: cause, retry: retry}
}

// gapReconciliationRetryBatch classifies a failed worker-to-watcher gap
// reconciliation the same way the watcher callback path does: scoped retry
// roots when the error carries them, otherwise an authoritative full sync.
// The daemon queues the batch on the watcher before opening dispatch so the
// affected roots re-reconcile with backoff.
func gapReconciliationRetryBatch(gapErr error) sync.WatchBatch {
	var scoped interface{ ReconciliationRetryRoots() []string }
	if errors.As(gapErr, &scoped) {
		if roots := deduplicateStrings(scoped.ReconciliationRetryRoots()); len(roots) > 0 {
			return sync.WatchBatch{ReconcileRoots: roots}
		}
	}
	return sync.WatchBatch{FullSync: true}
}

func (e *watchReconciliationError) Error() string { return e.cause.Error() }

func (e *watchReconciliationError) Unwrap() error { return e.cause }

func (e *watchReconciliationError) WatchRetryBatch() sync.WatchBatch {
	retry := e.retry
	retry.Paths = append([]string(nil), retry.Paths...)
	retry.ReconcileRoots = append([]string(nil), retry.ReconcileRoots...)
	return retry
}

// syncWatchBatch applies one watcher batch to the engine. recoveryScope
// supplies the probed availability snapshot used by a full recovery
// (overflow or unscoped rename) and by directory-rename promotion, per
// probeWatchRecoveryScope. Probing at call time keeps unavailable scopes (an
// unmounted volume, a missing provider subtree) out of authoritative
// reconciliation, which would otherwise read them as empty discoveries and
// tombstone every baselined session beneath them.
func syncWatchBatch(
	ctx context.Context,
	engine watchSyncer,
	batch sync.WatchBatch,
	recoveryScope func() watchRecoveryScope,
) error {
	paths := append([]string(nil), batch.Paths...)
	full := batch.FullSync
	reconcileRoots := append([]string(nil), batch.ReconcileRoots...)
	lostEvents := batch.LostEvents
	type renameOwner struct {
		path  string
		agent string
	}
	var scope watchRecoveryScope
	scopeProbed := false
	probeScope := func() watchRecoveryScope {
		if !scopeProbed {
			scope = recoveryScope()
			scopeProbed = true
		}
		return scope
	}
	authoritativePaths := make(map[string]struct{})
	authoritativeRenames := make(map[renameOwner]struct{})
	promoteDirectoryRename := func(rename sync.WatchRename) {
		roots := engine.ReconciliationRootsForAgent(rename.Agent)
		if rename.Agent == "" || len(roots) == 0 {
			full = true
			return
		}
		// FSEvents may report only one endpoint of a cross-root move, so
		// the promotion covers every currently available root of the owning
		// provider. Unavailable siblings are deferred to their polling
		// probes: reconciling them would read an unmounted volume as an
		// empty discovery and tombstone every baselined session beneath it.
		for _, root := range roots {
			if probeScope().coversProviderRoot(root) {
				reconcileRoots = append(reconcileRoots, root)
			}
		}
	}
	for _, rename := range batch.Renames {
		owner := renameOwner{path: rename.Path, agent: rename.Agent}
		if _, authoritative := authoritativeRenames[owner]; authoritative {
			continue
		}
		switch rename.ItemType {
		case sync.ItemIsFile:
			paths = appendUniqueString(paths, rename.Path)
		case sync.ItemIsDir:
			promoteDirectoryRename(rename)
			authoritativePaths[rename.Path] = struct{}{}
			authoritativeRenames[owner] = struct{}{}
			paths = removeString(paths, rename.Path)
		default:
			info, err := os.Stat(rename.Path)
			if err == nil {
				if info.IsDir() {
					promoteDirectoryRename(rename)
					authoritativePaths[rename.Path] = struct{}{}
					authoritativeRenames[owner] = struct{}{}
					paths = removeString(paths, rename.Path)
				} else {
					paths = appendUniqueString(paths, rename.Path)
				}
				continue
			}
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("classifying watcher rename %q: %w", rename.Path, err)
			}
			hasDescendant, err := engine.HasActiveSessionSourceBelow(rename.Agent, rename.Path)
			if err != nil {
				return err
			}
			if hasDescendant {
				promoteDirectoryRename(rename)
				authoritativePaths[rename.Path] = struct{}{}
				authoritativeRenames[owner] = struct{}{}
				paths = removeString(paths, rename.Path)
			} else {
				if _, authoritative := authoritativePaths[rename.Path]; !authoritative {
					paths = appendUniqueString(paths, rename.Path)
				}
			}
		}
	}
	if len(paths) > 0 {
		if err := engine.SyncPathsContext(ctx, paths); err != nil {
			retry := sync.WatchBatch{FullSync: full, LostEvents: lostEvents}
			if !full {
				retry.Paths = append([]string(nil), paths...)
				retry.ReconcileRoots = deduplicateStrings(reconcileRoots)
			}
			return &watchReconciliationError{
				cause: err,
				retry: retry,
			}
		}
	}
	if full {
		// Scope the recovery to the currently available roots, exactly like
		// the startup gap reconciliation and the archive audit. An engine-side
		// full pass would expand to every configured dir without probing, read
		// an unmounted volume or missing provider subtree as an empty
		// discovery, and tombstone every baselined session beneath it.
		// Unavailable scopes are deferred to their polling probes instead; a
		// failed recovery retries as a full batch so availability is re-probed.
		fullRoots := probeScope().available
		if len(fullRoots) == 0 {
			return nil
		}
		var err error
		if lostEvents {
			err = engine.ReconcileWatchRootsAfterLostEvents(ctx, fullRoots, false)
		} else {
			err = engine.ReconcileWatchRoots(ctx, fullRoots, false)
		}
		if err != nil {
			return newWatchReconciliationError(err, nil, true, lostEvents)
		}
		return nil
	}
	roots := deduplicateStrings(reconcileRoots)
	if len(roots) > 0 {
		var err error
		if lostEvents {
			err = engine.ReconcileWatchRootsAfterLostEvents(ctx, roots, false)
		} else {
			err = engine.ReconcileWatchRoots(ctx, roots, false)
		}
		if err != nil {
			return newWatchReconciliationError(err, roots, false, lostEvents)
		}
	}
	return nil
}

func removeString(values []string, remove string) []string {
	return slices.DeleteFunc(values, func(value string) bool { return value == remove })
}

func deduplicateStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

type watchScope struct {
	agent   parser.AgentType
	syncDir string
}

type watchRoot struct {
	path                  string
	recursive             bool
	exists                bool
	scopes                []watchScope
	pendingPollingDirs    []string
	persistentPollingDirs []string
}

func (r watchRoot) registeredRoot() sync.WatchRoot {
	scopes := make([]sync.WatchScope, 0, len(r.scopes))
	for _, scope := range r.scopes {
		scopes = append(scopes, sync.WatchScope{
			Agent:   string(scope.agent),
			SyncDir: scope.syncDir,
		})
	}
	return sync.WatchRoot{
		Path:      r.path,
		Recursive: r.recursive,
		Exists:    r.exists,
		Scopes:    scopes,
	}
}

func (r watchRoot) syncDirs() []string {
	dirs := make([]string, 0, len(r.scopes))
	for _, scope := range r.scopes {
		dirs = appendUniqueString(dirs, scope.syncDir)
	}
	return dirs
}

// collectWatchRoots resolves the configured watch plan. symlinkGatedDirs maps
// each recursive provider root skipped because it is a symlink to the
// configured dirs whose reconciliation scope its target availability gates;
// those roots never join the watcher plan or the returned roots.
func collectWatchRoots(cfg config.Config) (
	roots []watchRoot,
	unwatchedDirs []string,
	symlinkGatedDirs map[string][]string,
) {
	rootIndexes := make(map[string]int)
	persistentPollingDirs := make(map[string]struct{})
	symlinkGatedDirs = make(map[string][]string)
	addRoot := func(agent parser.AgentType, dir, path string, recursive, exists bool) {
		path = filepath.Clean(path)
		scope := watchScope{agent: agent, syncDir: dir}
		if idx, ok := rootIndexes[path]; ok {
			roots[idx].recursive = roots[idx].recursive || recursive
			roots[idx].exists = roots[idx].exists || exists
			if !slices.Contains(roots[idx].scopes, scope) {
				roots[idx].scopes = append(roots[idx].scopes, scope)
			}
			return
		}
		rootIndexes[path] = len(roots)
		roots = append(roots, watchRoot{
			path:      path,
			recursive: recursive,
			exists:    exists,
			scopes:    []watchScope{scope},
		})
	}
	for _, def := range parser.Registry {
		for _, d := range cfg.ResolveDirs(def.Type) {
			addAgentRoot := func(dir, root string, recursive, exists bool) {
				addRoot(def.Type, dir, root, recursive, exists)
			}
			_, hasProvider := parser.ProviderFactoryByType(def.Type)
			if providerWatched, polling := collectProviderWatchRoots(def, d, addAgentRoot); providerWatched {
				if polling.persistent {
					persistentPollingDirs[d] = struct{}{}
					unwatchedDirs = appendUniqueString(unwatchedDirs, d)
				}
				for _, symRoot := range polling.symlinkRoots {
					symlinkGatedDirs[symRoot] = appendUniqueString(
						symlinkGatedDirs[symRoot], d,
					)
				}
				for _, missing := range polling.missingRoots {
					idx, ok := rootIndexes[filepath.Clean(missing)]
					if !ok || idx < 0 || idx >= len(roots) {
						continue
					}
					roots[idx].pendingPollingDirs = appendUniqueString(
						roots[idx].pendingPollingDirs, d,
					)
					unwatchedDirs = appendUniqueString(unwatchedDirs, d)
				}
				continue
			}
			if !def.FileBased {
				if hasProvider {
					persistentPollingDirs[d] = struct{}{}
					unwatchedDirs = appendUniqueString(unwatchedDirs, d)
				}
				continue
			}
			fallbackUnwatched := collectLegacyWatchRoots(def, d, addAgentRoot)
			for _, pollingDir := range fallbackUnwatched {
				persistentPollingDirs[pollingDir] = struct{}{}
				unwatchedDirs = appendUniqueString(unwatchedDirs, pollingDir)
			}
		}
	}
	for dir := range persistentPollingDirs {
		for i := range roots {
			if slices.Contains(roots[i].syncDirs(), dir) {
				roots[i].persistentPollingDirs = appendUniqueString(
					roots[i].persistentPollingDirs, dir,
				)
				break
			}
		}
	}
	return roots, unwatchedDirs, symlinkGatedDirs
}

type providerPollingReasons struct {
	missingRoots []string
	// symlinkRoots are recursive provider roots skipped from watching because
	// the root itself is a symlink. They are served by persistent polling, and
	// their target availability gates the configured dir's reconciliation
	// scope: a broken symlink streams an empty discovery without error.
	symlinkRoots []string
	persistent   bool
}

func collectProviderWatchRoots(
	def parser.AgentDef,
	dir string,
	addRoot func(dir, root string, recursive, exists bool),
) (bool, providerPollingReasons) {
	factory, ok := parser.ProviderFactoryByType(def.Type)
	if !ok {
		return false, providerPollingReasons{}
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots: []string{dir},
	})
	roots, err := parser.ResolveWatchRoots(context.Background(), provider)
	if err != nil || len(roots) == 0 {
		if err != nil && !errors.Is(err, parser.ErrUnsupportedProviderFeature) {
			log.Printf("%s provider watch plan: %v", def.Type, err)
		}
		return false, providerPollingReasons{}
	}
	planned := false
	var missingRoots []string
	var polling providerPollingReasons
	for _, providerRoot := range roots {
		root := filepath.Clean(providerRoot.Path)
		if root == "" || root == "." {
			continue
		}
		planned = true
		if providerRoot.Recursive && isSymlinkPath(root) {
			polling.persistent = true
			polling.symlinkRoots = append(polling.symlinkRoots, root)
			continue
		}
		_, err := os.Stat(root)
		exists := err == nil
		addRoot(dir, root, providerRoot.Recursive, exists)
		if exists {
			continue
		}
		missingRoots = append(missingRoots, root)
	}
	if !planned {
		return false, providerPollingReasons{}
	}
	// Portable backends need polling for missing targets. Lifecycle-aware
	// backends can claim each target after acquiring bounded ancestor coverage;
	// their activation gate reconciles the target before opening native dispatch.
	polling.missingRoots = append(polling.missingRoots, missingRoots...)
	return true, polling
}

func isSymlinkPath(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || info == nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0
}

func appendUniqueString(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func appendUniqueStrings(values []string, additions ...string) []string {
	for _, value := range additions {
		values = appendUniqueString(values, value)
	}
	return values
}

// pathCoveredByAnyWatchRootCreation reports whether path is covered by an
// existing watch root strongly enough to observe creation of the missing root.
// Recursive roots cover the whole subtree. Shallow roots only cover direct
// children because fsnotify can report that immediate directory creation, after
// which the next watcher setup can add the provider's deeper watch root.
func pathCoveredByAnyWatchRootCreation(path string, roots []watchRoot) bool {
	for _, root := range roots {
		if !root.exists {
			continue
		}
		if !root.recursive {
			if filepath.Dir(path) == root.path {
				return true
			}
			continue
		}
		if path == root.path ||
			strings.HasPrefix(path, root.path+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func collectLegacyWatchRoots(
	def parser.AgentDef,
	dir string,
	addRoot func(dir, root string, recursive, exists bool),
) []string {
	var unwatchedDirs []string
	if def.ShallowWatchRootsFunc != nil {
		for _, watchDir := range def.ShallowWatchRootsFunc(dir) {
			if _, err := os.Stat(watchDir); err == nil {
				addRoot(dir, watchDir, false, true)
			}
		}
	}
	if def.WatchRootsFunc != nil {
		watchDirs := def.WatchRootsFunc(dir)
		if len(watchDirs) == 0 {
			return append(unwatchedDirs, dir)
		}
		for _, watchDir := range watchDirs {
			if _, err := os.Stat(watchDir); err == nil {
				addRoot(dir, watchDir, !def.ShallowWatch, true)
				continue
			}
			unwatchedDirs = append(unwatchedDirs, dir)
		}
		return unwatchedDirs
	}
	if len(def.WatchSubdirs) == 0 {
		if _, err := os.Stat(dir); err == nil {
			addRoot(dir, dir, !def.ShallowWatch, true)
		}
		return unwatchedDirs
	}
	for _, sub := range def.WatchSubdirs {
		watchDir := filepath.Join(dir, sub)
		if _, err := os.Stat(watchDir); err == nil {
			addRoot(dir, watchDir, !def.ShallowWatch, true)
		}
	}
	return unwatchedDirs
}

func startPeriodicSync(
	ctx context.Context,
	cfg config.Config,
	engine *sync.Engine,
	database *db.DB,
	lock *writeOwnerLock,
	idleTracker *server.IdleTracker,
	validRemotes bool,
	emitter sync.Emitter,
) {
	if validRemotes {
		for _, rh := range cfg.RemoteHosts {
			if rh.Interval > 0 {
				go startRemoteHostSync(
					ctx, cfg, database, engine, rh, emitter, idleTracker,
				)
			}
		}
	}

	// The daily archive audit runs on its own cadence in a worker process; it
	// must never run the archive-scale pass in the daemon. Its own loop keeps the
	// scheduled reconcile below (Task 5) untouched.
	go startArchiveAudit(ctx, cfg, engine, database, lock, idleTracker, emitter)

	// Remote object roots are static config; resolve them once. The scheduled
	// reconcile targets are re-probed each tick because disk availability
	// changes.
	remoteRoots := remoteSourceSyncRoots(cfg)

	ticker := time.NewTicker(periodicSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		log.Println("Running scheduled reconciliation...")
		idleTracker.Do(func() {
			runScheduledSyncPass(ctx, engine, scheduledReconcileTargets(cfg))
			runRemoteSourceSyncPass(ctx, engine, remoteRoots)
			recomputePendingSessions(engine, database)
		})
	}
}

// startArchiveAudit drives the daily archive audit with retry-and-backoff
// scheduling. Each attempt runs entirely in a worker process (via
// runWorkerWritePass) wrapped in idleTracker.Do; a failed attempt is retained
// and retried with a growing delay rather than falling back in process.
func startArchiveAudit(
	ctx context.Context,
	cfg config.Config,
	engine *sync.Engine,
	database *db.DB,
	lock *writeOwnerLock,
	idleTracker *server.IdleTracker,
	emitter sync.Emitter,
) {
	runArchiveAuditLoop(
		ctx,
		func(ctx context.Context, delay time.Duration) bool {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-timer.C:
				return true
			}
		},
		func(ctx context.Context) bool {
			return runArchiveAuditAttempt(ctx, idleTracker, func(ctx context.Context) error {
				return runArchiveAudit(ctx, cfg, engine, database, lock, emitter)
			})
		},
	)
}

// runArchiveAuditAttempt runs one audit attempt as idle-tracked work and reports
// success. It logs the worker's actual error on failure so the terminal Error
// string is visible in debug.log, but stays quiet once the context is cancelled
// so shutdown does not emit a spurious failure line.
func runArchiveAuditAttempt(
	ctx context.Context,
	idleTracker *server.IdleTracker,
	audit func(context.Context) error,
) bool {
	ok := false
	idleTracker.Do(func() {
		if err := audit(ctx); err != nil {
			if ctx.Err() == nil {
				log.Printf("archive audit attempt failed: %v", err)
			}
			return
		}
		ok = true
	})
	return ok
}

// runArchiveAuditLoop waits, runs one audit attempt, then reschedules from the
// outcome: success returns to the daily interval; failure retries after a delay
// that doubles each time and caps at archiveAuditInterval. wait blocks for the
// delay and reports false when the context is done; audit reports whether the
// attempt succeeded.
func runArchiveAuditLoop(
	ctx context.Context,
	wait func(context.Context, time.Duration) bool,
	audit func(context.Context) bool,
) {
	delay := archiveAuditInterval
	retry := archiveAuditRetryInitial
	for {
		if !wait(ctx, delay) {
			return
		}
		if audit(ctx) {
			delay = archiveAuditInterval
			retry = archiveAuditRetryInitial
			continue
		}
		delay = retry
		if ctx.Err() == nil {
			log.Printf("archive audit failed; next attempt in %s", retry)
		}
		retry = min(retry*2, archiveAuditInterval)
	}
}

// runArchiveAudit executes one audit attempt: a full authoritative
// reconciliation in a worker process via runWorkerWritePass, never in the
// daemon. It emits "sessions" whenever the terminal result reports committed
// changes — synced or tombstoned — even when the pass also failed, because the
// retry sees those rows as already synchronized and would never re-notify SSE
// clients or the embedding scheduler. It returns an error on any failure —
// spawn, pre-launch handoff, or a ran-and-failed worker — so the caller
// retries with backoff; it never falls back to an in-process pass. The
// daemon's in-memory skip cache is reloaded by runWorkerWritePass inside the
// pass's own exclusive section, so no queued sync can re-persist stale
// entries the audit worker removed.
func runArchiveAudit(
	ctx context.Context,
	cfg config.Config,
	engine *sync.Engine,
	database *db.DB,
	lock *writeOwnerLock,
	emitter sync.Emitter,
) error {
	result, err := runWorkerWritePass(
		ctx, ctx, cfg, engine, database, lock, "audit", nil,
	)
	if (result.Synced > 0 || result.Tombstoned > 0) && emitter != nil {
		emitter.Emit("sessions")
	}
	return err
}

// scheduledSyncEngine is the reconciliation surface the scheduled pass needs.
// Native-watched providers already get event-driven sync plus degraded-coverage
// polling, so the scheduled pass only reconciles the opted-in providers.
type scheduledSyncEngine interface {
	ReconcileProviderRoots(ctx context.Context, agent parser.AgentType, roots []string) error
}

// scheduledReconcileTarget pairs an opted-in provider with the configured roots
// the scheduled pass must reconcile for it.
type scheduledReconcileTarget struct {
	Agent parser.AgentType
	Roots []string
}

// scheduledReconcileTargets selects the configured roots for providers that
// declare PeriodicReconcile. Every other provider is covered by the watcher and
// the degraded-coverage poller, so the scheduled pass leaves them untouched.
func scheduledReconcileTargets(cfg config.Config) []scheduledReconcileTarget {
	roots, _, _ := collectWatchRoots(cfg)
	deferred := make(map[watchScope]struct{})
	for _, root := range roots {
		if root.exists {
			continue
		}
		for _, scope := range root.scopes {
			deferred[scope] = struct{}{}
		}
	}
	byAgent := make(map[parser.AgentType][]string)
	for _, root := range roots {
		if !root.exists {
			continue
		}
		for _, scope := range root.scopes {
			if _, blocked := deferred[scope]; blocked {
				continue
			}
			byAgent[scope.agent] = appendUniqueString(byAgent[scope.agent], scope.syncDir)
		}
	}
	var targets []scheduledReconcileTarget
	for _, def := range parser.Registry {
		if !def.PeriodicReconcile {
			continue
		}
		dirs := byAgent[def.Type]
		if len(dirs) == 0 {
			continue
		}
		targets = append(targets, scheduledReconcileTarget{Agent: def.Type, Roots: dirs})
	}
	return targets
}

// runScheduledSyncPass reconciles each opted-in provider within its own scope.
// A failure for one provider is logged and does not block the others.
func runScheduledSyncPass(
	ctx context.Context, engine scheduledSyncEngine, targets []scheduledReconcileTarget,
) {
	for _, target := range targets {
		if err := engine.ReconcileProviderRoots(ctx, target.Agent, target.Roots); err != nil {
			log.Printf("scheduled reconciliation for %s: %v", target.Agent, err)
		}
	}
}

func startRemoteHostSync(
	ctx context.Context,
	cfg config.Config,
	database *db.DB,
	engine *sync.Engine,
	rh config.RemoteHost,
	emitter sync.Emitter,
	idleTracker *server.IdleTracker,
) {
	syncFn := remoteHostSyncFunc(
		ctx, cfg, database, engine, rh,
		func(
			ctx context.Context,
			cfg config.Config,
			database *db.DB,
			rh config.RemoteHost,
			full bool,
		) (remotesync.SyncStats, error) {
			return runRemoteSyncTransportWithCleanup(
				ctx, cfg, database, rh, full, false,
			)
		},
	)
	runRemoteHostSyncLoop(ctx, rh.Host, rh.Interval, syncFn, emitter, idleTracker, nil)
}

type remoteSyncExclusiveRunner interface {
	RunExclusive(func() error) error
}

type remoteSyncRunner func(
	context.Context,
	config.Config,
	*db.DB,
	config.RemoteHost,
	bool,
) (remotesync.SyncStats, error)

// remoteHostSyncFunc owns the HTTP cleanup registry around the engine lock.
// Its injected transport must therefore run HTTP without acquiring that
// registry recursively; SSH transports have no cleanup-registry ownership.
func remoteHostSyncFunc(
	ctx context.Context,
	cfg config.Config,
	database *db.DB,
	runner remoteSyncExclusiveRunner,
	rh config.RemoteHost,
	runRemote remoteSyncRunner,
) func() (int, error) {
	return func() (int, error) {
		if runner == nil {
			return 0, fmt.Errorf("scheduled remote sync missing exclusive runner")
		}
		runExclusive := func() (remotesync.SyncStats, error) {
			var stats remotesync.SyncStats
			err := runner.RunExclusive(func() error {
				var err error
				stats, err = runRemote(
					ctx, cfg, database, rh, database.NeedsResync(),
				)
				return err
			})
			return stats, err
		}
		var stats remotesync.SyncStats
		var err error
		if rh.Transport == config.RemoteTransportHTTP {
			stats, err = httpRemoteCleanupRegistry.Run(runExclusive)
		} else {
			stats, err = runExclusive()
		}
		return stats.SessionsSynced, err
	}
}

// runRemoteHostSyncLoop drives the per-host sync ticker. syncFn returns
// the number of sessions synced so we only emit when data changed.
// When done is non-nil, closing it stops the loop.
func runRemoteHostSyncLoop(
	ctx context.Context,
	host string,
	interval time.Duration,
	syncFn func() (int, error),
	emitter sync.Emitter,
	idleTracker *server.IdleTracker,
	done <-chan struct{},
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
		}
		log.Printf("Running scheduled remote sync for %s...", host)
		finishWork, ok := idleTracker.BeginWork()
		if !ok {
			log.Printf("scheduled remote sync %s skipped: daemon is shutting down", host)
			continue
		}
		var synced int
		var err error
		func() {
			defer finishWork()
			synced, err = syncFn()
		}()
		if err != nil {
			log.Printf("scheduled remote sync %s: %v", host, err)
			continue
		}
		if synced > 0 && emitter != nil {
			emitter.Emit("sessions")
		}
	}
}

func recomputePendingSessions(
	engine *sync.Engine, database *db.DB,
) {
	cutoff := time.Now().Add(-signals.RecencyWindow).
		UTC().Format(time.RFC3339)
	ids, err := database.PendingSignalSessions(
		context.Background(), cutoff,
	)
	if err != nil {
		log.Printf("deferred recompute query: %v", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	log.Printf(
		"recomputing signals for %d deferred sessions",
		len(ids),
	)
	for _, id := range ids {
		// Errors are already logged by RecomputeSignals; the
		// deferred-recompute loop is best-effort, the next
		// pass will retry any that failed.
		_ = engine.RecomputeSignals(context.Background(), id)
	}
}
