package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/sync"
)

// openTestWriteDB opens a writable DB and acquires the write-owner lock for cfg,
// cleaning both up at test end.
func openTestWriteDB(t *testing.T, cfg config.Config) (*db.DB, *writeOwnerLock) {
	t.Helper()
	database, lock, err := openWriteDB(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { closeWriteDB(database, lock) })
	return database, lock
}

// writeOneSession attempts a single write through the DB writer pool.
func writeOneSession(database *db.DB) error {
	return database.UpsertSession(db.Session{
		ID:      "worker-pass-write",
		Project: "handoff",
		Machine: "local",
		Agent:   "claude",
	})
}

// assertWriteOwnerLockHeld verifies the write-owner flock for dataDir is held
// by probing a competing acquisition, which must fail while the lock is held.
func assertWriteOwnerLockHeld(t *testing.T, dataDir, msg string) {
	t.Helper()
	probe, err := tryAcquireWriteOwnerLock(dataDir)
	if err == nil {
		assert.NoError(t, probe.Close())
	}
	var held writeOwnerLockHeldError
	assert.ErrorAs(t, err, &held, msg)
}

// requireWriteOwnerLockReleased verifies the write-owner flock for dataDir is
// free by acquiring and immediately releasing a probe lock.
func requireWriteOwnerLockReleased(t *testing.T, dataDir, msg string) {
	t.Helper()
	probe, err := tryAcquireWriteOwnerLock(dataDir)
	require.NoError(t, err, msg)
	require.NoError(t, probe.Close())
}

// stubLaunchSyncWorker swaps the launchSyncWorker seam for a test double and
// restores it when the returned func runs.
func stubLaunchSyncWorker(
	t *testing.T,
	fn func(
		context.Context, config.Config, string, func(workerLine),
	) (workerResult, error),
) func() {
	t.Helper()
	prev := launchSyncWorker
	launchSyncWorker = fn
	return func() { launchSyncWorker = prev }
}

// stubCloseWriterFailure swaps the closeWriterForHandoff seam for a double
// that reproduces the drain-timeout failure posture — the writer really is
// closed (barrier active, this process keeps the flock) and an error is
// returned — restoring the seam when the returned func runs.
func stubCloseWriterFailure(t *testing.T) func() {
	t.Helper()
	prev := closeWriterForHandoff
	closeWriterForHandoff = func(d *db.DB) error {
		if err := d.CloseWriter(); err != nil {
			return err
		}
		return errors.New("writer connection still in use after close")
	}
	return func() { closeWriterForHandoff = prev }
}

// TestRunWorkerWritePassRecoversWriterWhenCloseFails pins availability after a
// failed writer close: the pass is abandoned — the worker never launches and
// the write-owner flock is never released — but the writer must be reopened so
// the daemon keeps serving writes instead of returning ErrWriterClosed until
// restart. Reopening cannot admit a second writer because ownership was never
// handed off.
func TestRunWorkerWritePassRecoversWriterWhenCloseFails(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	restoreClose := stubCloseWriterFailure(t)
	defer restoreClose()
	restoreLaunch := stubLaunchSyncWorker(t, func(
		context.Context, config.Config, string, func(workerLine),
	) (workerResult, error) {
		t.Error("worker must not launch after a failed writer close")
		return workerResult{}, nil
	})
	defer restoreLaunch()

	_, err := runWorkerWritePass(
		context.Background(), context.Background(), cfg, engine, database,
		lock, "audit", nil,
	)
	require.ErrorContains(t, err, "close writer for audit pass")
	assertWriteOwnerLockHeld(t, cfg.DataDir,
		"a failed close must keep the write-owner flock")
	assert.NoError(t, writeOneSession(database),
		"writes must recover without a daemon restart")
}

// TestRunWorkerResyncBuildRecoversWriterWhenCloseFails is the resync-build
// arm of the same posture: a failed writer close abandons the build before
// the worker launches, keeps ownership, and must restore write service.
func TestRunWorkerResyncBuildRecoversWriterWhenCloseFails(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, _ := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	restoreClose := stubCloseWriterFailure(t)
	defer restoreClose()
	restoreLaunch := stubLaunchSyncWorker(t, func(
		context.Context, config.Config, string, func(workerLine),
	) (workerResult, error) {
		t.Error("worker must not launch after a failed writer close")
		return workerResult{}, nil
	})
	defer restoreLaunch()

	_, err, spawnFailed := runWorkerResyncBuild(
		context.Background(), context.Background(), cfg, engine, database, nil,
	)
	require.False(t, spawnFailed,
		"a close failure is not a spawn failure and must not trigger the in-process fallback")
	require.ErrorContains(t, err, "close writer for resync build")
	assertWriteOwnerLockHeld(t, cfg.DataDir,
		"a failed close must keep the write-owner flock")
	assert.NoError(t, writeOneSession(database),
		"writes must recover without a daemon restart")
}

func TestRunWorkerWritePassYieldsWriteOwnership(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, mode string, _ func(workerLine),
	) (workerResult, error) {
		assert.Equal(t, "audit", mode)
		requireWriteOwnerLockReleased(t, cfg.DataDir, "flock released before spawn")
		return workerResult{Status: "ok", DiscoveryComplete: true}, nil
	})
	defer restore()

	result, err := runWorkerWritePass(
		context.Background(), context.Background(), cfg, engine, database, lock, "audit", nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Status)
	assertWriteOwnerLockHeld(t, cfg.DataDir, "flock reacquired after the worker")
	assert.NoError(t, writeOneSession(database), "writer reopened")
}

// TestRunWorkerWritePassReloadsSkipCacheBeforeReleasingLock pins the skip-cache
// reload into the pass's own exclusive section. Sync work queued behind the
// pass — a watcher-driven changed-path sync persists the daemon's in-memory
// skip cache wholesale — must only ever observe the post-worker state; if the
// lock were released before the reload, that queued work could durably
// resurrect entries the worker deleted.
func TestRunWorkerWritePassReloadsSkipCacheBeforeReleasingLock(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	hashKey := filepath.Join(t.TempDir(), "session.jsonl") + "?source_hash=unchanged"
	require.NoError(t, database.ReplaceSkippedFiles(map[string]int64{hashKey: 123}))
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()
	require.Contains(t, engine.SnapshotSkipCache(), hashKey)

	queued := make(chan map[string]int64, 1)
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, workerCfg config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		// The worker durably deletes the entry, exactly like a tombstoning
		// audit pass.
		workerDB, err := db.Open(workerCfg.DBPath)
		require.NoError(t, err)
		require.NoError(t, workerDB.ReplaceSkippedFiles(map[string]int64{}))
		require.NoError(t, workerDB.Close())
		// Queue competing exclusive work while the pass still holds the sync
		// lock, then linger so it is blocked on the lock before the pass ends.
		go func() {
			_ = engine.RunExclusive(func() error {
				queued <- engine.SnapshotSkipCache()
				return nil
			})
		}()
		time.Sleep(50 * time.Millisecond)
		return workerResult{
			Status: "ok", Tombstoned: 1, DiscoveryComplete: true,
		}, nil
	})
	defer restore()

	_, err := runWorkerWritePass(
		context.Background(), context.Background(), cfg, engine, database, lock,
		"audit", nil,
	)
	require.NoError(t, err)
	assert.NotContains(t, <-queued, hashKey,
		"work queued behind the pass must observe the reloaded skip cache, "+
			"never the stale pre-worker snapshot")
}

// TestRunWorkerWritePassKeepsSkipCacheOnSpawnFailure pins the reload gate: when
// no worker ever ran, the archive was not touched, so the daemon's in-memory
// skip state (which may be ahead of the last persisted snapshot) must be kept
// rather than clobbered by a reload.
func TestRunWorkerWritePassKeepsSkipCacheOnSpawnFailure(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	hashKey := filepath.Join(t.TempDir(), "session.jsonl") + "?source_hash=unchanged"
	require.NoError(t, database.ReplaceSkippedFiles(map[string]int64{hashKey: 123}))
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()
	require.Contains(t, engine.SnapshotSkipCache(), hashKey)
	// Make the durable snapshot lag the in-memory state, as after a failed
	// persist: a reload here would wrongly drop the in-memory entry.
	require.NoError(t, database.ReplaceSkippedFiles(map[string]int64{}))

	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		return workerResult{}, fmt.Errorf("%w: starting process: boom", errWorkerSpawn)
	})
	defer restore()

	_, err := runWorkerWritePass(
		context.Background(), context.Background(), cfg, engine, database, lock,
		"audit", nil,
	)
	require.ErrorIs(t, err, errWorkerSpawn)
	assert.Contains(t, engine.SnapshotSkipCache(), hashKey,
		"a pass whose worker never ran must not reload away in-memory skip state")
}

func TestRunWorkerWritePassReacquiresOnWorkerFailure(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	workerErr := errors.New("worker boom")
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		return workerResult{Status: "failed"}, workerErr
	})
	defer restore()

	result, err := runWorkerWritePass(
		context.Background(), context.Background(), cfg, engine, database, lock, "audit", nil,
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, workerErr, "worker error must propagate")
	assert.Equal(t, "failed", result.Status)
	assertWriteOwnerLockHeld(t, cfg.DataDir,
		"flock reacquired even after worker failure")
	assert.NoError(t, writeOneSession(database),
		"writer reopened even after worker failure")
}

func TestRunWorkerWritePassRetriesReacquireUntilLockFree(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	prevInitial, prevMax := reacquireBackoffInitial, reacquireBackoffMax
	reacquireBackoffInitial = 5 * time.Millisecond
	reacquireBackoffMax = 20 * time.Millisecond
	defer func() {
		reacquireBackoffInitial, reacquireBackoffMax = prevInitial, prevMax
	}()

	closeErr := make(chan error, 1)
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		// The daemon released the flock for the pass; a contender grabs it and
		// holds it briefly so the first reacquire attempts fail, then releases
		// so the retry loop must recover instead of stranding the writer.
		contender, err := tryAcquireWriteOwnerLock(cfg.DataDir)
		require.NoError(t, err, "contender takes the freed lock")
		go func() {
			time.Sleep(40 * time.Millisecond)
			closeErr <- contender.Close()
		}()
		return workerResult{Status: "ok", DiscoveryComplete: true}, nil
	})
	defer restore()

	result, err := runWorkerWritePass(
		context.Background(), context.Background(), cfg, engine, database, lock, "audit", nil,
	)
	require.NoError(t, err, "pass recovers once the contender releases the lock")
	require.NoError(t, <-closeErr, "contender released the lock cleanly")
	assert.Equal(t, "ok", result.Status)
	assertWriteOwnerLockHeld(t, cfg.DataDir, "flock eventually reacquired")
	assert.NoError(t, writeOneSession(database), "writer reopened after recovery")
}

// TestRunWorkerSyncPassRecordsSyncBookkeeping pins SyncThenRun parity for the
// worker-backed foreground sync: the full SyncStats payload reaches the
// caller, last-sync state feeds /sync/status, startup is reconciled, and the
// "sync" event is emitted for subscribers.
func TestRunWorkerSyncPassRecordsSyncBookkeeping(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	em := &scopedEmitter{scopes: make(chan string, 4)}
	engine := sync.NewEngine(database, sync.EngineConfig{Emitter: em})
	defer engine.Close()

	workerStats := sync.SyncStats{
		TotalSessions:  5,
		Synced:         3,
		OrphanedCopied: 2,
		Warnings:       []string{"one warning"},
	}
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, mode string, _ func(workerLine),
	) (workerResult, error) {
		assert.Equal(t, "sync", mode)
		return workerResult{
			Status: "ok", Synced: 3, DiscoveryComplete: true,
			Stats: &workerStats,
		}, nil
	})
	defer restore()

	stats, ran, err := runWorkerSyncPass(
		context.Background(), context.Background(), cfg, engine, database, lock, false, nil,
	)
	require.NoError(t, err)
	assert.True(t, ran)
	assert.Equal(t, 5, stats.TotalSessions,
		"the full SyncStats payload must survive the worker protocol")
	assert.Equal(t, 2, stats.OrphanedCopied)
	assert.Equal(t, []string{"one warning"}, stats.Warnings)
	assert.False(t, engine.LastSync().IsZero(),
		"last-sync state must reflect the worker-backed pass")
	assert.Equal(t, stats, engine.LastSyncStats())
	assert.True(t, engine.StartupReconciled())
	select {
	case scope := <-em.scopes:
		assert.Equal(t, "sync", scope)
	default:
		require.FailNow(t, "a worker-backed sync with changes must emit the sync event")
	}
}

// TestRunWorkerSyncPassNoWorkerRecordsNothing pins first-attempt semantics for
// every failure mode in which no worker process ran: a pre-launch writer-close
// failure, a pre-launch lock-release failure, and a spawn failure. The pass
// must report ran=false, record no startup reconciliation or last-sync state,
// keep the OnStartupReconciled callback (watcher dispatch) closed, and leave
// the archive writable so a retry or in-process fallback can run the first
// real attempt.
func TestRunWorkerSyncPassNoWorkerRecordsNothing(t *testing.T) {
	tests := []struct {
		name        string
		failClose   bool
		failRelease bool
		failSpawn   bool
		wantErrIs   error
		wantErrText string
	}{
		{
			name:        "writer close failure",
			failClose:   true,
			wantErrIs:   errWorkerHandoff,
			wantErrText: "close writer for sync pass",
		},
		{
			name:        "lock release failure",
			failRelease: true,
			wantErrIs:   errWorkerHandoff,
			wantErrText: "release write lock for sync pass",
		},
		{
			name:      "spawn failure",
			failSpawn: true,
			wantErrIs: errWorkerSpawn,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfigWithClaudeFixture(t)
			database, lock := openTestWriteDB(t, cfg)
			reconciled := make(chan error, 1)
			engine := sync.NewEngine(database, sync.EngineConfig{
				OnStartupReconciled: func(_ sync.SyncStats, err error) {
					reconciled <- err
				},
			})
			defer engine.Close()

			restoreClose := func() {}
			if tt.failClose {
				restoreClose = stubCloseWriterFailure(t)
			}
			defer restoreClose()
			passLock := lock
			if tt.failRelease {
				// A nil inner flock makes Release fail after the writer close
				// succeeded; the daemon's real flock stays held throughout.
				passLock = &writeOwnerLock{}
			}
			restoreLaunch := stubLaunchSyncWorker(t, func(
				context.Context, config.Config, string, func(workerLine),
			) (workerResult, error) {
				if tt.failSpawn {
					return workerResult{}, fmt.Errorf(
						"%w: starting process: boom", errWorkerSpawn,
					)
				}
				t.Error("worker must not launch after a pre-launch handoff failure")
				return workerResult{}, nil
			})
			defer restoreLaunch()

			stats, ran, err := runWorkerSyncPass(
				context.Background(), context.Background(), cfg, engine,
				database, passLock, false, nil,
			)
			require.ErrorIs(t, err, tt.wantErrIs)
			if tt.wantErrText != "" {
				require.ErrorContains(t, err, tt.wantErrText)
			}
			assert.False(t, ran, "no worker ran, so the pass must report ran=false")
			assert.Equal(t, sync.SyncStats{}, stats,
				"a pass without a worker must not synthesize stats")
			assert.False(t, engine.StartupReconciled(),
				"startup must stay unreconciled when no worker ran")
			assert.True(t, engine.LastSync().IsZero(),
				"no last-sync bookkeeping without a worker")
			assert.Equal(t, sync.SyncStats{}, engine.LastSyncStats(),
				"no last-sync stats without a worker")
			select {
			case cbErr := <-reconciled:
				require.FailNowf(t,
					"startup must not be acknowledged when no worker ran",
					"callback fired with: %v", cbErr)
			default:
			}
			assertWriteOwnerLockHeld(t, cfg.DataDir,
				"the daemon must keep write ownership across the failed pass")
			assert.NoError(t, writeOneSession(database),
				"writes must recover without a daemon restart")

			// First-attempt semantics survive: a later pass over the intact
			// state runs the worker and records reconciliation normally.
			restoreClose()
			restoreLaunch()
			restoreRetry := stubLaunchSyncWorker(t, func(
				context.Context, config.Config, string, func(workerLine),
			) (workerResult, error) {
				return workerResult{Status: "ok", DiscoveryComplete: true}, nil
			})
			defer restoreRetry()
			_, ran, err = runWorkerSyncPass(
				context.Background(), context.Background(), cfg, engine,
				database, lock, false, nil,
			)
			require.NoError(t, err)
			assert.True(t, ran, "the retry runs the first real worker pass")
			assert.True(t, engine.StartupReconciled(),
				"the successful retry must reconcile startup")
			select {
			case cbErr := <-reconciled:
				assert.NoError(t, cbErr,
					"the retry acknowledges startup without a prior failed attempt")
			default:
				require.FailNow(t, "a successful retry must acknowledge startup")
			}
		})
	}
}

// TestRunWorkerSyncPassWorkerFailureStillRecords pins the ran-and-failed path:
// a worker that launched and reported failure is surfaced as-is — ran stays
// true, last-sync stats carry the aborted pass, and the startup attempt is
// acknowledged (with its error) rather than re-run.
func TestRunWorkerSyncPassWorkerFailureStillRecords(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	reconciled := make(chan error, 1)
	engine := sync.NewEngine(database, sync.EngineConfig{
		OnStartupReconciled: func(_ sync.SyncStats, err error) {
			reconciled <- err
		},
	})
	defer engine.Close()

	workerErr := errors.New("worker boom")
	restore := stubLaunchSyncWorker(t, func(
		context.Context, config.Config, string, func(workerLine),
	) (workerResult, error) {
		return workerResult{Status: "failed"}, workerErr
	})
	defer restore()

	stats, ran, err := runWorkerSyncPass(
		context.Background(), context.Background(), cfg, engine, database,
		lock, false, nil,
	)
	require.ErrorIs(t, err, workerErr)
	assert.True(t, ran, "a worker that launched and failed still ran")
	assert.True(t, stats.Aborted,
		"an incomplete worker pass must surface as aborted stats")
	assert.Equal(t, stats, engine.LastSyncStats(),
		"the failed pass must reach last-sync bookkeeping")
	select {
	case cbErr := <-reconciled:
		assert.ErrorIs(t, cbErr, workerErr,
			"the startup attempt is acknowledged with the worker's error")
	default:
		require.FailNow(t,
			"a worker that ran must acknowledge the startup attempt")
	}
}

// TestWorkerNeverRanClassifiesFallbackErrors pins the caller-side fallback
// gate: only failures in which no worker process ran — spawn and pre-launch
// handoff — may trigger the in-process fallback; a worker that ran and failed
// must be surfaced without re-running.
func TestWorkerNeverRanClassifiesFallbackErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{
			name: "spawn failure",
			err:  fmt.Errorf("%w: starting process: boom", errWorkerSpawn),
			want: true,
		},
		{
			name: "pre-launch handoff failure",
			err:  fmt.Errorf("%w: close writer for sync pass: boom", errWorkerHandoff),
			want: true,
		},
		{
			name: "worker ran and failed",
			err:  errors.New("sync worker pass reported failed"),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, workerNeverRan(tt.err))
		})
	}
}

// TestRunWorkerSyncPassLockHeldRecheckSkipsDuplicate is the regression for the
// deferred-startup race: the timer fires while a foreground worker pass is
// still running, so the pre-lock StartupReconciled check reads false. The
// foreground pass records reconciliation before releasing the exclusive lock,
// and the deferred pass rechecks the gate while holding it, so exactly one
// archive-scale worker runs.
func TestRunWorkerSyncPassLockHeldRecheckSkipsDuplicate(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	launches := make(chan struct{}, 2)
	release := make(chan struct{})
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		launches <- struct{}{}
		<-release
		return workerResult{Status: "ok", Synced: 1, DiscoveryComplete: true}, nil
	})
	defer restore()

	require.False(t, engine.StartupReconciled(),
		"the deferred fallback's pre-lock gate must read false mid-race")

	type passOutcome struct {
		ran bool
		err error
	}
	foreground := make(chan passOutcome, 1)
	go func() {
		_, ran, err := runWorkerSyncPass(
			context.Background(), context.Background(), cfg, engine, database, lock, false, nil,
		)
		foreground <- passOutcome{ran: ran, err: err}
	}()
	<-launches // the foreground worker is running and holds the exclusive lock

	deferred := make(chan passOutcome, 1)
	go func() {
		_, ran, err := runWorkerSyncPass(
			context.Background(), context.Background(), cfg, engine, database, lock, true, nil,
		)
		deferred <- passOutcome{ran: ran, err: err}
	}()
	// Let the deferred pass reach the exclusive lock before the foreground
	// worker finishes, then release the worker.
	time.Sleep(100 * time.Millisecond)
	close(release)

	fg := <-foreground
	require.NoError(t, fg.err)
	assert.True(t, fg.ran)
	df := <-deferred
	require.NoError(t, df.err)
	assert.False(t, df.ran,
		"the deferred pass must skip after the foreground pass reconciled startup")
	assert.Len(t, launches, 0,
		"exactly one archive-scale worker may launch across the race")
	assertWriteOwnerLockHeld(t, cfg.DataDir, "flock restored after both passes")
	assert.NoError(t, writeOneSession(database), "writer restored after both passes")
}

func TestRunWorkerWritePassRecoversLockAfterRequestCancel(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	prevInitial, prevMax := reacquireBackoffInitial, reacquireBackoffMax
	reacquireBackoffInitial = 5 * time.Millisecond
	reacquireBackoffMax = 20 * time.Millisecond
	defer func() {
		reacquireBackoffInitial, reacquireBackoffMax = prevInitial, prevMax
	}()

	ctx, cancel := context.WithCancel(context.Background())
	closeErr := make(chan error, 1)
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		// The client disconnects mid-pass while a contender briefly holds the
		// freed lock: recovery must still reacquire and reopen the writer.
		contender, err := tryAcquireWriteOwnerLock(cfg.DataDir)
		require.NoError(t, err, "contender takes the freed lock")
		cancel()
		go func() {
			time.Sleep(40 * time.Millisecond)
			closeErr <- contender.Close()
		}()
		return workerResult{Status: "ok", DiscoveryComplete: true}, nil
	})
	defer restore()

	result, err := runWorkerWritePass(
		ctx, context.Background(), cfg, engine, database, lock, "sync", nil,
	)
	require.NoError(t, err,
		"recovery must survive request cancellation during contention")
	require.NoError(t, <-closeErr, "contender released the lock cleanly")
	assert.Equal(t, "ok", result.Status)
	assertWriteOwnerLockHeld(t, cfg.DataDir,
		"flock reacquired despite cancelled request")
	assert.NoError(t, writeOneSession(database),
		"writer reopened despite cancelled request")
}

func TestReacquireWriteOwnerLockStopsOnContextCancel(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	_, lock := openTestWriteDB(t, cfg)
	require.NoError(t, lock.Release())

	prevInitial := reacquireBackoffInitial
	reacquireBackoffInitial = time.Hour // never elapses within the test
	defer func() { reacquireBackoffInitial = prevInitial }()

	// A contender holds the lock so every reacquire attempt fails.
	contender, err := tryAcquireWriteOwnerLock(cfg.DataDir)
	require.NoError(t, err)
	defer func() { assert.NoError(t, contender.Close()) }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = reacquireWriteOwnerLock(ctx, lock, "audit")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestReadWorkerResultRequiresExactlyOneResult(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantErr   string
		wantLines int
	}{
		{
			name: "single result",
			input: `{"progress":{"phase":"syncing"}}` + "\n" +
				`{"result":{"status":"ok","discoveryComplete":true}}` + "\n",
			wantLines: 2,
		},
		{
			name:      "zero results is a protocol failure",
			input:     `{"progress":{"phase":"syncing"}}` + "\n",
			wantErr:   "0 terminal results",
			wantLines: 1,
		},
		{
			name: "duplicate results is a protocol failure",
			input: `{"result":{"status":"ok","discoveryComplete":true}}` + "\n" +
				`{"result":{"status":"ok","discoveryComplete":true}}` + "\n",
			wantErr:   "2 terminal results",
			wantLines: 2,
		},
		{
			name: "malformed line is a protocol failure",
			input: "not json\n" +
				`{"result":{"status":"ok","discoveryComplete":true}}` + "\n",
			wantErr:   "malformed",
			wantLines: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen := 0
			result, err := readWorkerResult(
				strings.NewReader(tt.input),
				func(workerLine) { seen++ },
			)
			assert.Equal(t, tt.wantLines, seen, "forwarded line count")
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "ok", result.Status)
			assert.True(t, result.DiscoveryComplete)
		})
	}
}

// TestOversizedWorkerOutputHelperProcess is the re-exec target for the
// oversized-output regression test. Gated by env, it emits a line beyond the
// parent's workerLineMaxBytes cap followed by far more output than any OS
// pipe buffer holds, then exits 0.
func TestOversizedWorkerOutputHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_OVERSIZED_WORKER_HELPER") != "1" {
		return
	}
	w := bufio.NewWriter(os.Stdout)
	mustWrite := func(s string) {
		_, err := w.WriteString(s)
		require.NoError(t, err)
	}
	mustWrite(strings.Repeat("a", workerLineMaxBytes+1) + "\n")
	filler := strings.Repeat("b", 1024) + "\n"
	for range 512 {
		mustWrite(filler)
	}
	mustWrite(`{"result":{"status":"ok","discoveryComplete":true}}` + "\n")
	require.NoError(t, w.Flush())
	os.Exit(0)
}

// TestCollectWorkerResultOversizedLineDoesNotDeadlock pins the drain-before-
// Wait contract: an oversized line stops the scanner mid-stream, and the
// remaining child output must be discarded before Wait — otherwise the child
// blocks writing to the full stdout pipe, Wait blocks waiting for an exit
// that cannot happen, and the sync pass hangs until daemon shutdown.
func TestCollectWorkerResultOversizedLineDoesNotDeadlock(t *testing.T) {
	cmd := exec.Command(
		os.Args[0], "-test.run=^TestOversizedWorkerOutputHelperProcess$",
	)
	cmd.Env = append(os.Environ(), "AGENTSVIEW_OVERSIZED_WORKER_HELPER=1")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	type outcome struct {
		parseErr error
		waitErr  error
	}
	done := make(chan outcome, 1)
	go func() {
		_, parseErr, waitErr := collectWorkerResult(cmd, stdout, nil)
		done <- outcome{parseErr: parseErr, waitErr: waitErr}
	}()
	select {
	case out := <-done:
		require.ErrorIs(t, out.parseErr, bufio.ErrTooLong,
			"the oversized line must surface as the scanner's protocol error")
		assert.NoError(t, out.waitErr, "the drained child must exit cleanly")
	case <-time.After(30 * time.Second):
		// The wedged child dies on the closed pipe when the test binary exits.
		t.Fatal("collectWorkerResult deadlocked: oversized output left the " +
			"child blocked writing to a full, unread stdout pipe")
	}
}

func TestSyncWorkerAcquiresWriteLockWhenDaemonYielded(t *testing.T) {
	dataDir, cfg := writeDBConfigForTest(t)
	t.Setenv(syncWorkerChildEnvVar, "1")
	_, err := WriteDaemonRuntime(dataDir, "127.0.0.1", 9, "test", false)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dataDir) })

	// The daemon lives but has yielded: writer closed, write lock free.
	database, lock, err := openWorkerWriteDB(cfg)
	require.NoError(t, err,
		"worker must acquire while a yielded daemon still lives")
	closeWriteDB(database, lock)
}

// A sync-worker pass tears down through closeWriteDB (see
// runSyncWorkerStartup): when the database close fails because a connection
// never drained, the write-owner flock must be retained — releasing it would
// let another process acquire writer ownership alongside the surviving
// SQLite connection.
func TestSyncWorkerTeardownKeepsWriteOwnerLockWhenCloseFails(t *testing.T) {
	restore := db.SetCloseDrainTimeoutForTest(100 * time.Millisecond)
	defer restore()
	_, cfg := writeDBConfigForTest(t)

	database, lock, err := openWorkerWriteDB(cfg)
	require.NoError(t, err)

	rows, err := database.Reader().Query("SELECT 1")
	require.NoError(t, err, "hold a reader connection so Close cannot drain")

	closeWriteDB(database, lock)
	assertWriteOwnerLockHeld(t, cfg.DataDir,
		"a failed database close must retain the write-owner flock")

	require.NoError(t, rows.Close())
	closeWriteDB(database, lock)
	requireWriteOwnerLockReleased(t, cfg.DataDir,
		"a successful close after the connection drains releases the flock")
}

func TestSyncWorkerRefusedWhenDaemonHoldsWriteLock(t *testing.T) {
	dataDir, cfg := writeDBConfigForTest(t)
	t.Setenv(syncWorkerChildEnvVar, "1")
	_, err := WriteDaemonRuntime(dataDir, "127.0.0.1", 9, "test", false)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dataDir) })
	holdWriteOwnerLockForTest(t, dataDir) // daemon has NOT yielded the flock

	_, _, err = openWorkerWriteDB(cfg)
	require.Error(t, err,
		"worker must be refused while the daemon still holds the write lock")
	assert.ErrorContains(t, err, "write lock")
}

// A failed writer restoration must be retried, not logged and abandoned:
// giving up leaves the daemon running with every write endpoint failing
// until restart.
func TestRestoreArchiveAccessRetriesUntilSuccess(t *testing.T) {
	prevInitial := reacquireBackoffInitial
	reacquireBackoffInitial = time.Millisecond
	defer func() { reacquireBackoffInitial = prevInitial }()

	attempts := 0
	err := restoreArchiveAccess(
		context.Background(), "test restore", func() error {
			attempts++
			if attempts < 3 {
				return fmt.Errorf("attempt %d fails", attempts)
			}
			return nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, 3, attempts,
		"restoration must retry failures until it succeeds")
}

func TestRestoreArchiveAccessStopsOnContextCancel(t *testing.T) {
	prevInitial := reacquireBackoffInitial
	reacquireBackoffInitial = time.Hour // never elapses within the test
	defer func() { reacquireBackoffInitial = prevInitial }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := restoreArchiveAccess(ctx, "test restore", func() error {
		return fmt.Errorf("always fails")
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// Persistent recovery failure must not outlive the daemon: recovery ignores
// request cancellation but stops at daemon shutdown, so a held-forever lock
// cannot block exit while the pass holds the engine's exclusive sync lock.
func TestRunWorkerWritePassShutdownStopsPersistentRecovery(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database, lock := openTestWriteDB(t, cfg)
	engine := sync.NewEngine(database, sync.EngineConfig{})
	defer engine.Close()

	prevInitial, prevMax := reacquireBackoffInitial, reacquireBackoffMax
	reacquireBackoffInitial = 5 * time.Millisecond
	reacquireBackoffMax = 20 * time.Millisecond
	defer func() {
		reacquireBackoffInitial, reacquireBackoffMax = prevInitial, prevMax
	}()

	daemonCtx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	var contender *writeOwnerLock
	restore := stubLaunchSyncWorker(t, func(
		_ context.Context, _ config.Config, _ string, _ func(workerLine),
	) (workerResult, error) {
		// A contender takes the freed lock and never releases it, then the
		// daemon shuts down while reacquisition is retrying.
		taken, err := tryAcquireWriteOwnerLock(cfg.DataDir)
		require.NoError(t, err, "contender takes the freed lock")
		contender = taken
		shutdown()
		return workerResult{Status: "ok", DiscoveryComplete: true}, nil
	})
	defer restore()
	defer func() {
		if contender != nil {
			assert.NoError(t, contender.Close())
		}
	}()

	done := make(chan error, 1)
	go func() {
		_, err := runWorkerWritePass(
			context.Background(), daemonCtx, cfg, engine, database, lock,
			"sync", nil,
		)
		done <- err
	}()
	select {
	case err := <-done:
		require.Error(t, err,
			"an unrecovered pass must surface its failure")
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("recovery kept retrying past daemon shutdown")
	}
}
