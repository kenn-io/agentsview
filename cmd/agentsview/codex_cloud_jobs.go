package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
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
	codexCloudLiveJobName  = "codex-cloud-live"
	codexCloudLiveInterval = 30 * time.Second
	codexCloudLiveJitter   = 5 * time.Second
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

// newCodexCloudMonitor builds the live token monitor. It shares the
// syncer's state store, whose per-account lock keeps the two jobs' writes
// apart, and its waits, which both jobs honor. trigger queues a sync run
// after a joined turn completes. Each joined thread holds an idle work
// lease until it leaves, so an idle daemon does not exit mid-capture.
func newCodexCloudMonitor(
	sync *codexcloud.Syncer, trigger *codexCloudTrigger, idle *server.IdleTracker,
) *codexcloud.Monitor {
	return &codexcloud.Monitor{
		Store:     sync.Store,
		Waits:     sync.Waits,
		Status:    sync.Status,
		Now:       sync.Now,
		Dial:      sync.Dial,
		Gates:     sync.Gates,
		NoSync:    sync.NoSync,
		ReadLogin: sync.ReadLogin,
		Trigger:   trigger.Trigger,
		BeginWork: idle.BeginWork,
		Version:   sync.Version,
		Log:       sync.Log,
	}
}

// codexCloudLiveJob finds running threads for the monitor. Joined threads
// stay open after a run returns; the monitor owns them and their work
// leases.
func codexCloudLiveJob(m *codexcloud.Monitor, idle *server.IdleTracker) poller.Job {
	return poller.Job{
		Name:       codexCloudLiveJobName,
		Interval:   codexCloudLiveInterval,
		Jitter:     codexCloudLiveJitter,
		RunAtStart: true,
		Run: func(ctx context.Context) error {
			done, ok := idle.BeginWork()
			if !ok {
				return nil
			}
			defer done()
			return m.Discover(ctx)
		},
	}
}

// codexCloudTrigger queues a sync run on a scheduler set after the jobs
// start, since the monitor is built before the scheduler exists.
type codexCloudTrigger struct {
	scheduler atomic.Pointer[poller.Scheduler]
}

// Trigger queues one sync run. A run already queued covers this request,
// so several turns completing together add at most one run.
func (t *codexCloudTrigger) Trigger() error {
	s := t.scheduler.Load()
	if s == nil {
		return nil
	}
	err := s.TriggerNow(codexCloudSyncJobName)
	if errors.Is(err, poller.ErrTriggerPending) {
		return nil
	}
	return err
}
