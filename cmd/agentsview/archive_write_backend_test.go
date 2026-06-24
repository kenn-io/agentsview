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
	backend := testLocalArchiveWriteBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var err error
	captureStdout(t, func() {
		_, err = backend.PGPush(
			ctx, config.PGConfig{}, PGPushConfig{}, nil, nil,
		)
	})

	require.ErrorIs(t, err, context.Canceled)
}

func TestLocalArchiveWriteBackendDuckDBPushStopsAfterCanceledLocalSync(t *testing.T) {
	backend := testLocalArchiveWriteBackend(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var err error
	captureStdout(t, func() {
		_, err = backend.DuckDBPush(
			ctx, config.DuckDBConfig{}, DuckDBPushConfig{}, nil, nil,
		)
	})

	require.ErrorIs(t, err, context.Canceled)
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := backend.PGPushWatch(
		ctx,
		config.PGConfig{},
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

func testLocalArchiveWriteBackend(t *testing.T) *localArchiveWriteBackend {
	t.Helper()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "sessions.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	return &localArchiveWriteBackend{
		appCfg: config.Config{
			DataDir: dataDir,
			DBPath:  dbPath,
		},
		database: database,
	}
}
