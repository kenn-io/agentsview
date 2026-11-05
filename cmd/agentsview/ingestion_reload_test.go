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
				go tracker.Run(t.Context())
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
			query := "overnight"
			if reload {
				_, err := ingestion.Reload(t.Context())
				require.NoError(t, err)
				require.Eventually(t, func() bool { return len(ingestion.Config().DisabledAgents) == 1 }, time.Second, time.Millisecond)
				query = "retained"
			}
			active := make(chan bool, 1)
			go func() { active <- ingestion.WaitActive(t.Context()) }()
			ts := serveIngestionArchive(t, cfg, database, engine, ingestion, server.WithIdleTracker(tracker))
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
			canceled, stop := context.WithCancel(t.Context())
			stop()
			assert.False(t, ingestion.EnsureFresh(canceled), "one canceled reader leaves shared work running")
			if mode == "ensure-fresh-stop" {
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
			releaseOnce()
			<-finished
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

func TestDaemonIngestionWakeCatchesUpChangedSources(t *testing.T) {
	for _, source := range []string{"symlink", "overlapping providers", "remote"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
			var change func()
			switch source {
			case "symlink":
				link := filepath.Join(t.TempDir(), "sessions")
				requireSymlinkOrSkip(t, root, link)
				root = link
				cfg.AgentDirs[parser.AgentGemini] = []string{root}
				writeReloadGeminiSession(t, root, "before")
				change = func() { writeReloadGeminiSession(t, root, "older-retry") }
			case "overlapping providers":
				writeReloadGeminiSession(t, root, "retained")
				cfg.AgentDirs[parser.AgentClaude] = []string{root}
				change = func() {
					require.NoError(t, os.RemoveAll(filepath.Join(root, "tmp")))
					dbtest.WriteTestFile(t, filepath.Join(root, "project", "morning.jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-02T00:00:00Z", "hello from morning", "morning").String()))
				}
			case "remote":
				content := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-02T00:00:00Z", "overnight remote message", "remote-overnight").String()
				var published atomic.Bool
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/bucket" || r.URL.Path == "/bucket/" {
						w.Header().Set("Content-Type", "application/xml")
						contents := ""
						if published.Load() {
							contents = fmt.Sprintf("<Contents><Key>sessions/project/overnight.jsonl</Key><LastModified>2030-01-02T00:00:00.000Z</LastModified><ETag>&quot;abc123&quot;</ETag><Size>%d</Size><StorageClass>STANDARD</StorageClass></Contents>", len(content))
						}
						_, _ = fmt.Fprintf(w, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>bucket</Name><Prefix>sessions/</Prefix><KeyCount>1</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>%s</ListBucketResult>`, contents)
						return
					}
					if strings.HasSuffix(r.URL.Path, "overnight.jsonl") {
						w.Header().Set("Last-Modified", "Wed, 02 Jan 2030 00:00:00 GMT")
						w.Header().Set("ETag", `"abc123"`)
						w.Header().Set("Content-Length", strconv.Itoa(len(content)))
						_, _ = io.WriteString(w, content)
						return
					}
					http.NotFound(w, r)
				}))
				t.Cleanup(remote.Close)
				t.Setenv("AWS_S3_ENDPOINT", remote.URL)
				t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
				t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
				t.Setenv("AWS_REGION", "us-east-1")

				cfg.AgentDirs = map[parser.AgentType][]string{parser.AgentClaude: {"s3://bucket/sessions"}}
				change = func() { published.Store(true) }
			}
			database := dbtest.OpenTestDB(t)
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			engine.SyncAll(t.Context(), nil)
			if source == "overlapping providers" {
				require.True(t, sessionImported(t, database, "gemini:retained"))
			}
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
			t.Cleanup(ingestion.Stop)
			ingestion.Suspend()
			ingestion.applying.Wait()
			change()
			if source == "symlink" {
				require.False(t, sessionImported(t, database, "gemini:older-retry"))
			}
			require.True(t, ingestion.EnsureFresh(t.Context()))
			assert.False(t, ingestion.Pending())
			switch source {
			case "symlink":
				require.True(t, sessionImported(t, database, "gemini:older-retry"), "catch-up polls the symlinked source")
			case "overlapping providers":
				assert.True(t, sessionImported(t, database, "morning"))
				assert.True(t, sessionImported(t, database, "gemini:retained"), "unavailable provider history stays archived")
			case "remote":
				messages, err := database.GetAllMessages(t.Context(), "overnight")
				require.NoError(t, err)
				require.Len(t, messages, 1)
				assert.Equal(t, "overnight remote message", messages[0].Content)
			}
		})
	}
}

func TestArchiveWriteRoutesWakeIdleCollection(t *testing.T) {
	for _, tc := range []struct{ name, route string }{{"sync", "/sync"}, {"resync", "/resync"}, {"startup-only", "/sync?startup_only=true"}} {
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
			ts := serveIngestionArchive(t, cfg, database, engine, ingestion)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1"+tc.route, nil)
			require.NoError(t, err)
			request.Header.Set("Origin", ts.URL)
			response, err := http.DefaultClient.Do(request)
			require.NoError(t, err)
			body, err := io.ReadAll(response.Body)
			require.NoError(t, response.Body.Close())
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode, string(body))
			assert.True(t, ingestion.Active(), "archive write routes resume collection through shared middleware")
			assert.False(t, ingestion.Pending())
		})
	}
}
