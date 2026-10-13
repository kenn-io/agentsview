package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/agentsview/internal/codexcloud"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/poller"
	"go.kenn.io/agentsview/internal/server"
)

const (
	codexCloudSyncJobName  = "codex-cloud-sync"
	codexCloudSyncInterval = 5 * time.Minute
	codexCloudSyncJitter   = 30 * time.Second
)

// newCodexCloudSyncer builds the thread syncer. load re-reads config.toml
// before each run, so a hand edit to [agents.codex-cloud], disabled_agents,
// or [agents.codex] homes takes effect without a restart. cfg.NoSync is
// fixed at start: it is set by --no-sync and by an archive-only archive.
func newCodexCloudSyncer(
	cfg config.Config, load func() (config.Config, error), status *codexcloud.StatusBoard,
) *codexcloud.Syncer {
	userHome, err := os.UserHomeDir()
	if err != nil {
		// DiscoverHomes then falls back to CODEX_HOME and configured homes.
		log.Printf("codex-cloud: home directory: %v", err)
	}
	return &codexcloud.Syncer{
		Store:  codexcloud.NewStore(filepath.Join(cfg.DataDir, "sources", "codex-cloud")),
		Waits:  codexcloud.NewWaits(time.Now),
		Status: status,
		Now:    time.Now,
		Dial:   codexcloud.Dial,
		Gates: codexcloud.NewGateSource(
			filepath.Join(cfg.DataDir, "config.toml"), cfg, load, os.Getenv, userHome,
		),
		NoSync:    cfg.NoSync,
		ReadLogin: codexcloud.ReadLogin,
		Version:   version,
		Log:       log.Default(),
	}
}

// codexCloudSyncJob registers each pass as tracked work, so an idle
// daemon waits for a running import instead of cutting it off.
func codexCloudSyncJob(sync *codexcloud.Syncer, idle *server.IdleTracker) poller.Job {
	return poller.Job{
		Name:       codexCloudSyncJobName,
		Interval:   codexCloudSyncInterval,
		Jitter:     codexCloudSyncJitter,
		RunAtStart: true,
		Run: func(ctx context.Context) error {
			done, ok := idle.BeginWork()
			if !ok {
				return nil
			}
			defer done()
			return sync.Run(ctx)
		},
	}
}
