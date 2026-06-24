package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestResolveArchiveQueryBackendNoSyncDoesNotAutostartDaemon(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	oldStart := startBackgroundServeForTransport
	startBackgroundServeForTransport = func(
		context.Context, *config.Config, time.Duration,
	) (*DaemonRuntime, error) {
		t.Fatal("--no-sync archive query must not auto-start a daemon")
		return nil, nil
	}
	t.Cleanup(func() { startBackgroundServeForTransport = oldStart })

	backend, cleanup, err := resolveArchiveQueryBackend(
		context.Background(),
		archiveQueryPolicy{
			NoSync:               true,
			ReadOnlyDaemon:       archiveQuerySkipReadOnlyDaemon,
			DirectReadOnlyAction: "refresh usage directly",
		},
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.IsType(t, localArchiveQueryBackend{}, backend)
}

func TestResolveArchiveQueryBackendSkipsReadOnlyDaemonForFreshQueries(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	var called bool
	ts := sessionUsageRuntimeServer(t, func(
		w http.ResponseWriter, r *http.Request,
	) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	})
	registerTestRuntime(t, dataDir, ts.URL, true)

	backend, cleanup, err := resolveArchiveQueryBackend(
		context.Background(),
		archiveQueryPolicy{
			ReadOnlyDaemon:       archiveQuerySkipReadOnlyDaemon,
			DirectReadOnlyAction: "refresh usage directly",
		},
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.IsType(t, localArchiveQueryBackend{}, backend)
	assert.False(t, called)
}

func TestResolveArchiveQueryBackendUsesGeneratedAutostartToken(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	oldStart := startBackgroundServeForTransport
	startBackgroundServeForTransport = func(
		_ context.Context, cfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		cfg.AuthToken = "generated-token"
		return &DaemonRuntime{Host: "127.0.0.1", Port: 12345}, nil
	}
	t.Cleanup(func() { startBackgroundServeForTransport = oldStart })

	backend, cleanup, err := resolveArchiveQueryBackend(
		context.Background(),
		archiveQueryPolicy{
			AutoStart:            true,
			ReadOnlyDaemon:       archiveQueryRejectReadOnlyDaemon,
			DirectReadOnlyAction: "refresh usage directly",
		},
	)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	daemonBackend, ok := backend.(daemonArchiveQueryBackend)
	require.True(t, ok)
	assert.Equal(t, "generated-token", daemonBackend.authToken)
}

func TestLocalArchiveQuerySessionUsageNoSyncSkipsSingleSessionSync(
	t *testing.T,
) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "sessions.db")
	writer, err := db.Open(dbPath)
	require.NoError(t, err)
	started := "2026-06-23T12:00:00Z"
	require.NoError(t, writer.UpsertSession(db.Session{
		ID:                   "codex:no-sync-usage",
		Project:              "proj",
		Machine:              "local",
		Agent:                "codex",
		StartedAt:            &started,
		MessageCount:         1,
		TotalOutputTokens:    42,
		HasTotalOutputTokens: true,
	}))
	require.NoError(t, writer.Close())

	readonly, err := db.OpenReadOnly(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { readonly.Close() })

	backend := localArchiveQueryBackend{
		cfg:           config.Config{DBPath: dbPath},
		database:      readonly,
		offline:       true,
		skipFreshData: true,
	}
	stderr := captureStderr(t, func() {
		out, exitCode, err := backend.SessionUsage(
			context.Background(), "codex:no-sync-usage",
		)
		require.NoError(t, err)
		require.NotNil(t, out)
		assert.Equal(t, tokenUseExitOK, exitCode)
		assert.Equal(t, 42, out.TotalOutputTokens)
	})
	assert.NotContains(t, stderr, "warning: sync failed")
	assert.NotContains(t, stderr, "warning: pricing seed failed")
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })

	var buf bytes.Buffer
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(&buf, r)
		readDone <- err
	}()

	fn()

	require.NoError(t, w.Close())
	os.Stderr = orig

	require.NoError(t, <-readDone)
	require.NoError(t, r.Close())
	return buf.String()
}
