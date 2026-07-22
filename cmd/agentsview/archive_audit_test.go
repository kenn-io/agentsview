package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// TestArchiveAuditRetriesWithBackoffOnFailure drives the audit schedule with a
// wait seam that records delays and a stubbed worker that fails six times then
// succeeds. It asserts the obligation is retained (attempts continue), the retry
// delay doubles and caps at archiveAuditInterval, success returns to the daily
// cadence, and no in-process sync ever runs.
func TestArchiveAuditRetriesWithBackoffOnFailure(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, workerEngineConfig(cfg))
	t.Cleanup(engine.Close)
	em := &scopedEmitter{scopes: make(chan string, 8)}

	attempts := 0
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, mode string, _ func(workerLine),
	) (workerResult, error) {
		assert.Equal(t, "audit", mode)
		attempts++
		if attempts <= 6 {
			return workerResult{Status: "failed"}, errors.New("audit boom")
		}
		return workerResult{Status: "ok", Synced: 1, DiscoveryComplete: true}, nil
	})
	defer restore()

	var delays []time.Duration
	ctx, cancel := context.WithCancel(context.Background())
	wait := func(ctx context.Context, d time.Duration) bool {
		delays = append(delays, d)
		return ctx.Err() == nil
	}
	audit := func(ctx context.Context) bool {
		ok := runArchiveAudit(ctx, cfg, engine, database, lock, em) == nil
		if attempts >= 7 {
			cancel()
		}
		return ok
	}

	runArchiveAuditLoop(ctx, wait, audit)

	assert.Equal(t, 7, attempts, "the audit obligation is retained across failures")
	require.GreaterOrEqual(t, len(delays), 8)
	assert.Equal(t, []time.Duration{
		archiveAuditInterval, // initial daily wait
		1 * time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour,
		16 * time.Hour, archiveAuditInterval, // backoff doubles then caps at 24h
		archiveAuditInterval, // success returns to the daily cadence
	}, delays[:8])
	assert.True(t, engine.LastSync().IsZero(),
		"the audit must never run an in-process sync pass")
}

// TestArchiveAuditEmitsOnDataChange asserts a successful audit that changed data
// emits the sessions scope so connected clients refresh.
func TestArchiveAuditEmitsOnDataChange(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, workerEngineConfig(cfg))
	t.Cleanup(engine.Close)
	em := &scopedEmitter{scopes: make(chan string, 1)}

	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		return workerResult{Status: "ok", Synced: 2, DiscoveryComplete: true}, nil
	})
	defer restore()

	require.NoError(t, runArchiveAudit(
		context.Background(), cfg, engine, database, lock, em,
	))
	select {
	case scope := <-em.scopes:
		assert.Equal(t, "sessions", scope)
	default:
		require.FailNow(t, "an audit with Synced>0 must emit the sessions scope")
	}
}

func TestArchiveAuditReloadsParentSkipCacheAfterWorkerTombstones(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	hashKey := filepath.Join(cfg.AgentDirs[parser.AgentClaude][0], "project", "session.jsonl") +
		"?source_hash=unchanged"
	require.NoError(t, database.ReplaceSkippedFiles(map[string]int64{hashKey: 123}))
	engine := sync.NewEngine(database, workerEngineConfig(cfg))
	t.Cleanup(engine.Close)
	require.Contains(t, engine.SnapshotSkipCache(), hashKey)

	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, workerCfg config.Config, mode string, _ func(workerLine),
	) (workerResult, error) {
		assert.Equal(t, "audit", mode)
		workerDB, err := db.Open(workerCfg.DBPath)
		require.NoError(t, err)
		require.NoError(t, workerDB.ReplaceSkippedFiles(map[string]int64{}))
		require.NoError(t, workerDB.Close())
		return workerResult{
			Status: "ok", Tombstoned: 1, DiscoveryComplete: true,
		}, nil
	})
	defer restore()

	require.NoError(t, runArchiveAudit(
		t.Context(), cfg, engine, database, lock, nil,
	))
	assert.NotContains(t, engine.SnapshotSkipCache(), hashKey,
		"the daemon must discard a hash-qualified entry removed by its audit worker")
}

// TestArchiveAuditReloadsSkipCacheOnLostTerminalResult pins the reload trigger
// to "the worker ran", not to the terminal result's content: a child can commit
// tombstones and durable skip-cache deletions and then die before its result
// line reaches the parent, so a zero-value result with a protocol error must
// still refresh the daemon's in-memory skip cache.
func TestArchiveAuditReloadsSkipCacheOnLostTerminalResult(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	hashKey := filepath.Join(cfg.AgentDirs[parser.AgentClaude][0], "project", "session.jsonl") +
		"?source_hash=unchanged"
	require.NoError(t, database.ReplaceSkippedFiles(map[string]int64{hashKey: 123}))
	engine := sync.NewEngine(database, workerEngineConfig(cfg))
	t.Cleanup(engine.Close)
	require.Contains(t, engine.SnapshotSkipCache(), hashKey)

	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, workerCfg config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		workerDB, err := db.Open(workerCfg.DBPath)
		require.NoError(t, err)
		require.NoError(t, workerDB.ReplaceSkippedFiles(map[string]int64{}))
		require.NoError(t, workerDB.Close())
		return workerResult{}, errors.New(
			"audit worker: sync worker emitted 0 terminal results, want exactly 1",
		)
	})
	defer restore()

	err := runArchiveAudit(t.Context(), cfg, engine, database, lock, nil)
	require.Error(t, err, "the protocol failure must surface so the caller retries")
	assert.NotContains(t, engine.SnapshotSkipCache(), hashKey,
		"a lost terminal result must not leave the daemon's skip cache stale")
}

// TestArchiveAuditEmitsOnPartialSuccess proves committed changes reach SSE
// clients even when the pass also failed: the retry sees those rows as already
// synchronized and would never re-emit, so the emit must not be gated on a nil
// error. The error still propagates so the retry obligation is retained.
func TestArchiveAuditEmitsOnPartialSuccess(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, workerEngineConfig(cfg))
	t.Cleanup(engine.Close)
	em := &scopedEmitter{scopes: make(chan string, 1)}

	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		return workerResult{Status: "failed", Synced: 2, Failed: 1},
			errors.New("audit worker pass reported failed")
	})
	defer restore()

	err := runArchiveAudit(context.Background(), cfg, engine, database, lock, em)
	require.Error(t, err, "the partial failure must still surface for retry")
	select {
	case scope := <-em.scopes:
		assert.Equal(t, "sessions", scope)
	default:
		require.FailNow(t, "an audit that committed sessions must emit even on failure")
	}
}

// TestArchiveAuditSurfacesWorkerFailureWithoutFallback proves the no-fallback
// rule: a failed worker audit returns an error (so the caller retries), runs no
// in-process sync, and emits nothing.
func TestArchiveAuditSurfacesWorkerFailureWithoutFallback(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, workerEngineConfig(cfg))
	t.Cleanup(engine.Close)
	em := &scopedEmitter{scopes: make(chan string, 1)}

	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		return workerResult{Status: "failed"}, errors.New("audit boom")
	})
	defer restore()

	err := runArchiveAudit(context.Background(), cfg, engine, database, lock, em)
	require.Error(t, err, "a failed audit must surface the error for retry")
	assert.True(t, engine.LastSync().IsZero(),
		"a failed audit must not fall back to an in-process sync")
	select {
	case <-em.scopes:
		require.FailNow(t, "a failed audit must not emit")
	default:
	}
}

// TestArchiveAuditAttemptLogsWorkerError proves the worker's terminal Error is
// no longer swallowed: a failed attempt logs the actual error and reports false.
func TestArchiveAuditAttemptLogsWorkerError(t *testing.T) {
	logs := captureLogOutput(t)
	ok := runArchiveAuditAttempt(
		context.Background(), nil,
		func(context.Context) error { return errors.New("audit boom") },
	)
	assert.False(t, ok, "a failed attempt reports failure")
	assert.Contains(t, logs.String(), "audit boom",
		"the worker's terminal error must reach the log")
}

// TestArchiveAuditAttemptSilentOnCancel proves a failure during shutdown does not
// log: the cancelled context suppresses the spurious failure line.
func TestArchiveAuditAttemptSilentOnCancel(t *testing.T) {
	logs := captureLogOutput(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ok := runArchiveAuditAttempt(
		ctx, nil,
		func(context.Context) error { return errors.New("audit boom") },
	)
	assert.False(t, ok)
	assert.NotContains(t, logs.String(), "audit boom",
		"a cancelled attempt must not log a failure")
}

// TestArchiveAuditLoopSuppressesBackoffLogOnShutdown proves the loop stays quiet
// once the context is cancelled: the "next attempt" backoff line is suppressed.
func TestArchiveAuditLoopSuppressesBackoffLogOnShutdown(t *testing.T) {
	logs := captureLogOutput(t)
	ctx, cancel := context.WithCancel(context.Background())
	wait := func(ctx context.Context, _ time.Duration) bool {
		return ctx.Err() == nil
	}
	audit := func(context.Context) bool {
		cancel() // fail this attempt and shut down before the next wait
		return false
	}
	runArchiveAuditLoop(ctx, wait, audit)
	assert.NotContains(t, logs.String(), "next attempt",
		"shutdown must not emit a backoff line")
}

// TestArchiveAuditLoopLogsBackoffWhileRunning proves the backoff line is still
// logged for a genuine mid-run failure, so suppression is scoped to shutdown.
func TestArchiveAuditLoopLogsBackoffWhileRunning(t *testing.T) {
	logs := captureLogOutput(t)
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	wait := func(ctx context.Context, _ time.Duration) bool {
		return ctx.Err() == nil
	}
	audit := func(context.Context) bool {
		attempts++
		if attempts >= 2 {
			cancel()
		}
		return false // first attempt fails while the context is still live
	}
	runArchiveAuditLoop(ctx, wait, audit)
	assert.Contains(t, logs.String(), "next attempt",
		"a live-run failure must log the backoff line")
}

// TestArchiveAuditLoopStopsOnContextCancel guards the shutdown path: a cancelled
// context ends the loop without running an audit.
func TestArchiveAuditLoopStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	audited := false
	runArchiveAuditLoop(ctx,
		func(context.Context, time.Duration) bool { return false },
		func(context.Context) bool { audited = true; return true },
	)
	assert.False(t, audited, "a cancelled context must stop the loop before auditing")
}

// TestSyncWorkerAuditModeRunsSyncPass confirms the audit worker mode performs a
// full authoritative pass over the archive and emits an ok terminal result.
func TestSyncWorkerAuditModeRunsSyncPass(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	var out bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, "audit", &out))
	result := decodeSingleResult(t, &out)
	assert.Equal(t, "ok", result.Status)
	assert.True(t, result.DiscoveryComplete)
	assert.Equal(t, 3, result.Synced)
}

// TestSyncWorkerAuditTombstonesMissedDeletion is the audit's safety-net
// regression: a source file deleted while no watcher was running must be
// tombstoned by the daily audit, with the session row preserved in the
// persistent archive.
func TestSyncWorkerAuditTombstonesMissedDeletion(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, err := db.Open(cfg.DBPath)
	require.NoError(t, err)
	engine := sync.NewEngine(database, workerEngineConfig(cfg))
	require.Equal(t, 3, engine.SyncAll(context.Background(), nil).Synced)
	claudeDir := cfg.AgentDirs[parser.AgentClaude][0]
	deletedPath := filepath.Join(claudeDir, "-home-proj0", "session0.jsonl")
	hashKey := deletedPath + "?source_hash=unchanged"
	require.NoError(t, database.ReplaceSkippedFiles(map[string]int64{hashKey: 123}))
	engine.Close()
	require.NoError(t, database.Close())

	// Delete one source with no watcher running: only the audit can notice.
	require.NoError(t, os.Remove(deletedPath))

	var out bytes.Buffer
	require.NoError(t, runSyncWorker(cfg, "audit", &out))
	result := decodeSingleResult(t, &out)
	assert.Equal(t, "ok", result.Status)
	assert.Equal(t, 1, result.Tombstoned,
		"the audit must reconcile the deletion the watcher missed")

	database, err = db.Open(cfg.DBPath)
	require.NoError(t, err)
	defer database.Close()
	var live, total int
	require.NoError(t, database.Reader().QueryRow(
		"SELECT COUNT(*) FROM sessions WHERE deleted_at IS NULL",
	).Scan(&live))
	require.NoError(t, database.Reader().QueryRow(
		"SELECT COUNT(*) FROM sessions",
	).Scan(&total))
	assert.Equal(t, 2, live, "the deleted source's session must be tombstoned")
	assert.Equal(t, 3, total, "tombstoning must preserve the archived row")
	persistedSkips, err := database.LoadSkippedFiles()
	require.NoError(t, err)
	assert.NotContains(t, persistedSkips, hashKey,
		"the audit worker must durably remove the tombstoned source's hash key")
}
