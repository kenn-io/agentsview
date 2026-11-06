package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	client, err := apiclient.NewHTTPClient(endpoint, "", http.DefaultClient)
	require.NoError(t, err)
	response, err := client.GetAPIV1SearchWithResponse(t.Context(), &apiclient.GetAPIV1SearchRequestOptions{Query: &apiclient.GetAPIV1SearchQuery{Q: query}})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode, string(response.Body))
	return response.HTTPResponse.Header, string(response.Body)
}

func serveIngestionArchive(t *testing.T, cfg config.Config, database *db.DB, engine *agentsync.Engine, ingestion *daemonIngestion, options ...server.Option) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	cfg.Port = ts.Listener.Addr().(*net.TCPAddr).Port
	options = append(options, server.WithBaseContext(t.Context()))
	if ingestion != nil {
		options = append(options, server.WithIngestionFreshness(ingestion))
	}
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

type ingestionLifecycleFixture struct {
	cfg       config.Config
	database  *db.DB
	root      string
	engine    *agentsync.Engine
	tracker   *server.IdleTracker
	ingestion *daemonIngestion
}

func newIngestionLifecycleFixture(t *testing.T) ingestionLifecycleFixture {
	t.Helper()
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
	return ingestionLifecycleFixture{cfg, database, root, engine, tracker, ingestion}
}

func TestDaemonIngestionSharedCatchupPreservesLifecycle(t *testing.T) {
	f := newIngestionLifecycleFixture(t)
	f.ingestion.OpenWatcherDispatch()
	suspended := make(chan bool, 4)
	f.tracker.SetSyncIdle(time.Second, func() {
		f.ingestion.Suspend()
		suspended <- f.ingestion.Active()
	})
	trackerCtx, cancel := context.WithCancel(t.Context())
	trackerDone := make(chan struct{})
	go func() { defer close(trackerDone); f.tracker.Run(trackerCtx) }()
	stopTracker := sync.OnceFunc(func() { cancel(); <-trackerDone })
	defer stopTracker()
	waitForSuspension := func(wantActive bool) {
		timeout := time.NewTimer(20 * time.Second)
		defer timeout.Stop()
		for {
			select {
			case active := <-suspended:
				if active == wantActive {
					return
				}
			case <-timeout.C:
				require.FailNow(t, "idle tracker did not reach the expected collection state")
			}
		}
	}
	waitForSuspension(false)
	f.ingestion.applying.Wait()

	lastSync := time.Now().Add(-time.Minute)
	require.NoError(t, f.database.SetSyncState(t.Context(), "last_sync_started_at", lastSync.Format(time.RFC3339Nano)))
	writeReloadGeminiSession(t, f.root, "overnight")
	writeReloadGeminiSession(t, f.root, "old")
	oldPath := filepath.Join(f.root, "tmp", "project", "chats", "session-old.json")
	oldTime := lastSync.Add(-time.Hour)
	require.NoError(t, os.Chtimes(oldPath, oldTime, oldTime))
	assert.Never(t, func() bool { return sessionImported(t, f.database, "gemini:overnight") }, time.Second, 50*time.Millisecond, "quiet ingestion leaves the new session on disk")
	require.False(t, f.ingestion.Active(), "the idle tracker suspends collection")

	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer func() { releaseOnce(); <-finished }()
	go func() {
		defer close(finished)
		_ = f.engine.RunExclusive(func() error { close(entered); <-release; return nil })
	}()
	<-entered
	ts := serveIngestionArchive(t, f.cfg, f.database, f.engine, f.ingestion, server.WithIdleTracker(f.tracker))
	headers, _ := searchIngestionArchive(t, ts.URL, "overnight")
	assert.Equal(t, "true", headers.Get(apiclient.ArchiveStaleHeader))
	require.True(t, f.ingestion.Active(), "watching resumes before catch-up finishes")
	start := time.Now()
	headers, _ = searchIngestionArchive(t, ts.URL, "overnight")
	assert.Equal(t, "true", headers.Get(apiclient.ArchiveStaleHeader))
	assert.Less(t, time.Since(start), ingestionWaitBudget, "later readers reuse the expired budget instead of starting another wait")
	waitForSuspension(true)
	assert.True(t, f.ingestion.Active(), "an idle deadline during catch-up keeps collection alive")
	assert.True(t, f.ingestion.Pending(), "the blocked catch-up still owes an archive refresh")
	releaseOnce()
	<-finished
	waitForSuspension(false)
	stopTracker()
	f.ingestion.applying.Wait()
	require.False(t, f.ingestion.Active(), "catch-up completed after the idle deadline, so collection must stop without needing another request")
	require.True(t, sessionImported(t, f.database, "gemini:overnight"))
	assert.True(t, sessionImported(t, f.database, "gemini:old"), "wake imports copied files with preserved old modification times")
}

func TestDaemonIngestionReloadWhileSuspended(t *testing.T) {
	f := newIngestionLifecycleFixture(t)
	f.ingestion.OpenWatcherDispatch()
	f.ingestion.Suspend()
	f.ingestion.applying.Wait()
	writeReloadGeminiSession(t, f.root, "overnight")
	_, err := f.ingestion.Reload(t.Context())
	require.NoError(t, err)
	f.ingestion.applying.Wait()
	assert.Equal(t, []parser.AgentType{parser.AgentGemini}, f.ingestion.Config().DisabledAgents)
	assert.False(t, f.ingestion.Active(), "reloading settings while suspended leaves collection suspended")
	require.True(t, f.ingestion.EnsureFresh(t.Context()))
	f.ingestion.applying.Wait()
	assert.False(t, sessionImported(t, f.database, "gemini:overnight"), "catch-up uses the newly disabled provider settings")
}

func TestDaemonIngestionStopWaitsForCatchup(t *testing.T) {
	f := newIngestionLifecycleFixture(t)
	f.ingestion.Suspend()
	f.ingestion.applying.Wait()
	writeReloadGeminiSession(t, f.root, "overnight")
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	defer func() { releaseOnce(); <-finished }()
	go func() {
		defer close(finished)
		_ = f.engine.RunExclusive(func() error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	readerDone := make(chan struct{})
	go func() { f.ingestion.EnsureFresh(ctx); close(readerDone) }()
	require.Eventually(t, f.ingestion.Active, time.Second, time.Millisecond, "catch-up has restored collection and is waiting for the engine")
	stop()
	<-readerDone
	stopped := make(chan struct{})
	go func() { f.ingestion.Stop(); close(stopped) }()
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
	require.Eventually(t, func() bool { return !f.ingestion.Pending() }, 5*time.Second, time.Millisecond)
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

func TestActiveManualSyncDoesNotWaitForReloadOrReconcile(t *testing.T) {
	f := newIngestionLifecycleFixture(t)
	require.True(t, f.ingestion.Active())
	// Dispatch stays closed so only an unwanted reconcile could mark this source missing.
	require.NoError(t, os.Remove(filepath.Join(f.root, "tmp", "project", "chats", "session-retained.json")))
	var calls atomic.Int64
	runner := f.ingestion.foregroundSync(func(context.Context, func(agentsync.Progress)) (agentsync.SyncStats, error) {
		calls.Add(1)
		return agentsync.SyncStats{}, nil
	})
	f.ingestion.applyMu.Lock()
	done := make(chan struct{})
	defer func() {
		f.ingestion.applyMu.Unlock()
		<-done
	}()
	var runErr error
	go func() {
		defer close(done)
		_, runErr = runner(t.Context(), nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "active manual sync waited for the provider reload lock")
	}
	require.NoError(t, runErr)
	assert.Equal(t, int64(1), calls.Load())
	after, err := f.database.GetSessionFull(t.Context(), "gemini:retained")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Nil(t, after.SourceMissingAt, "active manual sync leaves source reconciliation to the runner")
}

func TestArchiveWriteRoutesHandleIdleCollection(t *testing.T) {
	for _, tc := range []struct{ name, route string }{{"resync", "/resync"}, {"startup-only", "/sync?startup_only=true"}, {"nil receiver pass-through", "/sync"}} {
		t.Run(tc.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			writeReloadGeminiSession(t, root, "before")
			cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			engine.SyncAll(t.Context(), nil)
			var ingestion *daemonIngestion
			if tc.name != "nil receiver pass-through" {
				ingestion = newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
				t.Cleanup(ingestion.Stop)
				ingestion.Suspend()
				ingestion.applying.Wait()
			}
			writeReloadGeminiSession(t, root, "overnight")
			var syncCalls, resyncCalls atomic.Int64
			syncRunner := newForegroundSyncRunner(t.Context(), cfg, engine, database, nil)
			resyncRunner := newForegroundResyncRunner(t.Context(), cfg, engine, database)
			ts := serveIngestionArchive(t, cfg, database, engine, ingestion,
				server.WithLocalSyncRunner(ingestion.foregroundSync(withAttachedSources(nil, func(ctx context.Context, progress func(agentsync.Progress)) (agentsync.SyncStats, error) {
					syncCalls.Add(1)
					return syncRunner(ctx, progress)
				}))),
				server.WithLocalResyncRunner(server.LocalResyncRunner(ingestion.foregroundSync(withAttachedSources(nil, func(ctx context.Context, progress func(agentsync.Progress)) (agentsync.SyncStats, error) {
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
			if tc.name == "nil receiver pass-through" {
				assert.Equal(t, int64(1), syncCalls.Load())
				return
			}
			assert.True(t, sessionImported(t, database, "gemini:overnight"))
			assert.True(t, ingestion.Active(), "archive requests resume collection")
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
