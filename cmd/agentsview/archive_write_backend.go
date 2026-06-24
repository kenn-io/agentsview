package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
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

func (b daemonArchiveWriteBackend) PGPush(
	ctx context.Context,
	target pgTargetSelection,
	cfg PGPushConfig,
	projects []string,
	excludeProjects []string,
) (postgres.PushResult, error) {
	return postDaemonPush[postgres.PushResult](
		ctx, b.tr, b.appCfg.AuthToken, "/api/v1/push/pg",
		daemonPushRequest{
			Full:                   cfg.Full,
			Projects:               projects,
			ExcludeProjects:        excludeProjects,
			PG:                     &target.PG,
			SyncStateTarget:        target.SyncStateTarget,
			MigrateLegacySyncState: target.MigrateLegacySyncState,
		},
	)
}

func (b daemonArchiveWriteBackend) DuckDBPush(
	ctx context.Context,
	duckCfg config.DuckDBConfig,
	cfg DuckDBPushConfig,
	projects []string,
	excludeProjects []string,
) (duckdbsync.PushResult, error) {
	duckCfg, err := absolutizeDuckDBPath(duckCfg)
	if err != nil {
		return duckdbsync.PushResult{}, err
	}
	return postDaemonPush[duckdbsync.PushResult](
		ctx, b.tr, b.appCfg.AuthToken, "/api/v1/push/duckdb",
		daemonPushRequest{
			Full:            cfg.Full,
			Projects:        projects,
			ExcludeProjects: excludeProjects,
			DuckDB:          &duckCfg,
		},
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
		func(_ []string) {
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
	appCfg   config.Config
	database *db.DB
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
	forceFull := cfg.Full || didResync

	fmt.Println("Connecting to PostgreSQL...")
	connectStart := time.Now()
	applyClassifierConfig(b.appCfg)
	ps, err := postgres.New(
		target.PG.URL, target.PG.Schema, b.database,
		target.PG.MachineName, target.PG.AllowInsecure,
		target.syncOptions(projects, excludeProjects),
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
	result, err := ps.Push(ctx, forceFull,
		func(p postgres.PushProgress) {
			printPGPushProgress(p)
		},
	)
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
	didResync := runLocalSync(ctx, b.appCfg, b.database, cfg.Full)
	if err := ctx.Err(); err != nil {
		return duckdbsync.PushResult{}, err
	}
	forceFull := cfg.Full || didResync

	fmt.Println("Opening DuckDB mirror...")
	connectStart := time.Now()
	syncer, err := duckdbsync.New(
		duckCfg.Path, b.database, duckCfg.MachineName,
		duckdbsync.SyncOptions{
			Projects:        projects,
			ExcludeProjects: excludeProjects,
		},
	)
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
				"\rPushing... %d/%d sessions, %d messages",
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
		Machine:                 "local",
		BlockedResultCategories: b.appCfg.ResultContentBlockedCategories,
	})

	didResync, err := runPGWatchStartupSync(ctx, engine, cfg.Full)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}

	pusher := &pgPusher{
		localSync: func(c context.Context) error {
			engine.SyncAll(c, nil)
			return nil
		},
		connect: func() (pgTarget, error) {
			applyClassifierConfig(b.appCfg)
			s, cErr := postgres.New(
				target.PG.URL, target.PG.Schema, b.database,
				target.PG.MachineName, target.PG.AllowInsecure,
				target.syncOptions(projects, exclude),
			)
			if cErr != nil {
				return nil, cErr
			}
			return s, nil
		},
	}
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
		func(paths []string) {
			engine.SyncPaths(paths)
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
