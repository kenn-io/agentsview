// ABOUTME: Tests the bounded recovery path for abandoned daemon-launch syncs.
// ABOUTME: Uses a scratch database and synthetic timeout without starting a daemon.
package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestRunDeferredStartupSyncFallbackPerformsSkippedSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := dbtest.OpenTestDB(t)
	reconciled := make(chan struct{}, 1)
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
		Machine:                 "local",
		DeferStartupMaintenance: true,
		OnStartupReconciled: func(syncpkg.SyncStats, error) {
			reconciled <- struct{}{}
		},
	})
	t.Cleanup(engine.Close)
	timeout := make(chan time.Time, 1)
	timeout <- time.Now()

	// Under a test binary the worker path is skipped (testing.Testing()), so
	// the fallback runs the in-process sync; database/lock stay unused.
	ran, err := runDeferredStartupSyncFallback(
		t.Context(), config.Config{}, engine, database, nil, nil, timeout,
	)

	require.NoError(err)
	assert.True(ran)
	assert.False(engine.LastSyncStartedAt().IsZero(),
		"timeout fallback must perform the skipped local sync")
	select {
	case <-reconciled:
	case <-time.After(time.Second):
		require.FailNow("deferred fallback did not reconcile watcher startup")
	}
}

// TestRunDeferredStartupSyncFallbackSkipsWhenAlreadyReconciled proves the gate
// that avoids a redundant pass: once a foreground request has driven startup
// reconciliation, the deferred fallback performs no sync.
func TestRunDeferredStartupSyncFallbackSkipsWhenAlreadyReconciled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := dbtest.OpenTestDB(t)
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
		Machine:                 "local",
		DeferStartupMaintenance: true,
	})
	t.Cleanup(engine.Close)
	engine.RecordStartupReconciled(syncpkg.SyncStats{}, nil)
	require.True(engine.StartupReconciled())

	timeout := make(chan time.Time, 1)
	timeout <- time.Now()
	ran, err := runDeferredStartupSyncFallback(
		t.Context(), config.Config{}, engine, database, nil, nil, timeout,
	)

	require.NoError(err)
	assert.False(ran, "already-reconciled startup must skip the deferred sync")
	assert.True(engine.LastSyncStartedAt().IsZero(),
		"no redundant local sync should run")
}

// TestForegroundSyncRunnerReconcilesStartup covers Finding 2: the daemon's
// foreground local-sync runner must reconcile startup so an `agentsview sync`
// that drives the sync opens watcher dispatch. Under a test binary the runner
// takes the in-process SyncThenRun arm, which reconciles natively; production's
// worker arm mirrors it with RecordStartupReconciled.
func TestForegroundSyncRunnerReconcilesStartup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg := testConfigWithClaudeFixture(t)
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	opened := make(chan struct{}, 1)
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
		AgentDirs:               cfg.AgentDirs,
		Machine:                 "local",
		DeferStartupMaintenance: true,
		OnStartupReconciled: newStartupReconciliationHandler(
			t.Context(),
			func(context.Context) error { return nil },
			func() {
				select {
				case opened <- struct{}{}:
				default:
				}
			},
		),
	})
	t.Cleanup(engine.Close)

	runner := newForegroundSyncRunner(t.Context(), cfg, engine, database, nil)
	stats, err := runner(t.Context(), nil)

	require.NoError(err)
	assert.Equal(3, stats.Synced, "foreground runner syncs the fixture archive")
	assert.True(engine.StartupReconciled(),
		"foreground sync must reconcile startup")
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		require.FailNow("foreground runner did not open watcher dispatch")
	}
}
