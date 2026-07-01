package main

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestResolveArchiveQueryBackendNoSyncStartsNoSyncDaemon(t *testing.T) {
	testDataDir(t)
	var started bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, cfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		assert.True(t, cfg.NoSync)
		return &DaemonRuntime{Host: "127.0.0.1", Port: 12345}, nil
	})

	backend := resolveTestArchiveQueryBackend(t, defaultArchiveQueryPolicy(
		func(p *archiveQueryPolicy) { p.NoSync = true },
	))
	assert.True(t, started)
	assert.IsType(t, daemonArchiveQueryBackend{}, backend)
}

func TestResolveArchiveQueryBackendRefusesReadOnlyDaemonForFreshQueries(t *testing.T) {
	dataDir := testDataDir(t)

	var called bool
	ts := sessionUsageRuntimeServer(t, func(
		w http.ResponseWriter, r *http.Request,
	) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	})
	registerTestRuntime(t, dataDir, ts.URL, true)

	_, cleanup, err := resolveArchiveQueryBackend(
		context.Background(), defaultArchiveQueryPolicy(nil),
	)
	if cleanup != nil {
		t.Cleanup(cleanup)
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only")
	assert.NotContains(t, err.Error(), "--pg")
	assert.False(t, called)
}

func TestResolveArchiveQueryBackendUsesGeneratedAutostartToken(t *testing.T) {
	testDataDir(t)

	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, cfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		cfg.AuthToken = "generated-token"
		return &DaemonRuntime{Host: "127.0.0.1", Port: 12345}, nil
	})

	backend := resolveTestArchiveQueryBackend(t, defaultArchiveQueryPolicy(
		func(p *archiveQueryPolicy) {
			p.AutoStart = true
			p.ReadOnlyDaemon = archiveQueryRejectReadOnlyDaemon
		},
	))

	daemonBackend, ok := backend.(daemonArchiveQueryBackend)
	require.True(t, ok)
	assert.Equal(t, "generated-token", daemonBackend.authToken)
}

func TestLocalArchiveQuerySessionUsageNoSyncSkipsSingleSessionSync(
	t *testing.T,
) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "sessions.db")
	writer := dbtest.OpenTestDBAt(t, dbPath)
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
