package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
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
	appCfg config.Config
	tr     transport
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
	return b.duckDBPush(ctx, duckCfg, cfg, projects, excludeProjects, "")
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
		backend := archiveWriteBackend(b)
		cleanup := func() {}
		if reason != reasonStartup {
			var err error
			backend, cleanup, err = resolveArchiveWriteBackend(
				pctx, b.appCfg,
			)
			if err != nil {
				return err
			}
		}
		defer cleanup()
		res, err := backend.DuckDBPush(
			pctx, duckCfg, pushCfg, projects, excludeProjects,
		)
		if err != nil {
			return err
		}
		logDuckDBWatchPushResult(res, reason)
		return nil
	}
	if err := push(ctx, reasonStartup, cfg.Full); err != nil {
		log.Printf("duckdb watch: initial daemon push failed: %v", err)
	}

	loop, ticker := newPushLoopWithLabel(
		"duckdb watch", debounce, interval,
		func(c context.Context, r pushReason) error {
			return push(c, r, false)
		},
	)
	defer ticker.Stop()

	stopWatcher, unwatchedDirs := startFileWatcher(b.appCfg, nil,
		func(_ syncpkg.WatchBatch) {
			loop.NotifyDirty()
		},
	)
	defer stopWatcher()
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"duckdb watch: %d root(s) not watched; relying on the %s floor for coverage",
			len(unwatchedDirs), interval,
		)
	}

	loop.Run(ctx)
	return nil
}

func (b daemonArchiveWriteBackend) duckDBPush(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
	syncStateTarget string,
) (duckdbsync.PushResult, error) {
	if err := duckdbsync.ValidatePushTarget(duckCfg); err != nil {
		return duckdbsync.PushResult{}, err
	}
	duckCfg, err := absolutizeDuckDBPath(duckCfg)
	if err != nil {
		return duckdbsync.PushResult{}, err
	}
	if syncStateTarget == "" {
		syncStateTarget = duckdbsync.SyncStateTargetForConfig(duckCfg)
	}
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
			SyncStateTarget: syncStateTarget,
		},
		onProgress,
	)
}

func absolutizeDuckDBPath(
	duckCfg config.DuckDBConfig,
) (config.DuckDBConfig, error) {
	if duckCfg.Path == "" || filepath.IsAbs(duckCfg.Path) {
		return duckCfg, nil
	}
	abs, err := filepath.Abs(duckCfg.Path)
	if err != nil {
		return duckCfg, fmt.Errorf("resolving duckdb path: %w", err)
	}
	duckCfg.Path = abs
	return duckCfg, nil
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
	push := func(pctx context.Context, reason pushReason, full bool) error {
		pushCfg := cfg
		pushCfg.Full = full
		backend := archiveWriteBackend(b)
		cleanup := func() {}
		if reason != reasonStartup {
			var err error
			backend, cleanup, err = resolveArchiveWriteBackend(
				pctx, b.appCfg,
			)
			if err != nil {
				return err
			}
		}
		defer cleanup()
		res, err := backend.PGPush(
			pctx, target, pushCfg, projects, exclude,
		)
		if err != nil {
			return err
		}
		logPGWatchPushResult(res, reason)
		return nil
	}
	if err := push(ctx, reasonStartup, cfg.Full); err != nil {
		log.Printf("pg watch: initial daemon push failed: %v", err)
	}

	loop, ticker := newPushLoop(debounce, interval,
		func(c context.Context, r pushReason) error {
			return push(c, r, false)
		},
	)
	defer ticker.Stop()

	stopWatcher, unwatchedDirs := startFileWatcher(b.appCfg, nil,
		func(_ syncpkg.WatchBatch) {
			loop.NotifyDirty()
		},
	)
	defer stopWatcher()
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"pg watch: %d root(s) not watched; relying on the %s floor for coverage",
			len(unwatchedDirs), interval,
		)
	}

	loop.Run(ctx)
	return nil
}

type localArchiveWriteBackend struct {
	appCfg        config.Config
	database      *db.DB
	ensurePricing func(context.Context, *db.DB) error
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
	result, err := ps.Push(ctx, forceFull, newPGPushProgressPrinter())
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
	return b.duckDBPush(
		ctx, duckCfg, cfg, projects, excludeProjects, "",
	)
}

func (b *localArchiveWriteBackend) duckDBPush(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
	syncStateTarget string,
) (duckdbsync.PushResult, error) {
	if err := duckdbsync.ValidatePushTarget(duckCfg); err != nil {
		return duckdbsync.PushResult{}, err
	}
	didResync := runLocalSync(ctx, b.appCfg, b.database, cfg.Full)
	if err := ctx.Err(); err != nil {
		return duckdbsync.PushResult{}, err
	}
	forceFull := cfg.Full || didResync
	if syncStateTarget == "" {
		syncStateTarget = duckdbsync.SyncStateTargetForConfig(duckCfg)
	}

	fmt.Println("Opening DuckDB mirror...")
	connectStart := time.Now()
	opts := duckdbsync.SyncOptions{
		Projects:        projects,
		ExcludeProjects: excludeProjects,
		SyncStateTarget: syncStateTarget,
	}
	var syncer *duckdbsync.Sync
	var err error
	if duckCfg.URL != "" {
		syncer, err = duckdbsync.NewFromConfig(
			duckCfg, b.database, opts,
		)
	} else {
		syncer, err = duckdbsync.New(
			duckCfg.Path, b.database, duckCfg.MachineName, opts,
		)
	}
	if err != nil {
		return duckdbsync.PushResult{}, err
	}
	defer syncer.Close()
	fmt.Printf(
		"Opened DuckDB mirror in %s\n",
		time.Since(connectStart).Round(time.Millisecond),
	)

	fmt.Println("Preparing DuckDB schema...")
	schemaStart := time.Now()
	if err := syncer.EnsureSchema(ctx); err != nil {
		return duckdbsync.PushResult{}, fmt.Errorf("schema: %w", err)
	}
	fmt.Printf(
		"DuckDB schema ready in %s\n",
		time.Since(schemaStart).Round(time.Millisecond),
	)
	fmt.Println("Starting DuckDB push...")
	result, err := syncer.Push(ctx, forceFull,
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
		res, err := b.DuckDBPush(pctx, duckCfg, pushCfg, projects, exclude)
		if err != nil {
			return err
		}
		logDuckDBWatchPushResult(res, reason)
		return nil
	}
	if err := push(ctx, reasonStartup, cfg.Full); err != nil {
		log.Printf("duckdb watch: initial push failed: %v", err)
	}

	loop, ticker := newPushLoopWithLabel(
		"duckdb watch", debounce, interval,
		func(c context.Context, r pushReason) error {
			return push(c, r, false)
		},
	)
	defer ticker.Stop()

	stopWatcher, unwatchedDirs := startFileWatcher(b.appCfg, nil,
		func(_ syncpkg.WatchBatch) {
			loop.NotifyDirty()
		},
	)
	defer stopWatcher()
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"duckdb watch: %d root(s) not watched; relying on the %s floor for coverage",
			len(unwatchedDirs), interval,
		)
	}

	loop.Run(ctx)
	return nil
}

func logDuckDBWatchPushResult(res duckdbsync.PushResult, reason pushReason) {
	if res.Diagnostics.Cutoff != "" {
		log.Printf(
			"duckdb watch: source local %s; candidates %s; skipped unchanged %s; stale deleted %d; wrote sessions %s, messages %d (%s)",
			formatDuckDBPushSessionCounts(res.Diagnostics.LocalSessions),
			formatDuckDBPushSessionCounts(res.Diagnostics.CandidateSessions),
			formatDuckDBPushSessionCounts(res.Diagnostics.SkippedUnchangedSessions),
			res.Diagnostics.DeletedStaleSessions,
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
		SourceMachines:          b.appCfg.SourceMachines,
		IncludeCwdPrefixes:      b.appCfg.SyncIncludeCwdPrefixes,
		Machine:                 b.appCfg.LocalMachineName,
		BlockedResultCategories: b.appCfg.ResultContentBlockedCategories,
	})
	defer engine.Close()

	didResync, err := runPGWatchStartupSync(ctx, engine, cfg.Full)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}

	// One vectors.db adapter for the watch loop's lifetime: connect runs on
	// every reconnect, and a fresh source per reconnect would leak the
	// previous one's memoized read-only handle (postgres.Sync never closes
	// its source). The adapter is designed for reuse — it reopens lazily
	// after transient failures.
	vectorSource := pgVectorPushSource(b.appCfg, target, cfg)
	defer closeVectorPushSource(vectorSource)

	pusher := b.newPGPusher(
		func(c context.Context) error {
			engine.SyncAll(c, nil)
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
	defer pusher.reset()

	fmt.Printf(
		"agentsview pg watch: pushing to PostgreSQL as %q "+
			"(debounce %s, floor %s)\n",
		target.PG.MachineName, debounce, interval,
	)

	if err := pusher.push(ctx, reasonStartup, didResync); err != nil {
		log.Printf("pg watch: initial push failed: %v", err)
	}

	loop, ticker := newPushLoop(debounce, interval,
		func(c context.Context, r pushReason) error {
			return pusher.push(c, r, false)
		},
	)
	defer ticker.Stop()

	stopWatcher, unwatchedDirs := startFileWatcher(b.appCfg, engine,
		func(batch syncpkg.WatchBatch) {
			syncWatchBatch(ctx, engine, batch)
			loop.NotifyDirty()
		},
	)
	defer stopWatcher()
	if len(unwatchedDirs) > 0 {
		log.Printf(
			"pg watch: %d root(s) not watched; relying on the %s floor for coverage",
			len(unwatchedDirs), interval,
		)
	}

	loop.Run(ctx)
	return nil
}

func runPGWatchStartupSync(
	ctx context.Context,
	engine *syncpkg.Engine,
	full bool,
) (bool, error) {
	didResync := false
	_, err := engine.SyncThenRun(ctx, full, nil,
		func(forceFull bool) error {
			didResync = forceFull
			return nil
		})
	if err != nil {
		return false, err
	}
	return didResync, nil
}
