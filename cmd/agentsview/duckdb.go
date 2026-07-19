package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/agentsview/internal/config"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/server"
)

type DuckDBPushConfig struct {
	Full            bool
	ProjectsFlag    string
	ExcludeProjects string
	AllProjects     bool
	Watch           bool
	Debounce        time.Duration
	Interval        time.Duration
	// Automatic is set by the watch loops' automatic pushes: a mirror held
	// by a live serve process defers instead of rebuilding the whole
	// archive on every changed batch, and archive-scale diagnostics are
	// skipped (see duckdbsync.SyncOptions.Automatic). Explicit `duckdb
	// push` runs leave it false and do neither.
	Automatic bool
}

type DuckDBQuackServeConfig struct {
	Bind          string
	Path          string
	Token         string
	AllowInsecure bool
}

func runDuckDBPush(cfg DuckDBPushConfig) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFile(appCfg.DataDir)

	duckCfg, err := appCfg.ResolveDuckDB()
	if err != nil {
		fatal("duckdb push: %v", err)
	}
	projects, excludeProjects, err := resolveDuckDBPushProjects(duckCfg, cfg)
	if err != nil {
		fatal("duckdb push: %v", err)
	}
	if err := duckdbsync.ValidatePushTarget(duckCfg); err != nil {
		fatal("duckdb push: %v", err)
	}
	writeDuckDBPushPlan(os.Stdout, duckCfg, cfg, projects, excludeProjects)

	ctx, stop := signal.NotifyContext(
		context.Background(), duckDBLongRunningSignals()...,
	)
	defer stop()

	backend, cleanup, err := resolveArchiveWriteBackend(ctx, appCfg)
	if err != nil {
		fatal("opening writer: %v", err)
	}
	defer cleanup()

	if cfg.Watch {
		fmt.Printf(
			"agentsview duckdb watch: pushing to DuckDB "+
				"(debounce %s, floor %s)\n",
			cfg.Debounce, cfg.Interval,
		)
		if err := backend.DuckDBPushWatch(
			ctx, duckCfg, cfg, projects, excludeProjects,
			cfg.Debounce, cfg.Interval,
		); err != nil {
			fatal("duckdb watch: %v", err)
		}
		return
	}

	result, err := backend.DuckDBPush(
		ctx, duckCfg, cfg, projects, excludeProjects,
	)
	if err != nil {
		fatal("duckdb push: %v", err)
	}
	writeDuckDBPushDiagnostics(os.Stdout, result)
	fmt.Printf(
		"Pushed %d sessions, %d messages to DuckDB in %s\n",
		result.SessionsPushed,
		result.MessagesPushed,
		result.Duration.Round(time.Millisecond),
	)
	if result.Errors > 0 {
		fatal("duckdb push: %d session(s) failed", result.Errors)
	}
}

func writeDuckDBPushPlan(
	w io.Writer,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
) {
	target := "local file " + duckCfg.Path
	mode := "incremental"
	if cfg.Full {
		mode = "full"
	}
	fmt.Fprintf(
		w,
		"DuckDB push target: %s; machine %q; mode %s\n",
		target, duckCfg.MachineName, mode,
	)
	fmt.Fprintf(
		w, "DuckDB push filters: %s\n",
		formatDuckDBPushFilters(projects, excludeProjects),
	)
}

// writeDuckDBPushDiagnostics prints how the push selected sessions. A
// rebuild (result.Diagnostics.Full) has no incremental candidate/skip
// counters to report — pushEverything never populates them — so it gets its
// own branch that always prints, including the reason a rebuild ran instead
// of the requested incremental push (see rebuildReason in
// internal/duckdb/probe.go). Earlier code only printed anything when
// Diagnostics.Cutoff was non-empty, which incremental pushes always set but
// rebuilds never do; that made every rebuild-instead-of-incremental case
// (missing file, schema drift, a live serve holding the mirror locked, ...)
// silently print nothing here, leaving only the generic "Pushed N
// sessions..." summary with no indication a full rebuild had just run.
func writeDuckDBPushDiagnostics(w io.Writer, result duckdbsync.PushResult) {
	if result.Diagnostics.Deferred {
		reason := result.Diagnostics.DeferredReason
		if reason == "" {
			reason = "unspecified"
		}
		fmt.Fprintf(w, "DuckDB push mode: deferred (%s)\n", reason)
		return
	}
	if result.Diagnostics.Full {
		reason := result.Diagnostics.RebuildReason
		if reason == "" {
			reason = "unspecified"
		}
		fmt.Fprintf(w, "DuckDB push mode: rebuild (%s)\n", reason)
		fmt.Fprintf(
			w,
			"DuckDB push wrote: sessions %s, messages %d\n",
			formatDuckDBPushSessionCounts(result.Diagnostics.PushedSessions),
			result.MessagesPushed,
		)
		return
	}
	if result.Diagnostics.Cutoff == "" {
		return
	}
	fmt.Fprintf(
		w,
		"DuckDB push source: %s\n",
		formatDuckDBPushSource(result.Diagnostics),
	)
	fmt.Fprintf(
		w,
		"DuckDB push wrote: sessions %s, messages %d\n",
		formatDuckDBPushSessionCounts(result.Diagnostics.PushedSessions),
		result.MessagesPushed,
	)
}

// formatDuckDBPushSource renders an incremental push's source counters.
// The "local N" figure is omitted when LocalSessionCount is 0: automatic
// pushes skip the archive-scale scope count entirely (see
// duckdbsync.SyncOptions.Automatic), so 0 means "not counted", not an
// empty archive.
func formatDuckDBPushSource(d duckdbsync.PushDiagnostics) string {
	source := ""
	if d.LocalSessionCount > 0 {
		source = fmt.Sprintf("local %d; ", d.LocalSessionCount)
	}
	return source + fmt.Sprintf(
		"candidates %s; skipped unchanged %s; stale deleted %d",
		formatDuckDBPushSessionCounts(d.CandidateSessions),
		formatDuckDBPushSessionCounts(d.SkippedUnchangedSessions),
		d.DeletedStaleSessions,
	)
}

func formatDuckDBPushFilters(projects []string, excludeProjects []string) string {
	switch {
	case len(projects) > 0:
		return "include projects " + strings.Join(projects, ", ")
	case len(excludeProjects) > 0:
		return "exclude projects " + strings.Join(excludeProjects, ", ")
	default:
		return "all projects"
	}
}

func formatDuckDBPushSessionCounts(counts duckdbsync.PushSessionCounts) string {
	if len(counts.ByAgent) == 0 {
		return fmt.Sprintf("%d", counts.Total)
	}
	agents := make([]string, 0, len(counts.ByAgent))
	for agent := range counts.ByAgent {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	parts := make([]string, 0, len(agents))
	for _, agent := range agents {
		parts = append(parts, fmt.Sprintf("%s=%d", agent, counts.ByAgent[agent]))
	}
	return fmt.Sprintf("%d (%s)", counts.Total, strings.Join(parts, ", "))
}

func duckDBLongRunningSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}

func runDuckDBStatus() {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFile(appCfg.DataDir)

	duckCfg, err := appCfg.ResolveDuckDB()
	if err != nil {
		fatal("duckdb status: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	status, err := duckdbsync.ReadStatusFromConfig(ctx, duckCfg)
	if err != nil {
		fatal("duckdb status: %v", err)
	}
	if status.MirrorMissing {
		fmt.Printf(
			"DuckDB mirror not found at %s; run 'agentsview duckdb push' to create it\n",
			duckCfg.Path,
		)
		return
	}
	machine := status.LastPushMachine
	if machine == "" {
		machine = duckCfg.MachineName
	}
	scope := status.Scope
	if scope == "" {
		scope = "all projects"
	}
	fmt.Printf("Machine:         %s\n", machine)
	fmt.Printf("Last push:       %s\n", valueOrNever(status.LastPushAt))
	fmt.Printf("Schema version:  %d\n", status.SchemaVersion)
	fmt.Printf("Data version:    %d\n", status.DataVersion)
	fmt.Printf("Scope:           %s\n", scope)
	fmt.Printf("DuckDB sessions: %d\n", status.DuckDBSessions)
	fmt.Printf("DuckDB messages: %d\n", status.DuckDBMessages)
}

func loadDuckDBServeConfig(cmd *cobra.Command) (config.Config, string, error) {
	basePath, err := cmd.Flags().GetString("base-path")
	if err != nil {
		return config.Config{}, "", fmt.Errorf("reading base-path: %w", err)
	}
	cfg, err := config.LoadDuckDBServePFlags(cmd.Flags())
	if err != nil {
		return config.Config{}, "", fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return config.Config{}, "", fmt.Errorf("creating data dir: %w", err)
	}
	return cfg, basePath, nil
}

func runDuckDBServe(appCfg config.Config, basePath string) {
	setupLogFile(appCfg.DataDir)
	if appCfg.RequireAuth {
		if err := appCfg.EnsureAuthToken(); err != nil {
			fatal("duckdb serve: generating auth token: %v", err)
		}
	}
	if err := validateServeConfig(appCfg); err != nil {
		fatal("invalid serve config: %v", err)
	}

	duckCfg, err := appCfg.ResolveDuckDB()
	if err != nil {
		fatal("duckdb serve: %v", err)
	}
	if duckCfg.URL == "" && duckCfg.Path == "" {
		fatal("duckdb serve: path or url not configured")
	}

	ctx, stop := signal.NotifyContext(
		context.Background(), duckDBLongRunningSignals()...,
	)
	defer stop()

	store := openDuckDBServeStore(ctx, appCfg, duckCfg)
	defer store.Close()

	rtOpts := serveRuntimeOptions{
		Mode:          "duckdb-serve",
		RequestedPort: appCfg.Port,
	}
	appCfg, err = prepareServeRuntimeConfig(appCfg, rtOpts)
	if err != nil {
		fatal("duckdb serve: %v", err)
	}
	opts := []server.Option{
		server.WithVersion(server.VersionInfo{
			Version:   version,
			Commit:    commit,
			BuildDate: buildDate,
			ReadOnly:  true,
		}),
		server.WithDataDir(appCfg.DataDir),
		server.WithBaseContext(ctx),
	}
	if basePath != "" {
		opts = append(opts, server.WithBasePath(basePath))
	}
	srv := server.New(appCfg, store, nil, opts...)
	rt, err := startServerWithOptionalCaddy(ctx, appCfg, srv, rtOpts)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fatal("duckdb serve: %v", err)
	}
	if _, sfErr := writeDaemonRuntimeWithAuth(
		rt.Cfg.DataDir, rt.Cfg.Host, rt.Cfg.Port, version, true,
		rt.Cfg.RequireAuth,
		rt.Caddy.Pid(),
	); sfErr != nil {
		reportRuntimeRecordWrite(
			os.Stdout, sfErr,
			"duckdb serve daemon may not be discoverable by CLI", "",
		)
	} else {
		defer RemoveDaemonRuntime(rt.Cfg.DataDir)
	}
	if rt.Cfg.RequireAuth && rt.Cfg.AuthToken != "" {
		fmt.Println("Auth enabled. Token is configured.")
	}
	if rt.PublicURL == rt.LocalURL {
		fmt.Printf(
			"agentsview %s (duckdb read-only) at %s\n",
			version,
			rt.LocalURL,
		)
	} else {
		fmt.Printf(
			"agentsview %s (duckdb read-only) backend at %s, public at %s\n",
			version,
			rt.LocalURL,
			rt.PublicURL,
		)
	}
	if err := waitForServerRuntime(ctx, srv, rt); err != nil {
		fatal("duckdb serve: %v", err)
	}
}

// openDuckDBServeStore probes (local mirrors only), opens, and validates the
// DuckDB store runDuckDBServe hands to server.New: applying pricing/cursor
// secret config, checking schema compatibility, and starting the
// mirror-replacement watcher for local mirrors. It calls fatal (which exits
// the process) on any setup failure, so a returned store is always usable.
func openDuckDBServeStore(
	ctx context.Context, appCfg config.Config, duckCfg config.DuckDBConfig,
) *duckdbsync.Store {
	if duckCfg.URL == "" {
		if err := probeDuckDBMirrorForServe(ctx, duckCfg.Path); err != nil {
			fatal("duckdb serve: %v", err)
		}
		// Any reopen hardlink in the mirror's work directory here predates
		// this process: it can only have been left behind by a previous
		// serve process that crashed or was killed before removing its own
		// alias (a live Store always cleans up its own alias in Close or on
		// the next mirror-replacement swap). Safe to sweep unconditionally
		// before this process opens its own first handle — the sweep only
		// deletes inside the work directory, never siblings of the mirror.
		if err := duckdbsync.SweepStaleMirrorReopenAliases(duckCfg.Path); err != nil {
			log.Printf("duckdb serve: sweeping stale mirror reopen aliases: %v", err)
		}
	}

	applyClassifierConfig(appCfg)
	store, err := duckdbsync.NewStoreFromConfig(duckCfg)
	if err != nil {
		fatal("duckdb serve: %v", err)
	}
	if len(appCfg.CustomModelPricing) > 0 {
		store.SetCustomPricing(appCfg.CustomModelPricing)
	}
	if appCfg.CursorSecret != "" {
		secret, decErr := base64.StdEncoding.DecodeString(appCfg.CursorSecret)
		if decErr != nil {
			fatal("invalid cursor secret: %v", decErr)
		}
		store.SetCursorSecret(secret)
	}

	var schemaErr error
	if duckCfg.URL == "" {
		schemaErr = duckdbsync.CheckSchemaCompat(ctx, store.DB())
	} else {
		schemaErr = duckdbsync.CheckSchemaCompatViaQuack(ctx, store.DB())
	}
	if schemaErr != nil {
		if duckCfg.URL == "" {
			fatal("duckdb serve: schema incompatible: %v\n"+
				"Run 'agentsview duckdb push --full' to repopulate the mirror.", schemaErr)
		}
		fatal("duckdb serve: schema incompatible: %v", schemaErr)
	}
	if duckCfg.URL == "" {
		store.WatchMirrorReplacement(ctx, 2*time.Second, func(err error) {
			log.Printf("duckdb serve: mirror replacement: %v", err)
		})
	}
	return store
}

// probeDuckDBMirrorForServe checks that the local DuckDB mirror file exists
// and is schema-compatible before serve opens a live handle on it. Mirror
// schema v3 has no in-place migration path (see internal/duckdb.SchemaVersion),
// so a missing, malformed, or version-mismatched mirror is a fatal
// configuration error rather than something serve can fix by migrating.
func probeDuckDBMirrorForServe(ctx context.Context, path string) error {
	probe, err := duckdbsync.ProbeMirror(ctx, path)
	if err != nil {
		return err
	}
	return duckDBMirrorServeProbeError(probe)
}

// duckDBMirrorServeProbeError converts a failed serve-time probe into an
// actionable error: a lock conflict means another process is serving the
// file (not a damaged mirror), so the remedy differs from the rebuild case.
func duckDBMirrorServeProbeError(probe duckdbsync.MirrorProbe) error {
	reason := duckDBMirrorProbeFailureReason(probe)
	if reason == "" {
		return nil
	}
	if probe.LockConflict {
		return fmt.Errorf(
			"%s; the mirror is already open in another process (the error names "+
				"its PID) - stop that process or serve a different path", reason,
		)
	}
	return fmt.Errorf("%s; rebuild with 'agentsview duckdb push --full'", reason)
}

// duckDBMirrorProbeFailureReason reports why probe is not safe to serve
// as-is, or "" when it is. It does not check push scope (probe.Scope): that
// is a push-time concern, not a serve-time one.
func duckDBMirrorProbeFailureReason(probe duckdbsync.MirrorProbe) string {
	switch {
	case !probe.FileExists:
		return "duckdb mirror file does not exist"
	case !probe.ShapeOK:
		if probe.ShapeIssue != "" {
			return probe.ShapeIssue
		}
		return "duckdb mirror shape incompatible"
	case probe.SchemaVersion != duckdbsync.SchemaVersion:
		return fmt.Sprintf(
			"duckdb mirror schema version %d does not match this build's %d",
			probe.SchemaVersion, duckdbsync.SchemaVersion,
		)
	default:
		return ""
	}
}

func runDuckDBQuackServe(cfg DuckDBQuackServeConfig) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFile(appCfg.DataDir)

	duckCfg, err := appCfg.ResolveDuckDB()
	if err != nil {
		fatal("duckdb quack serve: %v", err)
	}
	if cfg.Path != "" {
		duckCfg.Path = cfg.Path
	}
	if cfg.AllowInsecure {
		duckCfg.AllowInsecure = true
	}
	if err := duckdbsync.ValidateQuackServeURI(
		cfg.Bind, duckCfg.AllowInsecure,
	); err != nil {
		fatal("duckdb quack serve: %v", err)
	}
	token, err := resolveQuackServeToken(cfg.Token, duckCfg.Token)
	if err != nil {
		fatal("duckdb quack serve: %v", err)
	}

	ctx, stop := signal.NotifyContext(
		context.Background(), duckDBLongRunningSignals()...,
	)
	defer stop()

	runDuckDBQuackServeLoop(ctx, duckCfg, cfg, token)
}

// Reopen tuning for runDuckDBQuackServeLoop: how often it stat-polls the
// mirror file for a rebuild-driven replacement while idle, and the
// exponential backoff it uses when a *replacement* file fails to open or
// compat-check (the initial open failure is still fatal; see the
// everServed check below).
const (
	quackServeReplacementPollInterval = 2 * time.Second
	quackServeReopenMinBackoff        = 5 * time.Second
	quackServeReopenMaxBackoff        = 60 * time.Second
)

// runDuckDBQuackServeLoop opens the mirror and serves it over Quack, then
// reopens whenever a rebuild swaps a new file into duckCfg.Path (detected
// by polling the file's identity), so a long-running quack serve process
// never gets stuck serving a stale inode. The very first open must
// succeed, matching the previous fatal-on-bad-file startup behavior; once
// serving has started at least once, a bad replacement file is logged and
// retried with backoff instead of exiting the process, mirroring 'duckdb
// serve's WatchMirrorReplacement.
func runDuckDBQuackServeLoop(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBQuackServeConfig,
	token string,
) {
	everServed := false
	backoff := quackServeReopenMinBackoff
	for {
		session, err := serveQuackOnce(ctx, duckCfg, cfg, token)
		if err != nil {
			if !everServed {
				fatal("duckdb quack serve: %v", err)
			}
			log.Printf(
				"duckdb quack serve: %v; retrying in %s", err, backoff,
			)
			if !sleepOrShutdown(ctx, backoff) {
				return
			}
			backoff = nextQuackServeBackoff(backoff)
			continue
		}
		everServed = true
		backoff = quackServeReopenMinBackoff

		replaced := waitForReplacementOrShutdown(
			ctx, duckCfg.Path, session.info, quackServeReplacementPollInterval,
		)
		session.stop()
		if !replaced {
			return
		}
	}
}

// quackServeSession is one open-mirror, quack_serve-listening generation.
type quackServeSession struct {
	info os.FileInfo
	stop func()
}

// serveQuackOnce probes and opens duckCfg.Path, installs/loads the quack
// extension, identifies this node, and starts quack_serve. It prints the
// same startup banner on every successful call, including reopens, so an
// operator watching the log can see when a rebuild swapped the mirror out
// from under a running serve (the listen URI can also change on reopen).
func serveQuackOnce(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBQuackServeConfig,
	token string,
) (quackServeSession, error) {
	if err := probeDuckDBMirrorForServe(ctx, duckCfg.Path); err != nil {
		return quackServeSession{}, err
	}
	info, err := os.Stat(duckCfg.Path)
	if err != nil {
		return quackServeSession{}, fmt.Errorf("statting duckdb mirror: %w", err)
	}
	duckdbsync.PrimeFileIdentity(info)
	conn, err := duckdbsync.Open(duckCfg.Path)
	if err != nil {
		return quackServeSession{}, err
	}
	if err := duckdbsync.CheckSchemaCompat(ctx, conn); err != nil {
		_ = conn.Close()
		return quackServeSession{}, fmt.Errorf("schema incompatible: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "INSTALL quack"); err != nil {
		_ = conn.Close()
		return quackServeSession{}, fmt.Errorf("installing quack: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "LOAD quack"); err != nil {
		_ = conn.Close()
		return quackServeSession{}, fmt.Errorf("loading quack: %w", err)
	}
	identifyQuackNode(ctx, conn, duckCfg.MachineName)

	quackInfo, err := startQuackServer(ctx, conn, cfg.Bind, token, duckCfg.AllowInsecure)
	if err != nil {
		_ = conn.Close()
		return quackServeSession{}, fmt.Errorf("starting quack server: %w", err)
	}
	writeDuckDBQuackServeStartup(os.Stdout, duckDBQuackServeStartup{
		Path: duckCfg.Path,
		Bind: cfg.Bind,
		Info: quackInfo,
	})

	stop := func() {
		if _, stopErr := conn.ExecContext(
			context.Background(), `CALL quack_stop(?)`, cfg.Bind,
		); stopErr != nil {
			log.Printf("warning: could not stop Quack server: %v", stopErr)
		}
		if closeErr := conn.Close(); closeErr != nil {
			log.Printf("warning: could not close duckdb quack mirror: %v", closeErr)
		}
	}
	return quackServeSession{info: info, stop: stop}, nil
}

// waitForReplacementOrShutdown polls path every interval until either ctx
// is done (returns false: caller should stop serving) or the file's
// identity no longer matches currentInfo (returns true: caller should
// reopen). Identity is compared with duckdbsync.SameMirrorFile rather than
// os.SameFile alone because Windows loads a FileInfo's file identity
// lazily, which makes a bare os.SameFile miss an already-completed rename
// replacement (see SameMirrorFile). Callers must have primed currentInfo's
// identity at capture time (duckdbsync.PrimeFileIdentity); the entry-time
// prime below is a second line of defense and a no-op when the caller
// already did. A stat error (the file briefly missing mid-rename) is
// treated as no change yet.
func waitForReplacementOrShutdown(
	ctx context.Context, path string, currentInfo os.FileInfo, interval time.Duration,
) bool {
	duckdbsync.PrimeFileIdentity(currentInfo)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if !duckdbsync.SameMirrorFile(currentInfo, info) {
				return true
			}
		}
	}
}

// sleepOrShutdown waits d, returning false early if ctx ends first.
func sleepOrShutdown(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextQuackServeBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > quackServeReopenMaxBackoff {
		return quackServeReopenMaxBackoff
	}
	return next
}

type duckDBQuackServeStartup struct {
	Path string
	Bind string
	Info quackServeInfo
}

func writeDuckDBQuackServeStartup(
	out io.Writer,
	startup duckDBQuackServeStartup,
) {
	fmt.Fprintf(out, "DuckDB file: %s\n", startup.Path)
	if startup.Info.ListenURI != "" {
		fmt.Fprintf(out, "Quack URI:   %s\n", startup.Info.ListenURI)
	} else {
		fmt.Fprintf(out, "Quack URI:   %s\n", startup.Bind)
	}
	if startup.Info.HTTPURL != "" {
		fmt.Fprintf(out, "HTTP URL:    %s\n", startup.Info.HTTPURL)
	}
	fmt.Fprintln(out, "Token:       configured")
	fmt.Fprintln(out, "Press Ctrl+C to stop.")
}

func resolveQuackServeToken(
	flagToken, configuredToken string,
) (string, error) {
	if flagToken != "" {
		return flagToken, nil
	}
	if configuredToken != "" {
		return configuredToken, nil
	}
	return "", fmt.Errorf(
		"token is required; set --token, AGENTSVIEW_DUCKDB_TOKEN, or [duckdb].token",
	)
}

func identifyQuackNode(ctx context.Context, conn *sql.DB, machine string) {
	meta := fmt.Sprintf(
		`{"version":%q,"commit":%q,"build_date":%q}`,
		version, commit, buildDate,
	)
	_, err := conn.ExecContext(ctx,
		`CALL quack_identify(?, ?, ?, ?, ?)`,
		"agentsview", "agentsview", machine, "", meta,
	)
	if err != nil {
		log.Printf("warning: could not identify Quack node: %v", err)
	}
}

type quackServeInfo struct {
	ListenURI string
	HTTPURL   string
}

func startQuackServer(
	ctx context.Context, conn *sql.DB, bind, token string, allowOther bool,
) (quackServeInfo, error) {
	query := `SELECT listen_uri, listen_url FROM quack_serve(?, token => ?)`
	args := []any{bind, token}
	if allowOther {
		query = `SELECT listen_uri, listen_url FROM quack_serve(?, token => ?, allow_other_hostname => ?)`
		args = append(args, allowOther)
	}
	var listenURI, httpURL sql.NullString
	if err := conn.QueryRowContext(ctx, query, args...).Scan(
		&listenURI, &httpURL,
	); err != nil {
		return quackServeInfo{}, fmt.Errorf("starting quack server: %w", err)
	}
	info := quackServeInfo{ListenURI: bind}
	if listenURI.Valid && listenURI.String != "" {
		info.ListenURI = listenURI.String
	}
	if httpURL.Valid {
		info.HTTPURL = httpURL.String
	}
	return info, nil
}

func resolveDuckDBPushProjects(
	duckCfg config.DuckDBConfig, cfg DuckDBPushConfig,
) (projects, exclude []string, err error) {
	if cfg.ProjectsFlag != "" && cfg.ExcludeProjects != "" {
		return nil, nil, fmt.Errorf(
			"--projects and --exclude-projects are mutually exclusive",
		)
	}
	if cfg.AllProjects &&
		(cfg.ProjectsFlag != "" || cfg.ExcludeProjects != "") {
		return nil, nil, fmt.Errorf(
			"--all-projects cannot be combined with --projects or --exclude-projects",
		)
	}
	projects = duckCfg.Projects
	exclude = duckCfg.ExcludeProjects
	if cfg.AllProjects {
		projects = nil
		exclude = nil
	}
	if cfg.ProjectsFlag != "" {
		projects = splitProjectList(cfg.ProjectsFlag)
		exclude = nil
	}
	if cfg.ExcludeProjects != "" {
		exclude = splitProjectList(cfg.ExcludeProjects)
		projects = nil
	}
	if len(projects) > 0 && len(exclude) > 0 {
		return nil, nil, fmt.Errorf(
			"projects and exclude_projects are mutually exclusive",
		)
	}
	return projects, exclude, nil
}
