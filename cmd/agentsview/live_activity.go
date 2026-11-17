package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func collectLiveActivityTargets(
	ctx context.Context,
	cfg config.Config,
) ([]agentsync.LiveActivityTarget, error) {
	var targets []agentsync.LiveActivityTarget
	var targetErrors []error
	for _, factory := range cfg.LocalProviderFactories() {
		roots := cfg.ResolveDirs(factory.Definition().Type)
		if len(roots) == 0 {
			continue
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots:   roots,
			Machine: cfg.InstallationID,
		})
		hints, supported, err := parser.ResolveActivityHintProvider(provider)
		if err != nil {
			targetErrors = append(targetErrors, err)
			continue
		}
		if !supported || hints == nil {
			continue
		}
		sources, err := hints.ActivityHintSources(ctx)
		if err != nil {
			targetErrors = append(targetErrors, fmt.Errorf(
				"%s activity hint sources: %w",
				factory.Definition().Type, err,
			))
			continue
		}
		if len(sources) == 0 {
			continue
		}
		targets = append(targets, agentsync.LiveActivityTarget{
			Provider: provider,
			Hints:    hints,
			Sources:  sources,
		})
	}
	return targets, errors.Join(targetErrors...)
}

func trackLiveActivitySync(
	idleTracker *server.IdleTracker,
	syncPaths agentsync.LiveActivitySync,
) agentsync.LiveActivitySync {
	return func(ctx context.Context, paths []string) error {
		done, ok := idleTracker.BeginWork()
		if !ok {
			return context.Canceled
		}
		defer done()
		return syncPaths(ctx, paths)
	}
}

func startLiveActivityRun(
	ctx context.Context,
	cancel context.CancelFunc,
	run func(context.Context),
) func() {
	var workers sync.WaitGroup
	workers.Go(func() {
		run(ctx)
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			workers.Wait()
		})
	}
}

// liveActivity holds the controls of the running live-activity poller.
type liveActivity struct {
	stop func()
	// pollNow runs one poll and waits for it. Before target discovery
	// finishes there is no poller to ask, and its first poll follows
	// discovery anyway.
	pollNow func(context.Context)
}

func startLiveActivityPoller(
	ctx context.Context,
	cfg config.Config,
	database *db.DB,
	engine *agentsync.Engine,
	idleTracker *server.IdleTracker,
	gap func() time.Duration,
) *liveActivity {
	runCtx, cancel := context.WithCancel(ctx)
	var running atomic.Pointer[agentsync.LiveActivityPoller]
	stop := startLiveActivityRun(runCtx, cancel, func(ctx context.Context) {
		targets, err := collectLiveActivityTargets(ctx, cfg)
		if err != nil {
			log.Printf("live activity target discovery: %v", err)
		}
		poller := agentsync.NewLiveActivityPoller(
			targets,
			agentsync.DBLiveActivityLookup(database),
			trackLiveActivitySync(idleTracker, engine.SyncPathsContext),
			log.Printf,
		)
		poller.SetRecentLookup(
			agentsync.DBRecentSessionLookup(
				database, cfg.InstallationID, cfg.SourceMachines,
			),
		)
		poller.SetGap(gap)
		running.Store(poller)
		poller.Run(ctx)
	})
	return &liveActivity{
		stop: stop,
		pollNow: func(ctx context.Context) {
			if poller := running.Load(); poller != nil {
				poller.PollNow(ctx)
			}
		},
	}
}
