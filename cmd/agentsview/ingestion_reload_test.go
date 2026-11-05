package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/server"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func writeReloadGeminiSession(t *testing.T, root, id string) {
	t.Helper()
	path := filepath.Join(root, "tmp", "project", "chats", "session-"+id+".json")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.GeminiSessionJSON(
		id, "project", "2026-08-09T10:00:00Z", "2026-08-09T10:01:00Z",
		[]map[string]any{testjsonl.GeminiUserMsg(
			"user", "2026-08-09T10:00:00Z", "hello from "+id,
		)},
	)), 0o644))
}

func sessionImported(t *testing.T, database *db.DB, id string) bool {
	t.Helper()
	stored, err := database.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	return stored != nil && stored.DeletedAt == nil
}

func searchIngestionArchive(t *testing.T, endpoint, query string) (http.Header, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint+"/api/v1/search?q="+url.QueryEscape(query), nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, response.Body.Close())
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	return response.Header, string(body)
}

func TestDaemonIngestionReloadAppliesProviderSettings(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	base := t.TempDir()
	primary := filepath.Join(base, "gemini")
	alternate := filepath.Join(base, "gemini-work")
	writeReloadGeminiSession(t, primary, "primary")
	writeReloadGeminiSession(t, alternate, "alternate")

	startup := config.Config{
		AgentDirs:      map[parser.AgentType][]string{parser.AgentGemini: {primary}},
		DisabledAgents: []parser.AgentType{parser.AgentGemini},
	}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{
		AgentDirs:      startup.AgentDirs,
		DisabledAgents: startup.DisabledAgents,
		Machine:        "test-machine",
	})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	require.False(t, sessionImported(t, database, "gemini:primary"))

	var mu sync.Mutex
	saved := startup
	load := func() (config.Config, error) {
		mu.Lock()
		defer mu.Unlock()
		return saved, nil
	}
	ingestion := newDaemonIngestion(
		t.Context(), startup, engine, database, nil, load,
	)
	t.Cleanup(ingestion.Stop)
	ingestion.OpenWatcherDispatch()

	// The user enables Gemini and adds a second home in one saved change.
	mu.Lock()
	saved = config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentGemini: {primary, alternate},
		},
	}
	mu.Unlock()
	reloaded, err := ingestion.Reload(t.Context())
	require.NoError(t, err)
	assert.Empty(t, reloaded.DisabledAgents)

	// The engine switches before the replacement watcher registers, so wait
	// for the whole apply; a file written in between would go unseen.
	ingestion.applying.Wait()
	require.Equal(t,
		[]string{primary, alternate},
		engine.ReconciliationRootsForAgent(string(parser.AgentGemini)),
		"the engine switches to the saved provider set")
	assert.False(t, sessionImported(t, database, "gemini:primary"),
		"a reload does not sync by itself")

	// A session written after the change is picked up by the replacement
	// watcher, which covers the new root.
	writeReloadGeminiSession(t, alternate, "later")
	assert.Eventually(t, func() bool {
		return sessionImported(t, database, "gemini:later")
	}, 20*time.Second, 50*time.Millisecond,
		"the watcher follows the new provider settings")

	// Sessions that were already on disk arrive with the next sync.
	engine.SyncAll(t.Context(), nil)
	assert.True(t, sessionImported(t, database, "gemini:primary"))
	assert.True(t, sessionImported(t, database, "gemini:alternate"))
}

func TestDaemonIngestionIdleSearchAndDirectRead(t *testing.T) {
	for _, mode := range []struct{ direct, idle bool }{{false, true}, {true, true}, {false, false}} {
		t.Run(fmt.Sprintf("direct=%t/idle=%t", mode.direct, mode.idle), func(t *testing.T) {
			var watchAttempts atomic.Int32
			originalLog := log.Writer()
			log.SetOutput(observedOutput{Writer: io.Discard, observe: func(p []byte) {
				if strings.Contains(string(p), "triggering sync") {
					watchAttempts.Add(1)
				}
			}})
			t.Cleanup(func() { log.SetOutput(originalLog) })
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			cfg := config.Config{Host: "127.0.0.1", DataDir: t.TempDir(), DBPath: database.Path(), AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}, InstallationID: "test-machine"}
			writeReloadGeminiSession(t, root, "before")
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: cfg.InstallationID})
			t.Cleanup(engine.Close)
			engine.SyncAll(t.Context(), nil)
			tracker := server.NewIdleTracker(0, func() {})
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, tracker, nil)
			if mode.idle {
				tracker.SetSyncIdle(time.Second, ingestion.Suspend)
			}
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			go tracker.Run(ctx)
			t.Cleanup(ingestion.Stop)
			ingestion.OpenWatcherDispatch()
			ts := httptest.NewUnstartedServer(nil)
			cfg.Port = ts.Listener.Addr().(*net.TCPAddr).Port
			srv := server.New(cfg, database, engine, server.WithBaseContext(t.Context()), server.WithIngestionFreshness(ingestion), server.WithIdleTracker(tracker))
			ts.Config.Handler = srv.Handler()
			ts.Start()
			t.Cleanup(ts.Close)
			endpoint := serverEndpoint(t, ts)
			writeDaemonRuntimeForTest(t, cfg.DataDir, endpoint.Host, endpoint.Port, version, false)
			for _, id := range []string{"morning", "afternoon"} {
				if mode.idle {
					require.Eventually(t, ingestion.Pending, 10*time.Second, 10*time.Millisecond)
					ingestion.applying.Wait()
				}
				beforeWatch := watchAttempts.Load()
				writeReloadGeminiSession(t, root, id)
				if mode.idle {
					assert.Never(t, func() bool { return sessionImported(t, database, "gemini:"+id) }, time.Second, 50*time.Millisecond, "quiet ingestion leaves the new session on disk")
				} else {
					require.Eventually(t, func() bool { return sessionImported(t, database, "gemini:"+id) }, 10*time.Second, 50*time.Millisecond)
				}
				quietAttempts := watchAttempts.Load() - beforeWatch
				if mode.idle {
					assert.Zero(t, quietAttempts)
				} else {
					assert.EqualValues(t, 1, quietAttempts)
				}
				if mode.direct {
					ensureFreshData(t.Context(), cfg, database, false)
					stored, err := database.GetSessionFull(t.Context(), "gemini:"+id)
					require.NoError(t, err)
					require.NotNil(t, stored)
				}
				headers, body := searchIngestionArchive(t, ts.URL, id)
				assert.Contains(t, body, "hello from "+id)
				assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader))
				assert.False(t, ingestion.Pending())
				t.Logf("idle=%t write=%s quiet watcher sync attempts=%d", mode.idle, id, quietAttempts)
			}
		})
	}
}

func TestDaemonIngestionReloadKeepsArchiveRequestBudget(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	writeReloadGeminiSession(t, root, "retained")
	cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	saved := cfg
	saved.DisabledAgents = []parser.AgentType{parser.AgentGemini}
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, func() (config.Config, error) { return saved, nil })
	t.Cleanup(ingestion.Stop)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	go func() {
		defer close(finished)
		_ = engine.RunExclusive(func() error { close(entered); <-release; return nil })
	}()
	<-entered
	ingestion.Suspend()
	_, err := ingestion.Reload(t.Context())
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(ingestion.Config().DisabledAgents) == 1 }, time.Second, time.Millisecond)
	// Reload has published its config and is waiting on real engine work.
	ts := httptest.NewUnstartedServer(nil)
	cfg.Port = ts.Listener.Addr().(*net.TCPAddr).Port
	ts.Config.Handler = server.New(cfg, database, engine, server.WithIngestionFreshness(ingestion)).Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/search?q=retained", nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	releaseOnce()
	<-finished
	require.NoError(t, err, "archive reads retain their two-second catch-up budget during reload")
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "hello from retained")
	assert.Equal(t, "true", response.Header.Get(apiclient.ArchiveStaleHeader))
	require.True(t, ingestion.EnsureFresh(t.Context()), "the shared catch-up finishes after the reload")
}

func TestDaemonIngestionSlowCatchupKeepsAuditWaiting(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	tracker := server.NewIdleTracker(0, func() {})
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, tracker, nil)
	t.Cleanup(ingestion.Stop)
	ingestion.Suspend()
	ingestion.applying.Wait()
	writeReloadGeminiSession(t, root, "overnight")
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	go func() {
		defer close(finished)
		_ = engine.RunExclusive(func() error { close(entered); <-release; return nil })
	}()
	<-entered
	auditReady := make(chan bool, 1)
	go func() { auditReady <- ingestion.WaitActive(t.Context()) }()
	ts := httptest.NewUnstartedServer(nil)
	cfg.Port = ts.Listener.Addr().(*net.TCPAddr).Port
	ts.Config.Handler = server.New(cfg, database, engine, server.WithIngestionFreshness(ingestion)).Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	headers, _ := searchIngestionArchive(t, ts.URL, "overnight")
	require.Equal(t, "true", headers.Get(apiclient.ArchiveStaleHeader))
	require.NotNil(t, ingestion.ActiveContext(), "watching resumes before catch-up finishes")
	assert.Empty(t, auditReady, "the audit waits for catch-up, not just watcher setup")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.False(t, ingestion.EnsureFresh(ctx), "one canceled reader leaves shared work running")
	releaseOnce()
	<-finished
	headers, body := searchIngestionArchive(t, ts.URL, "overnight")
	assert.Contains(t, body, "hello from overnight")
	assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader))
	require.True(t, <-auditReady)
}

func TestDaemonIngestionFailedCatchupRetriesOnRead(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	writeReloadGeminiSession(t, root, "retry")
	path := filepath.Join(root, "tmp", "project", "chats", "session-retry.json")
	require.NoError(t, os.WriteFile(path, []byte("{broken json"), 0o644))
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, nil)
	t.Cleanup(ingestion.Stop)
	ingestion.Suspend()
	ingestion.applying.Wait()
	assert.False(t, ingestion.EnsureFresh(t.Context()))
	assert.True(t, ingestion.Pending())
	writeReloadGeminiSession(t, root, "retry")
	require.True(t, ingestion.EnsureFresh(t.Context()))
	assert.True(t, sessionImported(t, database, "gemini:retry"))
}
