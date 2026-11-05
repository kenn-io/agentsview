package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
			var watchAttempts, wakePasses atomic.Int32
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
			ingestion.catchupImports = func(context.Context) error { wakePasses.Add(1); return nil }
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
				beforeWatch, beforeWake := watchAttempts.Load(), wakePasses.Load()
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
				wakes := wakePasses.Load() - beforeWake
				if mode.idle {
					assert.EqualValues(t, 1, wakes)
				} else {
					assert.Zero(t, wakes)
				}
				t.Logf("idle=%t write=%s quiet watcher sync attempts=%d wake reconciliations=%d", mode.idle, id, quietAttempts, wakes)
			}
		})
	}
}

func TestDaemonIngestionCatchupWaitersAndRetry(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, nil)
	t.Cleanup(ingestion.Stop)
	ingestion.OpenWatcherDispatch()
	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	var calls atomic.Int32
	ingestion.catchupImports = func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
			}
			return errors.New("import unavailable")
		}
		return nil
	}
	ingestion.Suspend()
	ingestion.applying.Wait()
	writeReloadGeminiSession(t, root, "waiters")
	canceledCtx, cancel := context.WithCancel(t.Context())
	first := make(chan bool, 1)
	go func() { first <- ingestion.EnsureFresh(canceledCtx) }()
	<-entered
	cancel()
	assert.False(t, <-first)
	srv := server.New(cfg, database, engine, server.WithIngestionFreshness(ingestion))
	handler := srv.Handler()
	status := httptest.NewRecorder()
	handler.ServeHTTP(status, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:0/api/v1/sync/status", nil))
	assert.Equal(t, http.StatusOK, status.Code)
	assert.Contains(t, status.Body.String(), `"sync_pending":true`)
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/missing", nil))
	assert.Equal(t, http.StatusNotFound, missing.Code)
	assert.Equal(t, "true", missing.Header().Get(apiclient.ArchiveStaleHeader))
	assert.EqualValues(t, 1, calls.Load(), "waiters share the import pass")
	ingestion.mu.Lock()
	done := ingestion.generation.attempt
	ingestion.mu.Unlock()
	releaseOnce()
	<-done
	assert.True(t, ingestion.Pending(), "failure retains the catch-up obligation")
	assert.False(t, ingestion.EnsureFresh(t.Context()), "cooldown does not start another attempt")
	require.Eventually(t, func() bool { return !ingestion.Pending() }, 15*time.Second, 50*time.Millisecond)
	assert.EqualValues(t, 2, calls.Load())
	assert.True(t, sessionImported(t, database, "gemini:waiters"))
	entered = make(chan struct{})
	ingestion.catchupImports = func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }
	ingestion.Suspend()
	result := make(chan bool, 1)
	go func() { result <- ingestion.EnsureFresh(t.Context()) }()
	<-entered
	ingestion.Stop()
	assert.False(t, <-result)
}

func TestDaemonIngestionPausedReloadAndMissingRoot(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	base := t.TempDir()
	root, alternate := filepath.Join(base, "primary"), filepath.Join(base, "alternate")
	writeReloadGeminiSession(t, root, "retained")
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root, filepath.Join(base, "unused")}}}
	saved := cfg
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, func() (config.Config, error) { return saved, nil })
	t.Cleanup(ingestion.Stop)
	ingestion.OpenWatcherDispatch()
	ingestion.Suspend()
	require.True(t, ingestion.EnsureFresh(t.Context()))
	ingestion.Suspend()
	ingestion.applying.Wait()
	writeReloadGeminiSession(t, root, "missed-offline")
	require.NoError(t, os.Rename(root, root+"-offline"))
	srv := httptest.NewUnstartedServer(nil)
	cfg.Host = "127.0.0.1"
	cfg.Port = srv.Listener.Addr().(*net.TCPAddr).Port
	srv.Config.Handler = server.New(cfg, database, engine, server.WithIngestionFreshness(ingestion)).Handler()
	srv.Start()
	t.Cleanup(srv.Close)
	headers, body := searchIngestionArchive(t, srv.URL, "missed-offline")
	assert.NotContains(t, body, "hello from missed-offline")
	assert.Equal(t, "true", headers.Get(apiclient.ArchiveStaleHeader))
	assert.True(t, ingestion.Pending())
	assert.True(t, sessionImported(t, database, "gemini:retained"), "an unavailable root preserves archived sessions")
	for range 5 {
		assert.False(t, ingestion.EnsureFresh(t.Context()))
	}
	require.NoError(t, os.Rename(root+"-offline", root))
	require.Eventually(t, func() bool { return !ingestion.Pending() }, 15*time.Second, 50*time.Millisecond)
	headers, body = searchIngestionArchive(t, srv.URL, "missed-offline")
	assert.Contains(t, body, "hello from missed-offline")
	assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader))
	ingestion.Suspend()
	ingestion.applying.Wait()
	saved.AgentDirs = map[parser.AgentType][]string{parser.AgentGemini: {root, alternate}}
	writeReloadGeminiSession(t, alternate, "enabled")
	_, err := ingestion.Reload(t.Context())
	require.NoError(t, err)
	ingestion.applying.Wait()
	assert.True(t, ingestion.Pending())
	assert.False(t, sessionImported(t, database, "gemini:enabled"))
	require.NoError(t, os.Remove(filepath.Join(root, "tmp", "project", "chats", "session-retained.json")))
	require.True(t, ingestion.EnsureFresh(t.Context()))
	assert.True(t, sessionImported(t, database, "gemini:enabled"))
	stored, err := database.GetSessionFull(t.Context(), "gemini:retained")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.NotNil(t, stored.SourceMissingAt, "an authoritative wake records the deleted source")
}

func TestDaemonIngestionRemoteFailureKeepsWarningUntilRetry(t *testing.T) {
	var denied atomic.Bool
	var listings atomic.Int32
	var available atomic.Bool
	remote := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "remote catchup transcript", "remote-session").String()
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bucket/machine/raw/claude/proj/remote-session.jsonl" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(remote)))
			w.Header().Set("Last-Modified", "Thu, 01 Jan 2026 00:00:00 GMT")
			_, _ = io.WriteString(w, remote)
			return
		}
		listings.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		if denied.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Access denied</Message></Error>`)
			return
		}
		if available.Load() {
			_, _ = fmt.Fprintf(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>bucket</Name><IsTruncated>false</IsTruncated><Contents><Key>machine/raw/claude/proj/remote-session.jsonl</Key><LastModified>2026-01-01T00:00:00Z</LastModified><ETag>"remote"</ETag><Size>%d</Size></Contents></ListBucketResult>`, len(remote))
			return
		}
		_, _ = io.WriteString(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>bucket</Name><IsTruncated>false</IsTruncated></ListBucketResult>`)
	}))
	t.Cleanup(s3.Close)
	t.Setenv("AWS_S3_ENDPOINT", s3.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	t.Setenv("AWS_REGION", "us-east-1")
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	cfg := config.Config{Host: "127.0.0.1", DataDir: t.TempDir(), AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}, parser.AgentClaude: {"s3://bucket/machine/raw/claude"}}}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	initial := engine.SyncAll(t.Context(), nil)
	require.True(t, initial.ProcessingComplete())
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, nil)
	t.Cleanup(ingestion.Stop)
	ingestion.catchupImports = func(ctx context.Context) error {
		return runRemoteSourceSyncPass(ctx, engine, remoteSourceSyncRoots(cfg))
	}
	ingestion.OpenWatcherDispatch()
	ts := httptest.NewUnstartedServer(nil)
	cfg.Port = ts.Listener.Addr().(*net.TCPAddr).Port
	ts.Config.Handler = server.New(cfg, database, engine, server.WithIngestionFreshness(ingestion)).Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	endpoint := serverEndpoint(t, ts)
	writeDaemonRuntimeForTest(t, cfg.DataDir, endpoint.Host, endpoint.Port, version, false)
	ingestion.Suspend()
	ingestion.applying.Wait()
	denied.Store(true)
	available.Store(true)
	writeReloadGeminiSession(t, root, "during-quiet")
	quietListings := listings.Load()
	assert.Never(t, func() bool { return listings.Load() != quietListings }, time.Second, 50*time.Millisecond, "suspended remote imports perform no listing")
	headers, body := searchIngestionArchive(t, ts.URL, "during-quiet")
	assert.Contains(t, body, "hello from during-quiet")
	assert.Equal(t, "true", headers.Get(apiclient.ArchiveStaleHeader))
	assert.True(t, ingestion.Pending())
	cooldownListings := listings.Load()
	for range 20 {
		assert.False(t, ingestion.EnsureFresh(t.Context()))
	}
	assert.Equal(t, cooldownListings, listings.Load(), "searches share the failed attempt and its cooldown")
	warning := captureStderr(t, func() { ensureFreshData(t.Context(), cfg, database, false) })
	assert.Contains(t, warning, apiclient.ArchiveStaleWarning)
	writeReloadGeminiSession(t, root, "while-remote-offline")
	require.Eventually(t, func() bool { return sessionImported(t, database, "gemini:while-remote-offline") }, 8*time.Second, 50*time.Millisecond, "local watching continues while the remote source remains unavailable")
	attempts := listings.Load()
	denied.Store(false)
	require.Eventually(t, func() bool { return !ingestion.Pending() }, 20*time.Second, 50*time.Millisecond)
	assert.Greater(t, listings.Load(), attempts)
	assert.False(t, ingestion.Pending())
	headers, body = searchIngestionArchive(t, ts.URL, "remote catchup transcript")
	assert.Contains(t, body, "remote catchup transcript")
	assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader))
	warning = captureStderr(t, func() { ensureFreshData(t.Context(), cfg, database, false) })
	assert.NotContains(t, warning, apiclient.ArchiveStaleWarning)
}

func TestDaemonIngestionSlowCatchupKeepsWatching(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	writeReloadGeminiSession(t, root, "before")
	cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	tracker := server.NewIdleTracker(0, func() {})
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, tracker, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	signalEntered := sync.OnceFunc(func() { close(entered) })
	ingestion.catchupImports = func(ctx context.Context) error {
		signalEntered()
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.Cleanup(ingestion.Stop)
	t.Cleanup(releaseOnce)
	tracker.SetSyncIdle(time.Second, ingestion.Suspend)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go tracker.Run(ctx)
	ingestion.Suspend()
	ingestion.applying.Wait()
	ts := httptest.NewUnstartedServer(nil)
	cfg.Port = ts.Listener.Addr().(*net.TCPAddr).Port
	ts.Config.Handler = server.New(cfg, database, engine, server.WithIngestionFreshness(ingestion), server.WithIdleTracker(tracker)).Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	headers, _ := searchIngestionArchive(t, ts.URL, "slow")
	<-entered
	assert.Equal(t, "true", headers.Get(apiclient.ArchiveStaleHeader))
	assert.Never(t, func() bool { return ingestion.ActiveContext() == nil }, 2*time.Second, 50*time.Millisecond, "the slow import holds collection through the idle deadline")
	writeReloadGeminiSession(t, root, "slow")
	require.Eventually(t, func() bool { return sessionImported(t, database, "gemini:slow") }, 8*time.Second, 50*time.Millisecond)
	releaseOnce()
	headers, body := searchIngestionArchive(t, ts.URL, "slow")
	assert.Contains(t, body, "hello from slow")
	assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader))
}

func TestDaemonIngestionCanceledGenerationCannotAcknowledgeWake(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {t.TempDir()}}}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs})
	t.Cleanup(engine.Close)
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, nil, nil)
	t.Cleanup(ingestion.Stop)
	entered := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	ingestion.catchupImports = func(ctx context.Context) error {
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ingestion.Suspend()
	first := make(chan bool, 1)
	go func() { first <- ingestion.EnsureFresh(t.Context()) }()
	<-entered
	ingestion.Suspend()
	assert.False(t, <-first)
	second := make(chan bool, 1)
	go func() { second <- ingestion.EnsureFresh(t.Context()) }()
	<-entered
	assert.True(t, ingestion.Pending(), "completion from the canceled generation retains the new wake")
	release <- struct{}{}
	assert.True(t, <-second)
}
