package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/pidlock"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/sync"
)

// pgTarget is the subset of *postgres.Sync the pusher needs. It is an
// interface so the pusher can be tested without a live database.
type pgTarget interface {
	EnsureSchema(ctx context.Context) error
	Push(
		ctx context.Context, full bool,
		onProgress func(postgres.PushProgress),
	) (postgres.PushResult, error)
	Close() error
}

// pgPusher runs a local sync then pushes to PostgreSQL, lazily
// connecting and reconnecting after errors so a transiently
// unreachable database never crashes the daemon.
type pgPusher struct {
	localSync func(context.Context) error
	connect   func() (pgTarget, error)
	target    pgTarget
}

// push performs one local-sync-then-push cycle. On any PG error it
// drops the cached connection so the next call reconnects.
func (p *pgPusher) push(
	ctx context.Context, reason pushReason, full bool,
) error {
	if err := p.localSync(ctx); err != nil {
		return fmt.Errorf("local sync: %w", err)
	}
	if p.target == nil {
		t, err := p.connect()
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		p.target = t
	}
	// EnsureSchema is idempotent and memoized inside *postgres.Sync,
	// so calling it every cycle is cheap after the first success.
	if err := p.target.EnsureSchema(ctx); err != nil {
		p.reset()
		return fmt.Errorf("ensure schema: %w", err)
	}
	res, err := p.target.Push(ctx, full, nil)
	if err != nil {
		p.reset()
		return fmt.Errorf("push: %w", err)
	}
	if res.Errors > 0 {
		log.Printf(
			"pg watch: pushed %d sessions, %d messages, %d errors (%s)",
			res.SessionsPushed, res.MessagesPushed, res.Errors, reason,
		)
		log.Printf(
			"pg watch: %d session(s) failed to push; will retry",
			res.Errors,
		)
		return nil
	}
	log.Printf(
		"pg watch: pushed %d sessions, %d messages (%s)",
		res.SessionsPushed, res.MessagesPushed, reason,
	)
	return nil
}

func (p *pgPusher) reset() {
	if p.target != nil {
		_ = p.target.Close()
		p.target = nil
	}
}

// resolveWatchTargets validates PG config and resolves the project
// filters for a watch run.
func resolveWatchTargets(
	appCfg config.Config, cfg PGPushConfig,
) (pgCfg config.PGConfig, projects, exclude []string, err error) {
	pgCfg, err = appCfg.ResolvePG()
	if err != nil {
		return config.PGConfig{}, nil, nil, err
	}
	if pgCfg.URL == "" {
		return config.PGConfig{}, nil, nil,
			fmt.Errorf("url not configured")
	}
	projects, exclude, err = resolvePushProjects(pgCfg, cfg)
	if err != nil {
		return config.PGConfig{}, nil, nil, err
	}
	return pgCfg, projects, exclude, nil
}

const (
	defaultWatchDebounce = 30 * time.Second
	defaultWatchInterval = 15 * time.Minute
)

// runPGPushWatch runs the long-lived auto-push daemon: an initial
// catch-up push, then pushes triggered by file changes (debounced)
// and a periodic floor tick, until interrupted.
func runPGPushWatch(cfg PGPushConfig) {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		log.Fatalf("loading config: %v", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		log.Fatalf("creating data dir: %v", err)
	}
	setupLogFileNamed(appCfg.DataDir, "pg-watch.log")

	pgCfg, projects, exclude, err := resolveWatchTargets(appCfg, cfg)
	if err != nil {
		fatal("pg push --watch: %v", err)
	}

	debounce := cfg.Debounce
	if debounce <= 0 {
		debounce = defaultWatchDebounce
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultWatchInterval
	}

	// Single-instance guard: only one watcher per data dir.
	lockPath := filepath.Join(appCfg.DataDir, "pg-watch.lock")
	lock, err := pidlock.Acquire(lockPath)
	if err != nil {
		fatal("pg push --watch: %v", err)
	}
	defer func() {
		if rerr := lock.Release(); rerr != nil {
			log.Printf("pg watch: releasing lock: %v", rerr)
		}
	}()

	applyClassifierConfig(appCfg)
	database := mustOpenDB(appCfg)
	defer database.Close()

	for _, def := range parser.Registry {
		if !appCfg.IsUserConfigured(def.Type) {
			continue
		}
		warnMissingDirs(appCfg.ResolveDirs(def.Type), string(def.Type))
	}
	cleanResyncTemp(appCfg.DBPath)

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs:               appCfg.AgentDirs,
		Machine:                 "local",
		BlockedResultCategories: appCfg.ResultContentBlockedCategories,
	})

	// Initial local sync. A data-version change forces a full push.
	didResync := cfg.Full || database.NeedsResync()
	if didResync {
		engine.ResyncAll(ctx, nil)
	} else {
		engine.SyncAll(ctx, nil)
	}
	if ctx.Err() != nil {
		return
	}

	pusher := &pgPusher{
		localSync: func(c context.Context) error {
			engine.SyncAll(c, nil)
			return nil
		},
		connect: func() (pgTarget, error) {
			// Repeated inside this closure (not only in the outer
			// body) because TestEveryStoreOpenPathIsWired scans each
			// function literal independently and requires a
			// classifier-wiring call in the same body as postgres.New.
			applyClassifierConfig(appCfg)
			s, cErr := postgres.New(
				pgCfg.URL, pgCfg.Schema, database,
				pgCfg.MachineName, pgCfg.AllowInsecure,
				postgres.SyncOptions{
					Projects:        projects,
					ExcludeProjects: exclude,
				},
			)
			if cErr != nil {
				return nil, cErr
			}
			return s, nil
		},
	}
	defer pusher.reset()

	log.Printf(
		"pg watch: starting (machine=%q debounce=%s interval=%s)",
		pgCfg.MachineName, debounce, interval,
	)
	fmt.Printf(
		"agentsview pg watch: pushing to PostgreSQL as %q "+
			"(debounce %s, floor %s)\n",
		pgCfg.MachineName, debounce, interval,
	)

	// Initial catch-up push (full if a resync just happened).
	if err := pusher.push(ctx, reasonStartup, didResync); err != nil {
		log.Printf("pg watch: initial push failed: %v", err)
	}

	loop, ticker := newPushLoop(debounce, interval,
		func(c context.Context, r pushReason) error {
			return pusher.push(c, r, false)
		},
	)
	defer ticker.Stop()

	stopWatcher, unwatchedDirs := startFileWatcher(appCfg, engine,
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
}
