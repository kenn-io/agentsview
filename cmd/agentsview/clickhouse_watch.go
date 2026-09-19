package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/gofrs/flock"
	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/config"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/kit/daemon"
)

// clickHousePusher runs a local engine sync then pushes to ClickHouse.
type clickHousePusher struct {
	localSync  func(context.Context) error
	scopedSync func(
		context.Context, syncpkg.WatchBatch, *syncpkg.WatchRecoveryScope,
		func() error,
	) error
	ensurePricing func(context.Context) error
	mirrorPush    func(context.Context, bool) (clickhouse.PushResult, error)
}

func (p *clickHousePusher) push(
	ctx context.Context, reason pushReason, full bool,
) error {
	return p.pushBatch(ctx, reason, full, nil, nil)
}

func (p *clickHousePusher) pushBatch(
	ctx context.Context,
	reason pushReason,
	full bool,
	batch *syncpkg.WatchBatch,
	recovery *syncpkg.WatchRecoveryScope,
) error {
	push := func() error { return p.pushAfterSync(ctx, reason, full) }
	if batch != nil {
		if p.scopedSync == nil {
			return errors.New("scoped local sync is unavailable")
		}
		return p.scopedSync(ctx, *batch, recovery, push)
	}
	if err := p.localSync(ctx); err != nil {
		return fmt.Errorf("local sync: %w", err)
	}
	return push()
}

func (p *clickHousePusher) pushAfterSync(
	ctx context.Context, reason pushReason, full bool,
) error {
	if p.ensurePricing != nil {
		if err := p.ensurePricing(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			log.Printf("warning: pricing refresh failed: %v", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	res, err := p.mirrorPush(ctx, full)
	if err != nil {
		return err
	}
	return completeClickHouseWatchPush(res, reason)
}

func completeClickHouseWatchPush(res clickhouse.PushResult, reason pushReason) error {
	if res.Errors > 0 {
		log.Printf(
			"clickhouse watch: pushed %d sessions, %d messages, %d errors (%s)",
			res.SessionsPushed, res.MessagesPushed, res.Errors, reason,
		)
		return fmt.Errorf("%d session(s) failed", res.Errors)
	}
	log.Printf(
		"clickhouse watch: pushed %d sessions, %d messages (%s)",
		res.SessionsPushed, res.MessagesPushed, reason,
	)
	return nil
}

func resolveClickHouseWatchTargets(
	appCfg config.Config, cfg ClickHousePushConfig, targetName string,
) (clickHouseTargetSelection, []string, []string, error) {
	targets, err := resolveClickHouseTargetSelections(appCfg, targetName, false)
	if err != nil {
		return clickHouseTargetSelection{}, nil, nil, err
	}
	target, err := resolveClickHouseTargetConfig(appCfg, targets[0])
	if err != nil {
		return clickHouseTargetSelection{}, nil, nil, err
	}
	if target.Config.URL == "" {
		return clickHouseTargetSelection{}, nil, nil, fmt.Errorf("url not configured")
	}
	if err := clickhouse.CheckTransportSecurity(
		target.Config.URL, target.Config.AllowInsecure,
	); err != nil {
		return clickHouseTargetSelection{}, nil, nil, err
	}
	projects, exclude, err := resolveClickHousePushProjects(target.Config, cfg)
	if err != nil {
		return clickHouseTargetSelection{}, nil, nil, err
	}
	return target, projects, exclude, nil
}

func runClickHousePushWatch(cfg ClickHousePushConfig, targetName string) error {
	appCfg, err := config.LoadMinimal()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	if err := os.MkdirAll(appCfg.DataDir, 0o755); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	setupLogFileNamed(appCfg.DataDir, "clickhouse-watch.log")

	target, projects, exclude, err := resolveClickHouseWatchTargets(appCfg, cfg, targetName)
	if err != nil {
		return err
	}

	debounce := cfg.Debounce
	if debounce <= 0 {
		debounce = defaultWatchDebounce
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultWatchInterval
	}

	lockPath, err := (daemon.RuntimeStore{
		Dir:    appCfg.DataDir,
		Prefix: "clickhouse-watch",
	}).LockPath()
	if err != nil {
		return err
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return fmt.Errorf("locking %s: %w", lockPath, err)
	}
	if !locked {
		return fmt.Errorf("already locked (%s)", lockPath)
	}
	defer func() {
		if rerr := lock.Unlock(); rerr != nil {
			log.Printf("clickhouse watch: releasing lock: %v", rerr)
		}
	}()

	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()

	log.Printf(
		"clickhouse watch: starting (machine=%q debounce=%s interval=%s)",
		target.Config.MachineName, debounce, interval,
	)

	backend, cleanup, err := resolveArchiveWriteBackend(ctx, appCfg)
	if err != nil {
		return fmt.Errorf("opening writer: %w", err)
	}
	defer cleanup()
	return backend.ClickHousePushWatch(
		ctx, target, cfg, projects, exclude, debounce, interval,
	)
}
