// ABOUTME: Tests the bounded recovery path for abandoned daemon-launch syncs.
// ABOUTME: Uses a scratch database and synthetic timeout without starting a daemon.
package main

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestRunDeferredStartupSyncFallbackPerformsSkippedSync(t *testing.T) {
	for _, firstImport := range []bool{false, true} {
		t.Run(fmt.Sprintf("first-import-%t", firstImport), func(t *testing.T) {
			cfg := testConfigWithClaudeFixture(t)
			database := dbtest.OpenTestDBAt(t, cfg.DBPath)
			reconciled := make(chan struct{}, 1)
			emitter := &scopedEmitter{scopes: make(chan string, 100)}
			engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
				AgentDirs: cfg.AgentDirs, Machine: "local", Emitter: emitter,
				DeferStartupMaintenance: true,
				OnStartupReconciled:     func(syncpkg.SyncStats, error) { reconciled <- struct{}{} },
			})
			t.Cleanup(engine.Close)
			timeout := make(chan time.Time, 1)
			timeout <- time.Now()
			ran, err := runDeferredStartupSyncFallback(t.Context(), cfg, engine, database, nil, nil, timeout, firstImport)
			require.NoError(t, err)
			assert.True(t, ran)
			found := false
			for len(emitter.scopes) > 0 {
				if <-emitter.scopes == "sessions" {
					found = true
				}
			}
			assert.Equal(t, firstImport, found)
			assert.False(t, engine.LastSyncStartedAt(t.Context()).IsZero())
			select {
			case <-reconciled:
			case <-time.After(time.Second):
				require.FailNow(t, "deferred fallback did not reconcile watcher startup")
			}
		})
	}
}

// TestRunDeferredStartupSyncFallbackSkipsWhenAlreadyReconciled proves the gate
// that avoids a redundant pass: once a foreground request has driven startup
// reconciliation, the deferred fallback performs no sync.
func TestRunDeferredStartupSyncFallbackSkipsWhenAlreadyReconciled(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
		Machine:                 "local",
		DeferStartupMaintenance: true,
	})
	t.Cleanup(engine.Close)
	engine.RecordStartupReconciled(syncpkg.SyncStats{}, nil)
	require.True(t, engine.StartupReconciled())

	timeout := make(chan time.Time, 1)
	timeout <- time.Now()
	ran, err := runDeferredStartupSyncFallback(
		t.Context(), config.Config{}, engine, database, nil, nil, timeout, false,
	)

	require.NoError(t, err)
	assert.False(t, ran, "already-reconciled startup must skip the deferred sync")
	assert.True(t, engine.LastSyncStartedAt(t.Context()).IsZero(),
		"no redundant local sync should run")
}

// TestForegroundSyncRunnerReconcilesStartup covers Finding 2: the daemon's
// foreground local-sync runner must reconcile startup so an `agentsview sync`
// that drives the sync opens watcher dispatch. Under a test binary the runner
// takes the in-process SyncThenRun arm, which reconciles natively; production's
// worker arm mirrors it with RecordStartupReconciled.
func TestForegroundSyncRunnerReconcilesStartup(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	opened := make(chan struct{}, 1)
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
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

	require.NoError(t, err)
	assert.Equal(t, 3, stats.Synced, "foreground runner syncs the fixture archive")
	assert.True(t, engine.StartupReconciled(),
		"foreground sync must reconcile startup")
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "foreground runner did not open watcher dispatch")
	}
}

func TestFirstImportProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	cfg := config.Config{DBPath: path}
	first, err := isFirstImport(t.Context(), cfg)
	require.NoError(t, err)
	require.True(t, first)
	database := dbtest.OpenTestDBAt(t, path)
	first, err = isFirstImport(t.Context(), cfg)
	require.NoError(t, err)
	require.True(t, first)
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "saved", Agent: "claude", Machine: "local"}))
	require.NoError(t, database.SetSyncState(t.Context(), "first_import_pending", "1"))
	first, err = isFirstImport(t.Context(), cfg)
	require.NoError(t, err)
	require.True(t, first, "an interrupted first import resumes without blocking startup")
	require.NoError(t, database.DeleteSyncState(t.Context(), "first_import_pending"))
	first, err = isFirstImport(t.Context(), cfg)
	require.NoError(t, err)
	require.False(t, first)
}
