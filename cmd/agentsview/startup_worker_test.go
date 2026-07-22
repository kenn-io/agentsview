package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// engineWithDispatchHandler builds an engine over the fixture archive whose
// OnStartupReconciled is the real startup reconciliation handler, so opening
// dispatch (leaving collecting mode) is observable through openedDispatch.
func engineWithDispatchHandler(
	t *testing.T, cfg config.Config, openedDispatch chan<- struct{},
) *syncpkg.Engine {
	t.Helper()
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
		AgentDirs: cfg.AgentDirs,
		Machine:   "local",
		OnStartupReconciled: newStartupReconciliationHandler(
			t.Context(),
			func(context.Context) error { return nil },
			func() {
				select {
				case openedDispatch <- struct{}{}:
				default:
				}
			},
		),
	})
	t.Cleanup(engine.Close)
	return engine
}

// TestStartupWorkerPathOpensWatcherDispatch reproduces the daemon side of the
// worker handshake: after the worker's out-of-process pass, the daemon runs the
// gap reconciliation and acknowledges startup, which must fire
// OnStartupReconciled and open watcher dispatch.
func TestStartupWorkerPathOpensWatcherDispatch(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	opened := make(chan struct{}, 1)
	engine := engineWithDispatchHandler(t, cfg, opened)

	// Gap reconciliation over all watch roots (warm; here it also performs the
	// first sync since the fixture DB is empty), then acknowledge startup with
	// the worker's terminal result.
	gapErr := engine.ReconcileWatchRoots(t.Context(), reconcileRootPaths(cfg), true)
	require.NoError(t, gapErr)
	engine.RecordStartupReconciled(
		statsFromWorkerResult(workerResult{
			Status: "ok", Synced: 3, DiscoveryComplete: true,
		}),
		gapErr,
	)

	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "watcher dispatch did not open after worker handshake")
	}
}

// TestStartupWorkerPathDefersMaintenanceUntilReconciled pins the worker-path
// maintenance gate: with a completed worker pass the daemon defers the gap
// reconciliation until after the HTTP server starts, so archive-wide startup
// maintenance must not acquire the sync mutex before that reconciliation.
// The gate opens only through RecordStartupReconciled.
func TestStartupWorkerPathDefersMaintenanceUntilReconciled(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{
		AgentDirs: cfg.AgentDirs,
		Machine:   "local",
		DeferStartupMaintenance: deferStartupMaintenance(
			false, true, // serve path: initial sync not skipped, worker ran
		),
	})
	t.Cleanup(engine.Close)

	maintenanceRan := make(chan struct{})
	go func() {
		_ = engine.RunStartupMaintenance(t.Context(), func() error {
			close(maintenanceRan)
			return nil
		})
	}()

	select {
	case <-maintenanceRan:
		require.FailNow(t,
			"startup maintenance must wait for the deferred gap reconciliation")
	case <-time.After(150 * time.Millisecond):
	}

	gapErr := engine.ReconcileWatchRoots(t.Context(), reconcileRootPaths(cfg), true)
	require.NoError(t, gapErr)
	engine.RecordStartupReconciled(
		statsFromWorkerResult(workerResult{
			Status: "ok", Synced: 3, DiscoveryComplete: true,
		}),
		gapErr,
	)

	select {
	case <-maintenanceRan:
	case <-time.After(2 * time.Second):
		require.FailNow(t,
			"maintenance never released after RecordStartupReconciled")
	}
}

// TestStartupWorkerFailureFallsBackInProcess asserts that a failed worker launch
// is surfaced (so runServe falls back), and the in-process initial sync still
// reconciles startup and opens dispatch.
func TestStartupWorkerFailureFallsBackInProcess(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)

	restore := stubLaunchSyncWorker(t, func(
		context.Context, config.Config, string, func(workerLine),
	) (workerResult, error) {
		return workerResult{}, errors.New("spawn boom")
	})
	defer restore()

	_, err := runStartupSyncViaWorker(
		t.Context(), cfg, newStartupStateWriter(cfg.DataDir, time.Now),
	)
	require.Error(t, err, "worker failure must be surfaced so the daemon falls back")

	opened := make(chan struct{}, 1)
	engine := engineWithDispatchHandler(t, cfg, opened)
	stats := runInitialSync(t.Context(), engine, nil)
	assert.False(t, stats.Aborted)
	assert.Equal(t, 3, stats.Synced, "in-process fallback syncs the fixture archive")
	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "in-process fallback did not open dispatch")
	}
}

func TestStartupWorkerPublishesEnrichedResyncProgress(t *testing.T) {
	cfg := config.Config{DataDir: t.TempDir()}
	now, step := fakeClock(time.Date(2026, 7, 22, 22, 0, 0, 0, time.UTC))
	progress := newStartupStateWriter(cfg.DataDir, now)
	progress.SetPhase("initial sync")

	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, mode string, onLine func(workerLine),
	) (workerResult, error) {
		assert.Equal(t, "startup", mode)
		onLine(workerLine{Progress: &syncpkg.Progress{
			Phase:  syncpkg.PhasePreparingResync,
			Detail: "Preparing full resync",
			Resync: true,
		}})
		state := readStartupState(cfg.DataDir)
		require.NotNil(t, state)
		assert.Equal(t, "full resync", state.Phase)
		assert.Equal(t, "Preparing full resync", state.Detail)

		step(startupDetailThrottle)
		onLine(workerLine{Progress: &syncpkg.Progress{
			Phase:           syncpkg.PhaseSyncing,
			Detail:          "Syncing sessions into rebuilt database",
			Resync:          true,
			SessionsDone:    25,
			SessionsTotal:   100,
			MessagesIndexed: 800,
		}})
		state = readStartupState(cfg.DataDir)
		require.NotNil(t, state)
		assert.Equal(t, "full resync", state.Phase)
		assert.Equal(t,
			"Syncing sessions into rebuilt database: 25/100 sessions (25%) · 800 messages",
			state.Detail,
		)

		stats := syncpkg.SyncStats{}
		return workerResult{
			Status: "ok", DiscoveryComplete: true, Stats: &stats,
		}, nil
	})
	defer restore()

	_, err := runStartupSyncViaWorker(t.Context(), cfg, progress)
	require.NoError(t, err)
}

// TestStartupWorkerOutcomeDiscriminatesSpawnFromRanFailed pins the Finding 1
// contract: a spawn failure asks the caller to fall back in process, while a
// worker that ran and reported failure is carried forward as an aborted result
// so the caller surfaces it without re-syncing.
func TestStartupWorkerOutcomeDiscriminatesSpawnFromRanFailed(t *testing.T) {
	ok := workerResult{Status: "ok", Synced: 3, DiscoveryComplete: true}

	tests := []struct {
		name        string
		result      workerResult
		err         error
		wantDone    bool
		wantAborted bool
		wantSynced  int
	}{
		{
			name:       "completed",
			result:     ok,
			err:        nil,
			wantDone:   true,
			wantSynced: 3,
		},
		{
			name:     "spawn failure falls back in process",
			result:   workerResult{},
			err:      fmt.Errorf("%w: exec: boom", errWorkerSpawn),
			wantDone: false,
		},
		{
			name: "ran and failed carries aborted result",
			result: workerResult{
				Status: "aborted", Synced: 1, DiscoveryComplete: false,
			},
			err:         errors.New("startup worker pass reported aborted"),
			wantDone:    true,
			wantAborted: true,
			wantSynced:  1,
		},
		{
			name:        "ran and failed with unusable result is synthesized",
			result:      workerResult{},
			err:         errors.New("startup worker emitted 0 terminal results"),
			wantDone:    true,
			wantAborted: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, done := startupWorkerOutcome(tc.result, tc.err)
			assert.Equal(t, tc.wantDone, done)
			if !done {
				// A spawn failure discards the result and falls back in process.
				return
			}
			stats := statsFromWorkerResult(got)
			assert.Equal(t, tc.wantAborted, stats.Aborted)
			assert.Equal(t, tc.wantSynced, stats.Synced)
		})
	}
}

// TestStartupWorkerRanFailedSurfacedWithoutResync exercises the Finding 1 daemon
// path end to end: the worker ran and returned a valid terminal result with a
// non-spawn error. The daemon must NOT fall back to the in-process initial sync
// (done stays true), and the gap reconciliation plus RecordStartupReconciled
// still open watcher dispatch, carrying the worker's aborted stats.
func TestStartupWorkerRanFailedSurfacedWithoutResync(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)

	restore := stubLaunchSyncWorker(t, func(
		context.Context, config.Config, string, func(workerLine),
	) (workerResult, error) {
		return workerResult{Status: "aborted", DiscoveryComplete: false},
			errors.New("startup worker pass reported aborted")
	})
	defer restore()

	result, syncErr := runStartupSyncViaWorker(
		t.Context(), cfg, newStartupStateWriter(cfg.DataDir, time.Now),
	)
	require.Error(t, syncErr)
	require.False(t, errors.Is(syncErr, errWorkerSpawn),
		"ran-and-failed must be distinct from spawn failure")

	carried, done := startupWorkerOutcome(result, syncErr)
	require.True(t, done, "daemon must not re-run the in-process initial sync")
	stats := statsFromWorkerResult(carried)
	require.True(t, stats.Aborted, "carried stats must reflect the aborted pass")

	opened := make(chan struct{}, 1)
	engine := engineWithDispatchHandler(t, cfg, opened)
	gapErr := engine.ReconcileWatchRoots(t.Context(), reconcileRootPaths(cfg), true)
	engine.RecordStartupReconciled(stats, gapErr)

	select {
	case <-opened:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "dispatch did not open after ran-and-failed handshake")
	}
}

func TestStatsFromWorkerResultMapsDiscoveryOntoAborted(t *testing.T) {
	complete := statsFromWorkerResult(workerResult{
		Status: "ok", Synced: 5, Skipped: 1, Failed: 0, DiscoveryComplete: true,
	})
	assert.False(t, complete.Aborted)
	assert.True(t, complete.AuthoritativeDiscoveryComplete())
	assert.Equal(t, 5, complete.Synced)

	incomplete := statsFromWorkerResult(workerResult{
		Status: "aborted", DiscoveryComplete: false,
	})
	assert.True(t, incomplete.Aborted)
	assert.False(t, incomplete.AuthoritativeDiscoveryComplete())
}

func TestSyncWorkerChildArgsForwardsServeConfigFlags(t *testing.T) {
	parent := []string{
		"serve", "--host", "0.0.0.0", "--port", "9999",
		"--background", "--pprof",
	}
	args := syncWorkerChildArgs(parent, "startup")

	require.Equal(t, "sync-worker", args[0])
	assert.Equal(t, []string{"--mode", "startup"}, args[1:3])
	assert.Contains(t, args, "--host=0.0.0.0", "serve config flag forwarded")
	assert.Contains(t, args, "--port=9999", "serve config flag forwarded")
	for _, a := range args {
		assert.NotContains(t, a, "background",
			"serve-only lifecycle flag must not reach the worker")
		assert.NotContains(t, a, "pprof",
			"serve-only lifecycle flag must not reach the worker")
	}
}

// TestSyncWorkerRealSpawnEmitsTerminalResult self-execs the built command in
// worker mode against an isolated fixture archive and validates the on-the-wire
// protocol: NDJSON lines, exactly one terminal result, exit 0.
func TestSyncWorkerRealSpawnEmitsTerminalResult(t *testing.T) {
	if testing.Short() {
		t.Skip("real-spawn worker test re-execs the binary; skipped in -short")
	}
	cfg := testConfigWithClaudeFixture(t)
	claudeDir := cfg.AgentDirs[parser.AgentClaude][0]

	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestSyncWorkerMainHelperProcess$",
		"--",
		"sync-worker", "--mode", "startup",
	)
	// Minimal, isolated env so no developer/CI agent directory is scanned.
	// os.UserHomeDir reads HOME on Unix but USERPROFILE on Windows, so the
	// isolated home must be exported under both names.
	home := t.TempDir()
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"USERPROFILE=" + home,
		"AGENTSVIEW_SYNC_WORKER_MAIN_HELPER=1",
		"AGENTSVIEW_DATA_DIR=" + cfg.DataDir,
		"CLAUDE_PROJECTS_DIR=" + claudeDir,
	}
	// The Go runtime and SQLite need core system variables on Windows;
	// passing them through keeps the env minimal without breaking the child.
	for _, key := range []string{"SYSTEMROOT", "TEMP", "TMP"} {
		if value := os.Getenv(key); value != "" {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Run(),
		"worker must exit 0 on an authoritative pass; stderr:\n%s", stderr.String())

	var results []workerResult
	sawProgress := false
	sc := bufio.NewScanner(&stdout)
	for sc.Scan() {
		var line workerLine
		require.NoError(t, json.Unmarshal(sc.Bytes(), &line),
			"every stdout line must be a workerLine JSON object; got %q", sc.Text())
		if line.Progress != nil {
			sawProgress = true
		}
		if line.Result != nil {
			results = append(results, *line.Result)
		}
	}
	require.NoError(t, sc.Err())
	assert.True(t, sawProgress, "worker must stream progress")
	require.Len(t, results, 1, "exactly one terminal result")
	assert.Equal(t, "ok", results[0].Status)
	assert.True(t, results[0].DiscoveryComplete)
	assert.Equal(t, 3, results[0].Synced)
}

// TestSyncWorkerMainHelperProcess is the re-exec target for the real-spawn test.
// It runs the actual CLI (via main) so os.Args[0] behaves like the agentsview
// binary, then exits 0 to suppress the test framework's own stdout output.
func TestSyncWorkerMainHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_SYNC_WORKER_MAIN_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"agentsview"}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	t.Fatal("missing helper args")
}
