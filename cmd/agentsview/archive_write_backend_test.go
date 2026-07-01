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
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestLocalArchiveWriteBackendPGPushStopsAfterCanceledLocalSync(t *testing.T) {
	testLocalArchivePushStopsAfterCanceledSync(t,
		func(backend *localArchiveWriteBackend, ctx context.Context) error {
			_, err := backend.PGPush(
				ctx, pgTargetSelection{}, PGPushConfig{}, nil, nil,
			)
			return err
		})
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
