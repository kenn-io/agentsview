package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/postgres"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// TestDaemonPushHeartbeat pins the daemon-delegated push's client-side
// output: an immediate delegation announcement, elapsed-time heartbeats
// while the blocking POST is in flight, and silence after stop.
func TestDaemonPushHeartbeat(t *testing.T) {
	orig := daemonPushHeartbeatInterval
	daemonPushHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { daemonPushHeartbeatInterval = orig })

	var out syncBuffer
	stop := startDaemonPushHeartbeatTo(&out, "PostgreSQL")
	assert.Contains(t, out.String(),
		"Pushing to PostgreSQL via the local daemon")

	require.Eventually(t, func() bool {
		return strings.Contains(out.String(),
			"still pushing to PostgreSQL via the daemon")
	}, time.Second, time.Millisecond, "heartbeat line must appear")

	stop()
	settled := out.String()
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, settled, out.String(), "no output after stop")
}

// TestDaemonPushProgressSilencesHeartbeat pins that the first streamed
// progress event stops the elapsed-time heartbeat for good: once the daemon
// starts reporting real progress, heartbeat lines must not interleave with
// the in-place progress line.
func TestDaemonPushProgressSilencesHeartbeat(t *testing.T) {
	orig := daemonPushHeartbeatInterval
	daemonPushHeartbeatInterval = 50 * time.Millisecond
	t.Cleanup(func() { daemonPushHeartbeatInterval = orig })

	out := captureStdout(t, func() {
		onProgress, finish := daemonPushProgress(
			"PostgreSQL", newPGPushProgressPrinter(),
		)
		onProgress(postgres.PushProgress{SessionsDone: 1, SessionsTotal: 2})
		time.Sleep(150 * time.Millisecond)
		finish()
	})

	assert.Contains(t, out, "Pushing to PostgreSQL via the local daemon")
	assert.Contains(t, out, "Pushing... 1/2 sessions")
	assert.NotContains(t, out, "still pushing",
		"heartbeat must stay silent after the first progress event")
}

// syncBuffer is a mutex-guarded bytes.Buffer: the heartbeat goroutine writes
// while the test reads.
type syncBuffer struct {
	mu  stdsync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestLocalArchiveWriteBackendPGPushStopsAfterCanceledLocalSync(t *testing.T) {
	testLocalArchivePushStopsAfterCanceledSync(t,
		func(backend *localArchiveWriteBackend, ctx context.Context) error {
			_, err := backend.PGPush(
				ctx, pgTargetSelection{}, PGPushConfig{}, nil, nil,
			)
			return err
		})
}

func TestLocalPGPushEnsuresPricingBeforeConnecting(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)
	backend.ensurePricing = func(_ context.Context, database *db.DB) error {
		require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{{
			ModelPattern:  "new-model",
			InputPerMTok:  2,
			OutputPerMTok: 8,
		}}))
		return nil
	}

	_, err := backend.PGPush(
		context.Background(),
		pgTargetSelection{PG: config.PGConfig{URL: unreachablePGURL}},
		PGPushConfig{}, nil, nil,
	)

	require.Error(t, err)
	rate, err := backend.database.GetModelPricing("new-model")
	require.NoError(t, err)
	require.NotNil(t, rate)
	assert.Equal(t, 8.0, rate.OutputPerMTok)
}

func TestLocalPGWatchPusherUsesBackendPricingEnsure(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)
	ensureCalls := 0
	backend.ensurePricing = func(_ context.Context, database *db.DB) error {
		require.Same(t, backend.database, database)
		ensureCalls++
		return nil
	}
	target := &fakeTarget{}
	pusher := backend.newPGPusher(
		func(context.Context) error { return nil },
		func() (pgTarget, error) { return target, nil },
	)

	require.NoError(t, pusher.push(
		context.Background(), reasonChange, false,
	))
	assert.Equal(t, 1, ensureCalls)
	assert.Equal(t, 1, target.pushes)
}

func TestLocalArchiveWriteBackendDuckDBPushStopsAfterCanceledLocalSync(t *testing.T) {
	testLocalArchivePushStopsAfterCanceledSync(t,
		func(backend *localArchiveWriteBackend, ctx context.Context) error {
			_, err := backend.DuckDBPush(
				ctx, config.DuckDBConfig{}, DuckDBPushConfig{}, nil, nil,
			)
			return err
		})
}

func TestLocalArchiveWriteBackendDuckDBPushUsesConfiguredRemoteURL(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)

	captureStdout(t, func() {
		_, err := backend.DuckDBPush(
			context.Background(),
			config.DuckDBConfig{
				URL:         "quack:https://duck.example.test",
				MachineName: "workstation",
			},
			DuckDBPushConfig{},
			nil,
			nil,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "duckdb quack token is required")
	})
}

func TestLocalArchiveWriteBackendDuckDBPushValidatesRemoteBeforeLocalSync(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)

	var err error
	out := captureStdout(t, func() {
		_, err = backend.DuckDBPush(
			context.Background(),
			config.DuckDBConfig{
				URL:         "quack:https://duck.example.test",
				MachineName: "workstation",
			},
			DuckDBPushConfig{Full: true},
			nil,
			nil,
		)
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "duckdb quack token is required")
	assert.NotContains(t, out, "Database:")
	assert.NotContains(t, out, "Opening DuckDB mirror")
}

func TestRunPGWatchStartupSyncFallsBackAfterAbortedResync(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	missingPath := filepath.Join(t.TempDir(), "missing.jsonl")
	dbtest.SeedSession(t, database, "existing", "proj",
		func(s *db.Session) {
			s.FilePath = &missingPath
		})
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{})

	didResync, err := runPGWatchStartupSync(
		context.Background(), engine, true,
	)

	require.NoError(t, err)
	assert.True(t, didResync)
	assert.False(t, engine.LastSyncStats().Aborted)
	assert.False(t, engine.LastSync().IsZero())
}

func TestLocalArchiveWriteBackendPGPushWatchCanceledStartupIsClean(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)

	err := backend.PGPushWatch(
		canceledContext(),
		pgTargetSelection{},
		PGPushConfig{},
		nil,
		nil,
		time.Millisecond,
		time.Millisecond,
	)

	require.NoError(t, err)
}

func TestResolveArchiveWriteBackendCopiesNoSyncRuntime(t *testing.T) {
	dataDir := t.TempDir()
	host, port := testPingServer(t)
	_, err := WriteDaemonRuntimeWithAuthAndNoSync(
		dataDir, host, port, "test", false, false, true,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dataDir) })

	backend, cleanup, err := resolveArchiveWriteBackend(
		context.Background(), config.Config{DataDir: dataDir},
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	daemonBackend, ok := backend.(daemonArchiveWriteBackend)
	require.True(t, ok)
	assert.True(t, daemonBackend.appCfg.NoSync)
}

// canceledContext returns a context that has already been canceled.
func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// testLocalArchivePushStopsAfterCanceledSync asserts that a local push aborts
// with context.Canceled when its context is already canceled. push runs the
// backend-specific push call and returns its error.
func testLocalArchivePushStopsAfterCanceledSync(
	t *testing.T,
	push func(*localArchiveWriteBackend, context.Context) error,
) {
	t.Helper()
	backend := testLocalArchiveWriteBackend(t)

	var err error
	captureStdout(t, func() {
		err = push(backend, canceledContext())
	})

	require.ErrorIs(t, err, context.Canceled)
}

func testLocalArchiveWriteBackend(t *testing.T) *localArchiveWriteBackend {
	t.Helper()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "sessions.db")
	database := dbtest.OpenTestDBAt(t, dbPath)

	return &localArchiveWriteBackend{
		appCfg: config.Config{
			DataDir: dataDir,
			DBPath:  dbPath,
		},
		database: database,
	}
}
