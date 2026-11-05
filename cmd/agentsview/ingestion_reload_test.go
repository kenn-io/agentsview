package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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

func serveIngestionArchive(t *testing.T, cfg config.Config, database *db.DB, engine *agentsync.Engine, ingestion *daemonIngestion, options ...server.Option) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	cfg.Port = ts.Listener.Addr().(*net.TCPAddr).Port
	options = append(options, server.WithBaseContext(t.Context()), server.WithIngestionFreshness(ingestion))
	ts.Config.Handler = server.New(cfg, database, engine, options...).Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	return ts
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

func TestDaemonIngestionSharedCatchupPreservesLifecycle(t *testing.T) {
	for _, mode := range []string{"automatic", "reload", "ensure-fresh-stop"} {
		t.Run(mode, func(t *testing.T) {
			reload := mode == "reload"
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			writeReloadGeminiSession(t, root, "retained")
			cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			engine.SyncAll(t.Context(), nil)
			saved := cfg
			saved.DisabledAgents = []parser.AgentType{parser.AgentGemini}
			tracker := server.NewIdleTracker(0, func() {})
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, tracker, func() (config.Config, error) { return saved, nil })
			t.Cleanup(ingestion.Stop)
			if mode == "automatic" {
				tracker.SetSyncIdle(100*time.Millisecond, ingestion.Suspend) //nolint:kennlint // deadlinetest: absence check; the engine lease keeps catch-up blocked beyond the idle deadline
				trackerCtx, cancel := context.WithCancel(t.Context())
				trackerDone := make(chan struct{})
				go func() { defer close(trackerDone); tracker.Run(trackerCtx) }()
				defer func() { cancel(); <-trackerDone }()
			}
			if mode == "automatic" {
				require.Eventually(t, func() bool { return !ingestion.Active() }, time.Second, 10*time.Millisecond, "the idle tracker suspends collection")
			} else {
				ingestion.Suspend()
			}
			ingestion.applying.Wait()
			writeReloadGeminiSession(t, root, "overnight")
			if mode == "automatic" {
				path := filepath.Join(root, "tmp", "project", "chats", "session-overnight.json")
				older := time.Now().Add(-time.Hour)
				require.NoError(t, os.Chtimes(path, older, older))
				assert.Never(t, func() bool { return sessionImported(t, database, "gemini:overnight") }, time.Second, 50*time.Millisecond, "quiet ingestion leaves the new session on disk")
			}
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			releaseOnce := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseOnce)
			go func() {
				defer close(finished)
				_ = engine.RunExclusive(func() error { close(entered); <-release; return nil })
			}()
			<-entered
			if reload {
				_, err := ingestion.Reload(t.Context())
				require.NoError(t, err)
				require.Eventually(t, func() bool { return len(ingestion.Config().DisabledAgents) == 1 }, time.Second, time.Millisecond)
				releaseOnce()
				<-finished
				require.True(t, ingestion.EnsureFresh(t.Context()))
				ingestion.applying.Wait()
				assert.False(t, sessionImported(t, database, "gemini:overnight"), "catch-up uses the newly disabled provider settings")
				return
			}
			if mode == "ensure-fresh-stop" {
				ctx, stop := context.WithCancel(t.Context())
				readerDone := make(chan struct{})
				go func() { ingestion.EnsureFresh(ctx); close(readerDone) }()
				require.Eventually(t, ingestion.Active, time.Second, time.Millisecond, "catch-up has restored collection and is waiting for the engine")
				stop()
				<-readerDone
				stopped := make(chan struct{})
				go func() { ingestion.Stop(); close(stopped) }()
				assert.Never(t, func() bool {
					select {
					case <-stopped:
						return true
					default:
						return false
					}
				}, 100*time.Millisecond, time.Millisecond, "Stop must retain the archive until catch-up leaves the engine")
				releaseOnce()
				<-finished
				<-stopped
				return
			}
			ts := serveIngestionArchive(t, cfg, database, engine, ingestion, server.WithIdleTracker(tracker))
			headers, _ := searchIngestionArchive(t, ts.URL, "overnight")
			assert.Equal(t, "true", headers.Get(apiclient.ArchiveStaleHeader))
			require.True(t, ingestion.Active(), "watching resumes before catch-up finishes")
			canceled, stop := context.WithCancel(t.Context())
			stop()
			assert.False(t, ingestion.EnsureFresh(canceled), "one canceled reader leaves shared work running")
			releaseOnce()
			<-finished
			require.True(t, ingestion.EnsureFresh(t.Context()))
			headers, freshBody := searchIngestionArchive(t, ts.URL, "overnight")
			assert.Contains(t, freshBody, "hello from overnight")
			assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader))
		})
	}
}

func TestDaemonIngestionWakeKeepsStartupDispatchClosed(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	writeReloadGeminiSession(t, root, "before")
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
	t.Cleanup(ingestion.Stop)
	ingestion.Suspend()
	ingestion.applying.Wait()
	require.True(t, ingestion.EnsureFresh(t.Context()))
	writeReloadGeminiSession(t, root, "before-startup-ack")
	assert.Never(t, func() bool { return sessionImported(t, database, "gemini:before-startup-ack") }, time.Second, 20*time.Millisecond, "watch events wait for startup acknowledgment after wake too")
	ingestion.OpenWatcherDispatch()
	require.Eventually(t, func() bool { return sessionImported(t, database, "gemini:before-startup-ack") }, 5*time.Second, 20*time.Millisecond)
}

func TestArchiveWriteRoutesWakeIdleCollection(t *testing.T) {
	for _, tc := range []struct{ name, route string }{{"resync", "/resync"}, {"startup-only", "/sync?startup_only=true"}} {
		t.Run(tc.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			writeReloadGeminiSession(t, root, "before")
			cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			engine.SyncAll(t.Context(), nil)
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
			t.Cleanup(ingestion.Stop)
			ingestion.Suspend()
			ingestion.applying.Wait()
			writeReloadGeminiSession(t, root, "overnight")
			var syncCalls, resyncCalls atomic.Int64
			syncRunner := newForegroundSyncRunner(t.Context(), cfg, engine, database, nil)
			resyncRunner := newForegroundResyncRunner(t.Context(), cfg, engine, database)
			ts := serveIngestionArchive(t, cfg, database, engine, ingestion,
				server.WithLocalSyncRunner(ingestion.foregroundSync(withAttachedSources(ingestion.attached, func(ctx context.Context, progress func(agentsync.Progress)) (agentsync.SyncStats, error) {
					syncCalls.Add(1)
					return syncRunner(ctx, progress)
				}))),
				server.WithLocalResyncRunner(server.LocalResyncRunner(ingestion.foregroundSync(withAttachedSources(ingestion.attached, func(ctx context.Context, progress func(agentsync.Progress)) (agentsync.SyncStats, error) {
					resyncCalls.Add(1)
					return resyncRunner(ctx, progress)
				})))),
			)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1"+tc.route, nil)
			require.NoError(t, err)
			request.Header.Set("Origin", ts.URL)
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, response.Body.Close())
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode, string(body))
			assert.True(t, ingestion.Active(), "archive requests resume collection")
			assert.True(t, sessionImported(t, database, "gemini:overnight"))
			assert.Zero(t, syncCalls.Load(), "startup-only reads use middleware catch-up")
			if tc.name == "resync" {
				assert.Equal(t, int64(1), resyncCalls.Load())
			} else {
				assert.Zero(t, resyncCalls.Load())
			}
			assert.False(t, ingestion.Pending())
		})
	}
}
