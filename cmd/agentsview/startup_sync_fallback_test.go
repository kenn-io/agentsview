// ABOUTME: Tests the bounded recovery path for abandoned daemon-launch syncs.
// ABOUTME: Uses a scratch database and synthetic timeout without starting a daemon.
package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

type blockingStartupEmitter struct {
	broadcaster *server.Broadcaster
	committed   chan struct{}
	release     chan struct{}
}

func (e *blockingStartupEmitter) Emit(scope string) {
	e.broadcaster.Emit(scope)
	if scope == "sessions" {
		select {
		case e.committed <- struct{}{}:
			<-e.release
		default:
		}
	}
}

func TestDeferredStartupServesCommittedSessionsDuringImport(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	broadcaster := server.NewBroadcaster(0)
	emitter := &blockingStartupEmitter{broadcaster: broadcaster, committed: make(chan struct{}, 1), release: make(chan struct{})}
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "local", DeferStartupMaintenance: true, Emitter: emitter})
	t.Cleanup(engine.Close)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	cfg.Host = host
	cfg.Port, err = strconv.Atoi(port)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(server.New(cfg, database, engine, server.WithBroadcaster(broadcaster)).Handler())
	_ = srv.Listener.Close()
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)
	events, unsubscribe := broadcaster.Subscribe()
	defer unsubscribe()
	timeout := make(chan time.Time, 1)
	timeout <- time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := runDeferredStartupSyncFallback(t.Context(), cfg, engine, database, nil, nil, timeout, true)
		done <- err
	}()
	t.Cleanup(func() { close(emitter.release); <-done })
	select {
	case <-emitter.committed:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "no session committed")
	}
	select {
	case event := <-events:
		assert.Equal(t, "sessions", event.Scope)
	case <-time.After(time.Second):
		require.FailNow(t, "no session refresh")
	}
	get := func(path string) string {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(request)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		return string(body)
	}
	list := get("/api/v1/sessions?include_one_shot=true&include_automated=true")
	id := ""
	for _, candidate := range []string{"session0", "session1", "session2"} {
		if strings.Contains(list, candidate) {
			id = candidate
			break
		}
	}
	require.NotEmpty(t, id, "session list: %s", list)
	assert.Contains(t, get("/api/v1/sessions/"+id+"/messages"), "hello")
	assert.Contains(t, get("/api/v1/search?q=hello&include_one_shot=true&include_automated=true"), id)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/api/v1/sessions/"+id+"/star", strings.NewReader("{}"))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", srv.URL)
	resp, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestRunDeferredStartupSyncFallbackPerformsSkippedSync(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	reconciled := make(chan struct{}, 1)
	engine := syncpkg.NewEngine(t.Context(), database, syncpkg.EngineConfig{
		Machine:                 "local",
		DeferStartupMaintenance: true,
		OnStartupReconciled: func(syncpkg.SyncStats, error) {
			reconciled <- struct{}{}
		},
	})
	t.Cleanup(engine.Close)
	timeout := make(chan time.Time, 1)
	timeout <- time.Now()

	// Under a test binary the worker path is skipped (testing.Testing()), so
	// the fallback runs the in-process sync; database/lock stay unused.
	ran, err := runDeferredStartupSyncFallback(
		t.Context(), config.Config{}, engine, database, nil, nil, timeout, false,
	)

	require.NoError(t, err)
	assert.True(t, ran)
	assert.False(t, engine.LastSyncStartedAt(t.Context()).IsZero(),
		"timeout fallback must perform the skipped local sync")
	select {
	case <-reconciled:
	case <-time.After(time.Second):
		require.FailNow(t, "deferred fallback did not reconcile watcher startup")
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
	first, err := isFirstImport(t.Context(), path)
	require.NoError(t, err)
	require.True(t, first)
	database := dbtest.OpenTestDBAt(t, path)
	first, err = isFirstImport(t.Context(), path)
	require.NoError(t, err)
	require.True(t, first)
	require.NoError(t, database.SetSyncState(t.Context(), "last_sync_finished_at", time.Now().UTC().Format(time.RFC3339Nano)))
	first, err = isFirstImport(t.Context(), path)
	require.NoError(t, err)
	require.False(t, first)
}
