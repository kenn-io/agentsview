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
	"slices"
	"strings"
	"syscall"
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
	ReplaceDaemon  bool
	NoSyncExplicit bool
	Pprof          bool
}

func runServe(cfg config.Config, opts serveOptions) {
	start := time.Now()
	setupLogFile(cfg.DataDir)

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

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()
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
				finishInitialResync(database, signalsCovered)
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
		go idleTracker.Do(func() {
			if err := database.BackfillSignals(
				ctx,
				func(bCtx context.Context, id string) error {
					return engine.RecomputeSignals(bCtx, id)
				},
			); err != nil && ctx.Err() == nil {
				log.Printf("signals backfill: %v", err)
			}
		})

		validRemotes := true
		if err := cfg.ValidateRemoteHosts(); err != nil {
			log.Printf("warning: remote_hosts config invalid, skipping periodic remote sync: %v", err)
			validRemotes = false
		}
		go startPeriodicSync(ctx, cfg, engine, database, idleTracker, validRemotes, broadcaster)
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
		server.WithIdleTracker(idleTracker),
		server.WithPprof(opts.Pprof),
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
	if _, sfErr := WriteDaemonRuntimeWithAuthAndNoSync(
		rt.Cfg.DataDir, rt.Cfg.Host, rt.Cfg.Port, version, false,
		rt.Cfg.RequireAuth, rt.Cfg.NoSync,
		rt.Caddy.Pid(),
	); sfErr != nil {
		log.Printf(
			"warning: could not write daemon runtime record: %v"+
				" (keeping start lock as fallback)",
			sfErr,
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
		// Registered before stopWatcher so LIFO defer order stops
		// the watcher first, then Close flushes any pending
		// debounced signal recomputes.
		defer engine.Close()
		stopWatcher, unwatchedDirs := startFileWatcher(
			cfg, engine, func(paths []string) {
				idleTracker.Do(func() {
					engine.SyncPaths(paths)
				})
			},
		)
		defer stopWatcher()
		if len(unwatchedDirs) > 0 {
			go startUnwatchedPoll(engine, unwatchedDirs, idleTracker)
		}
	}

	if err := waitForServerRuntime(ctx, srv, rt); err != nil {
		fatal("%v", err)
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
		"  - CLI: run `agentsview serve --replace`"
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
	dataDir := writeLockDataDir(cfg)
	if isExternalDaemonStarting(dataDir) || isLegacyDaemonStarting(dataDir) {
		return fmt.Errorf(
			"local daemon is starting and owns the SQLite archive; " +
				"refusing to write directly. Retry once it is ready " +
				"or run `agentsview serve stop` first",
		)
	}
	if isBackgroundLaunchActive(dataDir) &&
		!ownsForegroundServeLaunchLock(dataDir) &&
		!runningAsBackgroundChild() {
		return fmt.Errorf(
			"local daemon launch is in progress and owns the SQLite archive; " +
				"refusing to write directly. Retry once it is ready " +
				"or run `agentsview serve stop` first",
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
				"`agentsview serve stop` first",
			urlFromDaemonRuntime(rt),
		)
	}
	reason := errLocalDaemonUnreachable.Error()
	if _, err := FindIncompatibleDaemonRuntime(dataDir, cfg.AuthToken); err != nil {
		reason = err.Error()
	}
	return fmt.Errorf(
		"%s; refusing to write directly. Retry through the daemon or "+
			"run `agentsview serve stop` first",
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
		database.Close()
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
	progress := newResyncProgressPrinter(os.Stdout, time.Now)
	stats := engine.ResyncAll(ctx, progress.Print)
	progress.Finish()
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
			if providerWatched, providerUnwatched := collectProviderWatchRoots(def, d, addRoot); providerWatched {
				unwatchedDirs = append(unwatchedDirs, providerUnwatched...)
				continue
			}
			fallbackUnwatched := collectLegacyWatchRoots(def, d, addRoot)
			unwatchedDirs = append(unwatchedDirs, fallbackUnwatched...)
		}
	}
	return roots, unwatchedDirs
}

func collectProviderWatchRoots(
	def parser.AgentDef,
	dir string,
	addRoot func(dir, root string, shallow bool),
) (bool, []string) {
	factory, ok := parser.ProviderFactoryByType(def.Type)
	if !ok {
		return false, nil
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots: []string{dir},
	})
	plan, err := provider.WatchPlan(context.Background())
	if err != nil || len(plan.Roots) == 0 {
		if err != nil && !errors.Is(err, parser.ErrUnsupportedProviderFeature) {
			log.Printf("%s provider watch plan: %v", def.Type, err)
		}
		return false, nil
	}
	added := false
	var addedRoots []watchRoot
	var missingRoots []string
	var unwatchedDirs []string
	for _, providerRoot := range plan.Roots {
		root := filepath.Clean(providerRoot.Path)
		if root == "" || root == "." {
			continue
		}
		if providerRoot.Recursive && isSymlinkPath(root) {
			unwatchedDirs = appendUniqueString(unwatchedDirs, dir)
			continue
		}
		if _, err := os.Stat(root); err == nil {
			addRoot(dir, root, !providerRoot.Recursive)
			added = true
			addedRoots = append(addedRoots, watchRoot{
				root:    root,
				shallow: !providerRoot.Recursive,
			})
			continue
		}
		missingRoots = append(missingRoots, root)
	}
	if !added {
		if len(unwatchedDirs) > 0 {
			return true, unwatchedDirs
		}
		return false, nil
	}
	// A watch target that does not exist yet but lives under an already-watched
	// root needs no separate polling only when the ancestor is recursive or
	// when a shallow root can observe creation of the missing root itself. A
	// shallow ancestor sees only immediate child creation, so it cannot cover a
	// missing nested provider root.
	for _, missing := range missingRoots {
		if !pathCoveredByAnyWatchRootCreation(missing, addedRoots) {
			unwatchedDirs = appendUniqueString(unwatchedDirs, dir)
		}
	}
	return true, unwatchedDirs
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

// pathCoveredByAnyWatchRootCreation reports whether path is covered by an
// existing watch root strongly enough to observe creation of the missing root.
// Recursive roots cover the whole subtree. Shallow roots only cover direct
// children because fsnotify can report that immediate directory creation, after
// which the next watcher setup can add the provider's deeper watch root.
func pathCoveredByAnyWatchRootCreation(path string, roots []watchRoot) bool {
	for _, root := range roots {
		if root.shallow {
			if filepath.Dir(path) == root.root {
				return true
			}
			continue
		}
		if path == root.root ||
			strings.HasPrefix(path, root.root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func collectLegacyWatchRoots(
	def parser.AgentDef,
	dir string,
	addRoot func(dir, root string, shallow bool),
) []string {
	var unwatchedDirs []string
	if def.ShallowWatchRootsFunc != nil {
		for _, watchDir := range def.ShallowWatchRootsFunc(dir) {
			if _, err := os.Stat(watchDir); err == nil {
				addRoot(dir, watchDir, true)
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
				addRoot(dir, watchDir, def.ShallowWatch)
				continue
			}
			unwatchedDirs = append(unwatchedDirs, dir)
		}
		return unwatchedDirs
	}
	if len(def.WatchSubdirs) == 0 {
		if _, err := os.Stat(dir); err == nil {
			addRoot(dir, dir, def.ShallowWatch)
		}
		return unwatchedDirs
	}
	for _, sub := range def.WatchSubdirs {
		watchDir := filepath.Join(dir, sub)
		if _, err := os.Stat(watchDir); err == nil {
			addRoot(dir, watchDir, def.ShallowWatch)
		}
	}
	return unwatchedDirs
}

func startPeriodicSync(
	ctx context.Context,
	cfg config.Config,
	engine *sync.Engine,
	database *db.DB,
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
	ticker := time.NewTicker(periodicSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		log.Println("Running scheduled sync...")
		idleTracker.Do(func() {
			engine.SyncAll(ctx, nil)
			recomputePendingSessions(engine, database)
		})
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
		runRemoteSyncTransport,
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
		var stats remotesync.SyncStats
		err := runner.RunExclusive(func() error {
			var err error
			stats, err = runRemote(ctx, cfg, database, rh, database.NeedsResync())
			return err
		})
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

type unwatchedPollSyncer interface {
	SyncRootsSince(
		context.Context, []string, time.Time, sync.ProgressFunc,
	) sync.SyncStats
}

func startUnwatchedPoll(
	engine unwatchedPollSyncer,
	roots []string,
	idleTracker *server.IdleTracker,
) {
	ticker := time.NewTicker(unwatchedPollInterval)
	defer ticker.Stop()
	for range ticker.C {
		log.Println("Polling unwatched directories...")
		idleTracker.Do(func() {
			pollUnwatchedRootsOnce(engine, roots)
		})
	}
}

func pollUnwatchedRootsOnce(engine unwatchedPollSyncer, roots []string) {
	engine.SyncRootsSince(context.Background(), roots, time.Time{}, nil)
}
