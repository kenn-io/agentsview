//go:build !(windows && arm64)

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/storage"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

// These tests open a real DuckDB mirror. duckdb-go-bindings ships no
// prebuilt DuckDB library for windows/arm64, where the backend reports an
// unsupported-platform error instead.

func TestAutomaticContentionRetainsWatchWork(t *testing.T) {
	ctx := t.Context()
	local := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	opts := storage.MirrorPushOptions{Automatic: true}
	_, err := duckdbsync.Push(ctx, path, local, "test-machine", opts, false, nil)
	require.NoError(t, err)
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: db.Session{
			ID: "watch-session", Project: "alpha", Machine: "local", Agent: "claude",
			CreatedAt: "2026-02-01T00:00:00Z", MessageCount: 1, UserMessageCount: 1,
		},
		Messages: []db.Message{{
			SessionID: "watch-session", Ordinal: 0, Role: "user", Content: "hello",
		}},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)

	// Hold the same file lock as a competing push, with no unfinished rebuild.
	holder := flock.New(filepath.Join(path+".agentsview-work", filepath.Base(path)+".rebuild.lock"))
	locked, err := holder.TryLock()
	require.NoError(t, err)
	require.True(t, locked)
	t.Cleanup(func() { require.NoError(t, holder.Unlock()) })

	pusher := &duckDBPusher{mirrorPush: func(ctx context.Context, full bool) (storage.MirrorPushResult, error) {
		return duckdbsync.Push(ctx, path, local, "test-machine", opts, full, nil)
	}}
	loop, _, _ := newBatchTestLoop(func(ctx context.Context, reason pushReason, batch *syncpkg.WatchBatch) error {
		require.NotNil(t, batch)
		assert.Equal(t, []string{"session.jsonl"}, batch.Paths)
		return pusher.push(ctx, reason, false)
	})
	ack := loop.NotifyBatchWithAck(syncpkg.WatchBatch{Paths: []string{"session.jsonl"}})
	loop.runNextTask(ctx, false)

	mirroredCount := func() int {
		conn, err := duckdbsync.OpenReadOnly(ctx, path)
		require.NoError(t, err)
		defer func() { require.NoError(t, conn.Close()) }()
		var count int
		require.NoError(t, conn.QueryRowContext(ctx,
			"SELECT count(*) FROM sessions WHERE id = 'watch-session'",
		).Scan(&count))
		return count
	}
	acknowledged := false
	select {
	case err := <-ack:
		acknowledged = true
		assert.NoError(t, err)
		assert.Equal(t, 1, mirroredCount(), "acknowledgement requires the watch session to be published")
	default:
	}

	// Retry the retained batch without another filesystem event or floor push.
	require.NoError(t, holder.Unlock())
	loop.runNextTask(ctx, false)
	assert.Equal(t, 1, mirroredCount(), "contention must not discard unpublished watch work")
	if !acknowledged {
		select {
		case err := <-ack:
			assert.NoError(t, err)
		default:
			assert.Fail(t, "published watch work must be acknowledged")
		}
	}
}

func TestProbeDuckDBMirrorForServeAcceptsCompatibleMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	buildEmptyDuckDBMirrorFixture(t, path)

	assert.NoError(t, probeDuckDBMirrorForServe(t.Context(), path))
}

func TestDuckDBServeRuntimeRecordWriteFailureWarnsVisible(t *testing.T) {
	out, err := runDuckDBRuntimeWarningHelper(t)
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "could not write daemon runtime record")
}

func runDuckDBRuntimeWarningHelper(t *testing.T) ([]byte, error) {
	t.Helper()
	dataDir := t.TempDir()
	mirrorPath := filepath.Join(dataDir, "mirror.duckdb")
	buildEmptyDuckDBMirrorFixture(t, mirrorPath)
	return runRuntimeWarningHelperProcess(
		t, "DuckDB", "TestRunDuckDBRuntimeWarningHelperProcess",
		[]string{
			"AGENTSVIEW_RUN_DUCKDB_RUNTIME_WARNING_HELPER=1",
			"AGENTSVIEW_DATA_DIR=" + dataDir,
			"AGENTSVIEW_DUCKDB_RUNTIME_WARNING_PATH=" + mirrorPath,
		},
		"could not write daemon runtime record",
	)
}

// buildEmptyDuckDBMirrorFixture creates a schema-compatible, empty DuckDB
// mirror file at path. 'duckdb serve' now probes instead of migrating (see
// probeDuckDBMirrorForServe), so it fatally refuses to serve a missing or
// bare file; tests that just need serve to reach its normal startup path
// must seed a valid mirror first instead of relying on serve to create one.
func buildEmptyDuckDBMirrorFixture(t *testing.T, path string) {
	t.Helper()

	conn, err := duckdbsync.Open(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, duckdbsync.EnsureSchema(t.Context(), conn))
	require.NoError(t, conn.Close())
}

func TestRunDuckDBRuntimeWarningHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_RUN_DUCKDB_RUNTIME_WARNING_HELPER") != "1" {
		return
	}
	writeDaemonRuntimeWithAuth = func(
		string, string, int, string, string, bool, bool, ...int,
	) (string, error) {
		return "", errors.New("forced runtime-record write failure")
	}
	// This is only an orphan guard if the parent dies; normal completion is
	// driven by the parent observing the warning on stdout.
	time.AfterFunc(2*time.Minute, func() { os.Exit(0) })
	runDuckDBServe(t.Context(), config.Config{
		Host:    "127.0.0.1",
		Port:    0,
		DataDir: os.Getenv("AGENTSVIEW_DATA_DIR"),
		DuckDB: config.DuckDBConfig{
			Path: os.Getenv("AGENTSVIEW_DUCKDB_RUNTIME_WARNING_PATH"),
		},
	}, "")
}
