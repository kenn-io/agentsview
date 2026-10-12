package main

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/agentsview/internal/codexcloud"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/poller"
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
		Store:     codexcloud.NewStore(filepath.Join(cfg.DataDir, "sources", "codex-cloud")),
		Waits:     codexcloud.NewWaits(time.Now),
		Status:    status,
		Now:       time.Now,
		Dial:      codexcloud.Dial,
		Gates:     codexcloud.NewGateSource(filepath.Join(cfg.DataDir, "config.toml"), load, os.Getenv, userHome),
		NoSync:    cfg.NoSync,
		ReadLogin: codexcloud.ReadLogin,
		Version:   version,
		Log:       log.Default(),
	}
}

func codexCloudSyncJob(sync *codexcloud.Syncer) poller.Job {
	return poller.Job{
		Name:       codexCloudSyncJobName,
		Interval:   codexCloudSyncInterval,
		Jitter:     codexCloudSyncJitter,
		RunAtStart: true,
		Run:        sync.Run,
	}
}
