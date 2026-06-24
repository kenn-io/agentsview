package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/kit/daemon"
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
		logPGWatchPushResult(res, reason)
		log.Printf(
			"pg watch: %d session(s) failed to push; will retry",
			res.Errors,
		)
		return nil
	}
	logPGWatchPushResult(res, reason)
	return nil
}

func logPGWatchPushResult(res postgres.PushResult, reason pushReason) {
	if res.SkippedConflicts > 0 {
		log.Printf(
			"pg watch: pushed %d sessions, %d messages, skipped %d ownership conflict(s), %d errors (%s)",
			res.SessionsPushed, res.MessagesPushed,
			res.SkippedConflicts, res.Errors, reason,
		)
		log.Printf(
			"pg watch: %d session(s) skipped due to PostgreSQL ownership conflicts",
			res.SkippedConflicts,
		)
		return
	}
	if res.Errors > 0 {
		log.Printf(
			"pg watch: pushed %d sessions, %d messages, %d errors (%s)",
			res.SessionsPushed, res.MessagesPushed,
			res.Errors, reason,
		)
		return
	}
	log.Printf(
		"pg watch: pushed %d sessions, %d messages (%s)",
		res.SessionsPushed, res.MessagesPushed, reason,
	)
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
	lockPath, err := (daemon.RuntimeStore{
		Dir:    appCfg.DataDir,
		Prefix: "pg-watch",
	}).LockPath()
	if err != nil {
		fatal("pg push --watch: %v", err)
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		fatal("pg push --watch: locking %s: %v", lockPath, err)
	}
	if !locked {
		fatal("pg push --watch: already locked (%s)", lockPath)
	}
	defer func() {
		if rerr := lock.Unlock(); rerr != nil {
			log.Printf("pg watch: releasing lock: %v", rerr)
		}
	}()

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	log.Printf(
		"pg watch: starting (machine=%q debounce=%s interval=%s)",
		pgCfg.MachineName, debounce, interval,
	)

	backend, cleanup, err := resolveArchiveWriteBackend(ctx, appCfg)
	if err != nil {
		fatal("opening writer: %v", err)
	}
	defer cleanup()
	if err := backend.PGPushWatch(
		ctx, pgCfg, cfg, projects, exclude, debounce, interval,
	); err != nil {
		fatal("pg push --watch: %v", err)
	}
}
