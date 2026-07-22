package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/sync"
)

// TestForegroundResyncRunnerFallsBackInProcess verifies the resync seam: under a
// test binary the runner takes the in-process ResyncAll arm (the worker arm is
// gated by !testing.Testing()), rebuilds the archive, and clears the stale-data
// flag. The worker build-and-swap mechanism is covered by the engine split tests
// and the resync-build worker mode test.
func TestForegroundResyncRunnerFallsBackInProcess(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engine := sync.NewEngine(database, workerEngineConfig(cfg))
	t.Cleanup(engine.Close)
	require.Equal(t, 3, engine.SyncAll(context.Background(), nil).Synced)

	runner := newForegroundResyncRunner(context.Background(), cfg, engine, database)
	stats, err := runner(context.Background(), nil)

	require.NoError(t, err)
	assert.False(t, stats.Aborted)
	assert.Equal(t, 3, stats.Synced, "in-process resync fallback rebuilds the archive")
	assert.False(t, database.NeedsResync())
}

// requireStartupMaintenanceReleased asserts that RunStartupMaintenance is no
// longer gated: its work function must run promptly instead of blocking on the
// startup-maintenance gate until shutdown.
func requireStartupMaintenanceReleased(t *testing.T, engine *sync.Engine) {
	t.Helper()
	maintenanceRan := make(chan struct{})
	maintenanceDone := make(chan error, 1)
	go func() {
		maintenanceDone <- engine.RunStartupMaintenance(
			t.Context(), func() error {
				close(maintenanceRan)
				return nil
			},
		)
	}()
	select {
	case <-maintenanceRan:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "startup maintenance still gated after the pass")
	}
	require.NoError(t, <-maintenanceDone)
}

// TestForegroundResyncRunnerReleasesStartupMaintenance covers the
// skip-initial-sync daemon shape: when the in-process fallback drives startup
// reconciliation, it must also release the startup-maintenance gate.
// Otherwise the deferred startup fallback sees the closed reconciliation gate,
// returns early, and archive-wide backfills stay blocked until shutdown.
func TestForegroundResyncRunnerReleasesStartupMaintenance(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	engineCfg := workerEngineConfig(cfg)
	engineCfg.DeferStartupMaintenance = true
	engine := sync.NewEngine(database, engineCfg)
	t.Cleanup(engine.Close)

	runner := newForegroundResyncRunner(context.Background(), cfg, engine, database)
	_, err = runner(context.Background(), nil)

	require.NoError(t, err)
	require.True(t, engine.StartupReconciled(),
		"a successful in-process resync closes the reconciliation gate")
	requireStartupMaintenanceReleased(t, engine)
}

// TestForegroundResyncRunnerAbortedResyncFallsBackIncremental verifies the
// in-process fallback keeps SyncThenRun's abort semantics: a safely aborted
// resync catches up with an incremental pass instead of surfacing the bare
// abort, and startup still reconciles and releases maintenance.
func TestForegroundResyncRunnerAbortedResyncFallsBackIncremental(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	missingPath := filepath.Join(t.TempDir(), "missing.jsonl")
	dbtest.SeedSession(t, database, "existing", "proj", func(s *db.Session) {
		s.FilePath = &missingPath
	})
	engine := sync.NewEngine(database, sync.EngineConfig{
		Machine:                 "local",
		DeferStartupMaintenance: true,
	})
	t.Cleanup(engine.Close)

	runner := newForegroundResyncRunner(
		context.Background(), config.Config{}, engine, database,
	)
	stats, err := runner(context.Background(), nil)

	require.NoError(t, err)
	assert.False(t, stats.Aborted,
		"a safely aborted resync must fall back to an incremental sync")
	assert.True(t, engine.StartupReconciled(),
		"the incremental fallback pass reconciles startup")
	requireStartupMaintenanceReleased(t, engine)
}

// TestSyncAllReleasingStartupMaintenance covers the worker-aborted resync arm
// (unreachable under a test binary): a completed incremental catch-up must
// release the startup-maintenance gate, while a cancelled pass leaves the gate
// closed so the deferred startup fallback still owns recovery.
func TestSyncAllReleasingStartupMaintenance(t *testing.T) {
	t.Run("releases after completed pass", func(t *testing.T) {
		database := dbtest.OpenTestDB(t)
		engine := sync.NewEngine(database, sync.EngineConfig{
			Machine:                 "local",
			DeferStartupMaintenance: true,
		})
		t.Cleanup(engine.Close)

		syncAllReleasingStartupMaintenance(context.Background(), engine, nil)

		requireStartupMaintenanceReleased(t, engine)
	})
	t.Run("releases when cancelled after reconciliation", func(t *testing.T) {
		database := dbtest.OpenTestDB(t)
		engine := sync.NewEngine(database, sync.EngineConfig{
			Machine:                 "local",
			DeferStartupMaintenance: true,
		})
		t.Cleanup(engine.Close)
		// A completed pass closes the startup-reconciliation gate (SyncAll
		// records it internally but never releases maintenance) ...
		engine.SyncAll(context.Background(), nil)
		require.True(t, engine.StartupReconciled(),
			"a completed pass must close the reconciliation gate")

		// ... and cancellation can land before the helper's own ctx check.
		// The deferred startup fallback skips on the closed reconciliation
		// gate without releasing, so the helper must release here or
		// archive-wide backfills stay gated until shutdown.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		syncAllReleasingStartupMaintenance(ctx, engine, nil)

		requireStartupMaintenanceReleased(t, engine)
	})
	t.Run("keeps gate closed when cancelled", func(t *testing.T) {
		database := dbtest.OpenTestDB(t)
		engine := sync.NewEngine(database, sync.EngineConfig{
			Machine:                 "local",
			DeferStartupMaintenance: true,
		})
		t.Cleanup(engine.Close)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		syncAllReleasingStartupMaintenance(ctx, engine, nil)

		maintenanceErr := make(chan error, 1)
		blockedCtx, blockedCancel := context.WithTimeout(
			context.Background(), 100*time.Millisecond,
		)
		defer blockedCancel()
		go func() {
			maintenanceErr <- engine.RunStartupMaintenance(
				blockedCtx, func() error { return nil },
			)
		}()
		require.ErrorIs(t, <-maintenanceErr, context.DeadlineExceeded,
			"a cancelled pass must leave maintenance to the deferred fallback")
	})
}
