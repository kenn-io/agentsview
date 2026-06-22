package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
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
	periodicSyncInterval  = 15 * time.Minute
	telemetryPingInterval = 24 * time.Hour
	unwatchedPollInterval = 2 * time.Minute
	watcherDebounce       = 500 * time.Millisecond
	recursiveWatchBudget  = 8192
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
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(code)
	}
}

// warnMissingDirs prints a warning to stderr for each
// configured directory that does not exist or is
// inaccessible.
func warnMissingDirs(dirs []string, label string) {
	for _, d := range dirs {
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

func runServe(cfg config.Config) {
	start := time.Now()
	setupLogFile(cfg.DataDir)

	if err := validateServeConfig(cfg); err != nil {
		fatal("invalid serve config: %v", err)
	}

	// When auth is required, ensure a token exists before publishing
	// startup state so waiting CLI probes can authenticate the first
	// protected /api/ping after startup completes.
	if cfg.RequireAuth {
		if err := cfg.EnsureAuthToken(); err != nil {
			log.Fatalf("Failed to generate auth token: %v", err)
		}
		// A background child redirects stdout to serve.log; printing the
		// token there would persist it to a file. The parent already
		// printed the token to the invoking terminal, so the child stays
		// quiet about it.
		if cfg.AuthToken != "" && !runningAsBackgroundChild() {
			fmt.Printf("Auth enabled. Token: %s\n", cfg.AuthToken)
		}
	}

	// Acquire the daemon start lock immediately after config setup,
	// before opening the DB, so token-use never sees a window
	// with no lock and no runtime record during startup.
	MarkDaemonStarting(cfg.DataDir)
	defer UnmarkDaemonStarting(cfg.DataDir)

	applyClassifierConfig(cfg)
	database := mustOpenDB(cfg)
	defer database.Close()

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

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

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

	var engine *sync.Engine
	if !cfg.NoSync {
		engine = sync.NewEngine(database, sync.EngineConfig{
			AgentDirs:               cfg.AgentDirs,
			Machine:                 "local",
			BlockedResultCategories: cfg.ResultContentBlockedCategories,
			Emitter:                 broadcaster,
		})

		if database.NeedsResync() {
			signalsCovered := runInitialResync(ctx, engine)
			if ctx.Err() == nil {
				if err := database.Vacuum(); err != nil {
					log.Printf("vacuum after resync: %v", err)
				}
				// Only short-circuit BackfillSignals when resync
				// rewrote every session through the inline signal
				// path. Aborted resyncs fall back to incremental
				// sync (existing rows untouched) and orphans are
				// copied as-is from the previous DB without
				// recompute -- both leave sessions that still
				// need backfill.
				if signalsCovered {
					if err := database.MarkSignalsBackfillDone(); err != nil {
						log.Printf(
							"mark signals backfill done: %v", err,
						)
					}
				}
			}
		} else {
			runInitialSync(ctx, engine)
		}
		if ctx.Err() != nil {
			return
		}

		// Backfill runs in the background. On a large DB (e.g.
		// after copying tens of thousands of orphaned sessions
		// during a resync), walking every row to recompute
		// signals would otherwise block the HTTP server from
		// listening for minutes. Backfill is idempotent and
		// guarded by a one-shot marker, so concurrent writes
		// from the file watcher and periodic sync are safe.
		go func() {
			if err := database.BackfillSignals(
				ctx,
				func(bCtx context.Context, id string) error {
					return engine.RecomputeSignals(bCtx, id)
				},
			); err != nil && ctx.Err() == nil {
				log.Printf("signals backfill: %v", err)
			}
		}()

		go startPeriodicSync(engine, database)
	}

	// Seed model_pricing after any resync swap so the new DB
	// file (which doesn't carry pricing across the swap) is
	// populated before the dashboard starts answering
	// requests. Synchronous fallback upsert so the first
	// usage page load does not observe an empty table;
	// background LiteLLM refresh follows immediately.
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

	srv := server.New(cfg, database, engine,
		server.WithVersion(server.VersionInfo{
			Version:   version,
			Commit:    commit,
			BuildDate: buildDate,
		}),
		server.WithDataDir(cfg.DataDir),
		server.WithBaseContext(ctx),
		server.WithBroadcaster(broadcaster),
	)

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
	if _, sfErr := WriteDaemonRuntime(
		rt.Cfg.DataDir, rt.Cfg.Host, rt.Cfg.Port, version, false,
		rt.Caddy.Pid(),
	); sfErr != nil {
		log.Printf(
			"warning: could not write daemon runtime record: %v"+
				" (keeping start lock as fallback)",
			sfErr,
		)
	} else {
		defer RemoveDaemonRuntime(rt.Cfg.DataDir)
		UnmarkDaemonStarting(rt.Cfg.DataDir)
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

	if engine != nil {
		stopWatcher, unwatchedDirs := startFileWatcher(
			cfg, engine, func(paths []string) {
				engine.SyncPaths(paths)
			},
		)
		defer stopWatcher()
		if len(unwatchedDirs) > 0 {
			go startUnwatchedPoll(engine, unwatchedDirs)
		}
	}

	if err := waitForServerRuntime(ctx, srv, rt); err != nil {
		fatal("%v", err)
	}
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

func mustOpenDB(cfg config.Config) *db.DB {
	database, err := openDB(cfg)
	if err != nil {
		fatal("opening database: %v", err)
	}

	if cfg.CursorSecret != "" {
		secret, err := base64.StdEncoding.DecodeString(cfg.CursorSecret)
		if err != nil {
			fatal("invalid cursor secret: %v", err)
		}
		database.SetCursorSecret(secret)
	}

	return database
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
) {
	fmt.Println("Running initial sync...")
	t := time.Now()
	stats := engine.SyncAll(ctx, printSyncProgress)
	printSyncSummary(stats, t)
}

// runInitialResync runs ResyncAll, falling back to incremental
// sync when the resync aborts. Returns true only when every
// session in the resulting DB went through the inline signal
// path -- see resyncCoversSignals.
func runInitialResync(
	ctx context.Context, engine *sync.Engine,
) bool {
	fmt.Println("Data version changed, running full resync...")
	t := time.Now()
	stats := engine.ResyncAll(ctx, printSyncProgress)
	printSyncSummary(stats, t)

	fellBack := false
	if stats.Aborted && ctx.Err() == nil {
		fmt.Println("Resync incomplete, running incremental sync...")
		t = time.Now()
		fallback := engine.SyncAll(ctx, printSyncProgress)
		printSyncSummary(fallback, t)
		fellBack = true
	}

	if ctx.Err() != nil {
		return false
	}
	return resyncCoversSignals(stats, fellBack)
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
	fmt.Print(summary)
	for _, w := range stats.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
}

func printSyncProgress(p sync.Progress) {
	if p.Detail != "" {
		detail := p.Detail
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
		fmt.Printf("\r  %s\x1b[K", detail)
		return
	}
	if p.SessionsTotal > 0 {
		fmt.Printf(
			"\r  %d/%d sessions (%.0f%%) · %d messages\x1b[K",
			p.SessionsDone, p.SessionsTotal,
			p.Percent(), p.MessagesIndexed,
		)
	}
}

func startFileWatcher(
	cfg config.Config, engine *sync.Engine, onChange func(paths []string),
) (stopWatcher func(), unwatchedDirs []string) {
	t := time.Now()
	watcher, err := sync.NewWatcher(watcherDebounce, onChange, cfg.WatchExcludePatterns)
	if err != nil {
		log.Printf(
			"warning: file watcher unavailable: %v"+
				"; will poll every %s",
			err, unwatchedPollInterval,
		)
		return func() {}, []string{"all"}
	}

	roots, unwatchedDirs := collectWatchRoots(cfg)

	var totalWatched int
	var shallowWatched int
	remaining := recursiveWatchBudget
	for _, r := range roots {
		if r.shallow {
			if watcher.WatchShallow(r.root) {
				shallowWatched++
				totalWatched++
			} else {
				unwatchedDirs = append(unwatchedDirs, r.dirs...)
			}
			continue
		}
		result := watcher.WatchRecursiveBudgeted(r.root, remaining)
		totalWatched += result.Watched
		remaining -= result.Watched
		if result.Unwatched > 0 || result.BudgetExhausted ||
			result.ResourceExhausted || result.Err != nil {
			unwatchedDirs = append(unwatchedDirs, r.dirs...)
			log.Printf(
				"Couldn't watch %d directories under %s, will poll every %s",
				result.Unwatched, r.root, unwatchedPollInterval,
			)
			if result.Err != nil {
				log.Printf("watching %s: %v", r.root, result.Err)
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
	watcher.Start()
	return watcher.Stop, unwatchedDirs
}

type watchRoot struct {
	dirs    []string
	root    string // actual path passed to WatchRecursive
	shallow bool   // use shallow watch (root only)
}

func collectWatchRoots(cfg config.Config) (roots []watchRoot, unwatchedDirs []string) {
	rootIndexes := make(map[string]int)
	addRoot := func(dir, root string, shallow bool) {
		if idx, ok := rootIndexes[root]; ok {
			if !slices.Contains(roots[idx].dirs, dir) {
				roots[idx].dirs = append(roots[idx].dirs, dir)
			}
			return
		}
		rootIndexes[root] = len(roots)
		roots = append(roots, watchRoot{
			dirs:    []string{dir},
			root:    root,
			shallow: shallow,
		})
	}
	for _, def := range parser.Registry {
		if !def.FileBased {
			continue
		}
		for _, d := range cfg.ResolveDirs(def.Type) {
			if def.ShallowWatchRootsFunc != nil {
				for _, watchDir := range def.ShallowWatchRootsFunc(d) {
					if _, err := os.Stat(watchDir); err == nil {
						addRoot(d, watchDir, true)
					}
				}
			}
			if def.WatchRootsFunc != nil {
				watchDirs := def.WatchRootsFunc(d)
				if len(watchDirs) == 0 {
					unwatchedDirs = append(unwatchedDirs, d)
					continue
				}
				for _, watchDir := range watchDirs {
					if _, err := os.Stat(watchDir); err == nil {
						addRoot(d, watchDir, def.ShallowWatch)
						continue
					}
					unwatchedDirs = append(unwatchedDirs, d)
				}
				continue
			}
			if len(def.WatchSubdirs) == 0 {
				if _, err := os.Stat(d); err == nil {
					addRoot(d, d, def.ShallowWatch)
				}
				continue
			}
			for _, sub := range def.WatchSubdirs {
				watchDir := filepath.Join(d, sub)
				if _, err := os.Stat(watchDir); err == nil {
					addRoot(d, watchDir, def.ShallowWatch)
				}
			}
		}
	}
	return roots, unwatchedDirs
}

func startPeriodicSync(
	engine *sync.Engine, database *db.DB,
) {
	ticker := time.NewTicker(periodicSyncInterval)
	defer ticker.Stop()
	for range ticker.C {
		log.Println("Running scheduled sync...")
		engine.SyncAll(context.Background(), nil)
		recomputePendingSessions(engine, database)
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

type unwatchedPollSyncer interface {
	SyncRootsSince(
		context.Context, []string, time.Time, sync.ProgressFunc,
	) sync.SyncStats
}

func startUnwatchedPoll(engine unwatchedPollSyncer, roots []string) {
	ticker := time.NewTicker(unwatchedPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		log.Println("Polling unwatched directories...")
		pollUnwatchedRootsOnce(engine, roots)
	}
}

func pollUnwatchedRootsOnce(engine unwatchedPollSyncer, roots []string) {
	engine.SyncRootsSince(context.Background(), roots, time.Time{}, nil)
}
