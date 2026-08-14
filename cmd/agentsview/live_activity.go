package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

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
			Machine: cfg.LocalMachineName,
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

func newLiveActivityLookup(database *db.DB) agentsync.LiveActivityLookup {
	return func(
		ctx context.Context,
		fullSessionID string,
	) (agentsync.LiveActivitySource, bool, error) {
		session, err := database.GetSessionFull(ctx, fullSessionID)
		if err != nil {
			return agentsync.LiveActivitySource{}, false, err
		}
		if session == nil || session.FilePath == nil || *session.FilePath == "" {
			return agentsync.LiveActivitySource{}, false, nil
		}
		source := agentsync.LiveActivitySource{Path: *session.FilePath}
		if session.FileSize != nil && session.FileMtime != nil {
			source.StoredSize = *session.FileSize
			source.StoredMTimeNS = *session.FileMtime
			source.HasStoredStat = true
		}
		if session.FileInode != nil && session.FileDevice != nil {
			source.StoredInode = *session.FileInode
			source.StoredDevice = *session.FileDevice
			source.HasStoredIdentity = true
		}
		return source, true, nil
	}
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
	poller *agentsync.LiveActivityPoller,
) func() {
	var workers sync.WaitGroup
	workers.Go(func() {
		poller.Run(ctx)
	})
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			workers.Wait()
		})
	}
}

func startLiveActivityPoller(
	ctx context.Context,
	cfg config.Config,
	database *db.DB,
	engine *agentsync.Engine,
	idleTracker *server.IdleTracker,
) func() {
	runCtx, cancel := context.WithCancel(ctx)
	targets, err := collectLiveActivityTargets(runCtx, cfg)
	if err != nil {
		log.Printf("live activity target discovery: %v", err)
	}
	poller := agentsync.NewLiveActivityPoller(
		targets,
		newLiveActivityLookup(database),
		trackLiveActivitySync(idleTracker, engine.SyncPathsContext),
		log.Printf,
	)
	return startLiveActivityRun(runCtx, cancel, poller)
}
