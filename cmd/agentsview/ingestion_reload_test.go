package main

import (
	"context"
	"fmt"
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

func serveIngestionArchive(t *testing.T, cfg config.Config, database *db.DB, engine *agentsync.Engine, ingestion *daemonIngestion) *httptest.Server {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	cfg.Port = ts.Listener.Addr().(*net.TCPAddr).Port
	ts.Config.Handler = server.New(cfg, database, engine, server.WithBaseContext(t.Context()), server.WithIngestionFreshness(ingestion)).Handler()
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

func TestDaemonIngestionIdleSearchAndCLIRead(t *testing.T) {
	for _, mode := range []struct{ cli, idle bool }{{false, true}, {true, true}, {false, false}} {
		t.Run(fmt.Sprintf("cli=%t/idle=%t", mode.cli, mode.idle), func(t *testing.T) {
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
				writeReloadGeminiSession(t, root, id)
				if mode.idle {
					assert.Never(t, func() bool { return sessionImported(t, database, "gemini:"+id) }, time.Second, 50*time.Millisecond, "quiet ingestion leaves the new session on disk")
				} else {
					require.Eventually(t, func() bool { return sessionImported(t, database, "gemini:"+id) }, 10*time.Second, 50*time.Millisecond)
				}
				if mode.cli {
					backend, closeBackend, err := resolveArchiveQueryBackendWithConfig(t.Context(), cfg, defaultArchiveQueryPolicy(nil))
					require.NoError(t, err)
					require.IsType(t, daemonArchiveQueryBackend{}, backend)
					_, err = backend.DailyUsage(t.Context(), dailyUsageQuery{NoDefaultRange: true})
					closeBackend()
					require.NoError(t, err)
					require.True(t, sessionImported(t, database, "gemini:"+id))
				}
				headers, body := searchIngestionArchive(t, ts.URL, id)
				assert.Contains(t, body, "hello from "+id)
				assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader))
				assert.False(t, ingestion.Pending())
			}
		})
	}
}

func TestDaemonIngestionSharedCatchupKeepsWaitActiveBlocked(t *testing.T) {
	for _, mode := range []string{"automatic", "reload", "requested", "failed", "canceled"} {
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
				go tracker.Run(t.Context())
			}
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
			requested := make(chan error, 1)
			requestedCtx, cancelRequested := context.WithCancel(t.Context())
			defer cancelRequested()
			if mode == "requested" || mode == "failed" || mode == "canceled" {
				registered := make(chan struct{})
				go func() {
					requested <- ingestion.RunSync(requestedCtx, func(ctx context.Context) error {
						close(registered)
						<-release
						if mode == "failed" {
							return os.ErrPermission
						}
						if err := ctx.Err(); err != nil {
							return err
						}
						engine.SyncAll(ctx, nil)
						return nil
					})
				}()
				<-registered
			}
			query := "overnight"
			if reload {
				_, err := ingestion.Reload(t.Context())
				require.NoError(t, err)
				require.Eventually(t, func() bool { return len(ingestion.Config().DisabledAgents) == 1 }, time.Second, time.Millisecond)
				query = "retained"
			}
			active := make(chan bool, 1)
			go func() { active <- ingestion.WaitActive(t.Context()) }()
			ts := httptest.NewUnstartedServer(nil)
			cfg.Port = ts.Listener.Addr().(*net.TCPAddr).Port
			ts.Config.Handler = server.New(cfg, database, engine, server.WithIngestionFreshness(ingestion)).Handler()
			ts.Start()
			t.Cleanup(ts.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/search?q="+query, nil)
			require.NoError(t, err)
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err, "archive reads retain their two-second catch-up budget")
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode, string(body))
			assert.Equal(t, "true", response.Header.Get(apiclient.ArchiveStaleHeader))
			if reload {
				assert.Contains(t, string(body), "hello from retained")
			} else {
				require.True(t, ingestion.Active(), "watching resumes before catch-up finishes")
			}
			assert.Empty(t, active, "WaitActive waits for catch-up, not just watcher setup")
			require.ErrorIs(t, ingestion.RunSync(t.Context(), func(context.Context) error { return nil }), agentsync.ErrSyncInProgress)
			canceled, stop := context.WithCancel(t.Context())
			stop()
			assert.False(t, ingestion.EnsureFresh(canceled), "one canceled reader leaves shared work running")
			if mode == "canceled" {
				cancelRequested()
			}
			releaseOnce()
			<-finished
			switch mode {
			case "requested":
				require.NoError(t, <-requested)
			case "failed", "canceled":
				require.Error(t, <-requested)
				require.True(t, ingestion.Pending(), "an unsuccessful requested pass leaves catch-up pending")
			}
			require.True(t, ingestion.EnsureFresh(t.Context()))
			headers, freshBody := searchIngestionArchive(t, ts.URL, query)
			assert.Contains(t, freshBody, "hello from "+query)
			assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader))
			require.True(t, <-active)
			if mode == "automatic" {
				require.Eventually(t, func() bool { return !ingestion.Active() }, time.Second, 10*time.Millisecond, "collection suspends again after background catch-up")
			}
		})
	}
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
	// Recovery shares the engine's cached terminal parse-failure policy.
	require.True(t, ingestion.EnsureFresh(t.Context()))
	assert.False(t, sessionImported(t, database, "gemini:retry"))
	ingestion.Suspend()
	ingestion.applying.Wait()
	writeReloadGeminiSession(t, root, "retry")
	require.True(t, ingestion.EnsureFresh(t.Context()))
	assert.True(t, sessionImported(t, database, "gemini:retry"))
}

func TestDaemonIngestionStopWaitsForCatchup(t *testing.T) {
	for _, requested := range []bool{false, true} {
		t.Run(fmt.Sprintf("requested=%v", requested), func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, nil)
			stop := sync.OnceFunc(ingestion.Stop)
			t.Cleanup(stop)
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
			pass := make(chan error, 1)
			if requested {
				registered := make(chan struct{})
				go func() {
					pass <- ingestion.RunSync(t.Context(), func(ctx context.Context) error {
						close(registered)
						engine.SyncAll(ctx, nil)
						return nil
					})
				}()
				<-registered
			} else {
				require.False(t, ingestion.EnsureFresh(t.Context()))
			}
			ingestion.mu.Lock()
			catchupDone := ingestion.catchup
			ingestion.mu.Unlock()
			stopped := make(chan struct{})
			go func() { stop(); close(stopped) }()
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
			<-catchupDone
			<-stopped
			if requested {
				require.NoError(t, <-pass)
			}
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
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, nil)
	t.Cleanup(ingestion.Stop)
	ingestion.Suspend()
	ingestion.applying.Wait()
	require.True(t, ingestion.EnsureFresh(t.Context()))
	writeReloadGeminiSession(t, root, "before-startup-ack")
	assert.Never(t, func() bool { return sessionImported(t, database, "gemini:before-startup-ack") }, time.Second, 20*time.Millisecond, "watch events wait for startup acknowledgment after wake too")
	ingestion.OpenWatcherDispatch()
	require.Eventually(t, func() bool { return sessionImported(t, database, "gemini:before-startup-ack") }, 5*time.Second, 20*time.Millisecond)
}

func TestDaemonIngestionWakeReconcilesOlderQueuedRetry(t *testing.T) {
	for _, linked := range []bool{false, true} {
		t.Run(fmt.Sprintf("symlink=%v", linked), func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			if linked {
				link := filepath.Join(t.TempDir(), "sessions")
				requireSymlinkOrSkip(t, root, link)
				root = link
			}
			writeReloadGeminiSession(t, root, "before")
			cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			engine.SyncAll(t.Context(), nil)
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, nil)
			t.Cleanup(ingestion.Stop)
			writeReloadGeminiSession(t, root, "older-retry")
			path := filepath.Join(root, "tmp", "project", "chats", "session-older-retry.json")
			older := time.Now().Add(-time.Hour)
			require.NoError(t, os.Chtimes(path, older, older))
			ingestion.QueueWatchRetry(agentsync.WatchBatch{Paths: []string{path}})
			ingestion.Suspend()
			ingestion.applying.Wait()
			require.False(t, sessionImported(t, database, "gemini:older-retry"))
			require.True(t, ingestion.EnsureFresh(t.Context()))
			require.True(t, sessionImported(t, database, "gemini:older-retry"), "catch-up must include the retry queued before the quiet cutoff")
		})
	}
}

func TestDaemonIngestionWakeKeepsLocalResultsDuringRemoteFailure(t *testing.T) {
	for _, local := range []bool{true, false} {
		t.Run(fmt.Sprintf("local=%v", local), func(t *testing.T) {
			var requests atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
			}))
			t.Cleanup(remote.Close)
			t.Setenv("AWS_S3_ENDPOINT", remote.URL)
			t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
			t.Setenv("AWS_REGION", "us-east-1")
			roots := []string{"s3://bucket/sessions"}
			root := t.TempDir()
			if local {
				roots = append(roots, root)
				dbtest.WriteTestFile(t, filepath.Join(root, "project", "before.jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "hello from before", "before").String()))
			}
			cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: roots}}
			database := dbtest.OpenTestDB(t)
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, nil)
			t.Cleanup(ingestion.Stop)
			ingestion.Suspend()
			ingestion.applying.Wait()
			if local {
				dbtest.WriteTestFile(t, filepath.Join(root, "project", "morning.jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-02T00:00:00Z", "hello from morning", "morning").String()))
			}
			ts := serveIngestionArchive(t, cfg, database, engine, ingestion)
			headers, body := searchIngestionArchive(t, ts.URL, "morning")
			if local {
				assert.Contains(t, body, "hello from morning")
			}
			assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader))
			assert.Positive(t, requests.Load(), "idle wake also attempts the paused remote imports")
			assert.False(t, ingestion.Pending())
		})
	}
}

func TestDaemonIngestionWakeCoversAvailableOverlappingProviders(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("nested=%v", nested), func(t *testing.T) {
			root := t.TempDir()
			gemini := root
			if nested {
				gemini = filepath.Join(root, "nested")
			}
			writeReloadGeminiSession(t, gemini, "retained")
			cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{
				parser.AgentClaude: {root}, parser.AgentGemini: {gemini},
			}}
			database := dbtest.OpenTestDB(t)
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			engine.SyncAll(t.Context(), nil)
			require.True(t, sessionImported(t, database, "gemini:retained"))
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, nil)
			t.Cleanup(ingestion.Stop)
			ingestion.Suspend()
			ingestion.applying.Wait()
			require.NoError(t, os.RemoveAll(filepath.Join(gemini, "tmp")))
			dbtest.WriteTestFile(t, filepath.Join(root, "project", "morning.jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-02T00:00:00Z", "hello from morning", "morning").String()))
			ts := serveIngestionArchive(t, cfg, database, engine, ingestion)
			headers, body := searchIngestionArchive(t, ts.URL, "morning")
			assert.Contains(t, body, "hello from morning")
			assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader))
			assert.True(t, sessionImported(t, database, "gemini:retained"), "unavailable provider history stays archived")
		})
	}
}

type countingIngestionFactory struct {
	parser.ProviderFactory
	calls *atomic.Int32
}

func (f countingIngestionFactory) NewProvider(cfg parser.ProviderConfig) parser.Provider {
	provider := f.ProviderFactory.NewProvider(cfg)
	return countingIngestionProvider{Provider: provider, ReconciliationSourceResolver: provider.(parser.ReconciliationSourceResolver), calls: f.calls}
}

type countingIngestionProvider struct {
	parser.Provider
	parser.ReconciliationSourceResolver
	calls *atomic.Int32
}

func (p countingIngestionProvider) Discover(ctx context.Context) ([]parser.SourceRef, error) {
	p.calls.Add(1)
	return p.Provider.Discover(ctx)
}

func (p countingIngestionProvider) DiscoverEach(ctx context.Context, yield func(parser.SourceRef) error) error {
	p.calls.Add(1)
	return p.Provider.(parser.StreamingDiscoverer).DiscoverEach(ctx, yield)
}

func TestDaemonIngestionRequestedSyncOwnsCatchup(t *testing.T) {
	for _, route := range []string{"/sync", "/sync?wait=true", "/resync", "/sync?startup_only=true"} {
		t.Run(route, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			writeReloadGeminiSession(t, root, "before")
			cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
			var calls atomic.Int32
			var factory parser.ProviderFactory
			for _, candidate := range parser.ProviderFactories() {
				if candidate.Definition().Type == parser.AgentGemini {
					factory = candidate
				}
			}
			require.NotNil(t, factory)
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine", ProviderFactories: []parser.ProviderFactory{countingIngestionFactory{factory, &calls}}})
			t.Cleanup(engine.Close)
			engine.SyncAll(t.Context(), nil)
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, nil)
			t.Cleanup(ingestion.Stop)
			ingestion.Suspend()
			ingestion.applying.Wait()
			writeReloadGeminiSession(t, root, "morning")
			calls.Store(0)
			ts := serveIngestionArchive(t, cfg, database, engine, ingestion)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1"+route, nil)
			require.NoError(t, err)
			request.Header.Set("Origin", ts.URL)
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, response.Body.Close())
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode, string(body))
			if route == "/sync?startup_only=true" {
				assert.Zero(t, calls.Load())
				assert.True(t, ingestion.Pending())
				assert.False(t, sessionImported(t, database, "gemini:morning"))
				searchIngestionArchive(t, ts.URL, "morning")
			} else {
				assert.EqualValues(t, 1, calls.Load(), "the requested pass also satisfies idle catch-up")
				assert.Empty(t, response.Header.Get(apiclient.ArchiveStaleHeader), "completed sync must report current freshness")
			}
			assert.True(t, sessionImported(t, database, "gemini:morning"))
			assert.False(t, ingestion.Pending())
		})
	}
}
