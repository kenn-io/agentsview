package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestIdleDirectReadWakesDaemon(t *testing.T) {
	for _, slow := range []bool{false, true} {
		name := "fresh"
		if slow {
			name = "catch-up exceeds wait budget"
		}
		t.Run(name, func(t *testing.T) {
			f := newIngestionLifecycleFixture(t)
			f.cfg.DataDir = t.TempDir()
			f.ingestion.Suspend()
			f.ingestion.applying.Wait()
			writeReloadGeminiSession(t, f.root, "overnight")
			require.False(t, sessionImported(t, f.database, "gemini:overnight"))
			ts := serveIngestionArchive(t, f.cfg, f.database, f.engine, f.ingestion)
			ep := serverEndpoint(t, ts)
			_, err := WriteDaemonRuntime(f.cfg.DataDir, ep.Host, ep.Port, "test", false)
			require.NoError(t, err)

			unblock := func() {}
			if slow {
				entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
				unblock = sync.OnceFunc(func() { close(release); <-finished })
				defer unblock()
				go func() {
					defer close(finished)
					_ = f.engine.RunExclusive(func() error { close(entered); <-release; return nil })
				}()
				<-entered
			}

			stderr := captureStderr(t, func() {
				ensureFreshData(t.Context(), f.cfg, f.database, false)
			})

			assert.True(t, f.ingestion.Active(), "the direct reader wakes daemon collection")
			if slow {
				assert.Equal(t, apiclient.ArchiveStaleWarning+"\n", stderr)
				assert.False(t, sessionImported(t, f.database, "gemini:overnight"), "the reader returns while catch-up is blocked")
			} else {
				assert.Empty(t, stderr)
			}
			unblock()
			f.ingestion.applying.Wait()
			messages, err := f.database.GetAllMessages(t.Context(), "gemini:overnight")
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "hello from overnight", messages[0].Content)
		})
	}
}

func TestIdleDirectReadSkipsSyncWhileDaemonOwnsArchive(t *testing.T) {
	for _, state := range []string{"request fails", "starting", "incompatible"} {
		t.Run(state, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			cfg := config.Config{DataDir: runtimeTestDir(t), Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
			var reads atomic.Int64
			switch state {
			case "request fails":
				ts := httptest.NewUnstartedServer(nil)
				cfg.Port = ts.Listener.Addr().(*net.TCPAddr).Port
				daemon := server.New(cfg, database, nil).Handler()
				ts.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v1/codex-cloud/status" {
						reads.Add(1)
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					daemon.ServeHTTP(w, r)
				})
				ts.Start()
				t.Cleanup(ts.Close)
				_, err := WriteDaemonRuntime(cfg.DataDir, cfg.Host, cfg.Port, "test", false)
				require.NoError(t, err)
			case "starting":
				MarkDaemonStarting(cfg.DataDir)
				t.Cleanup(func() { UnmarkDaemonStarting(cfg.DataDir) })
			case "incompatible":
				writeLiveRuntime(t, cfg.DataDir, false, withRuntimeAPIVersion(0))
			}
			writeReloadGeminiSession(t, root, "overnight")

			stderr := captureStderr(t, func() {
				ensureFreshData(t.Context(), cfg, database, false)
			})

			if state == "request fails" {
				assert.Equal(t, apiclient.ArchiveStaleWarning+"\n", stderr)
				assert.Equal(t, int64(1), reads.Load(), "the reader attempts to wake the running daemon")
			}
			messages, err := database.GetAllMessages(t.Context(), "gemini:overnight")
			require.NoError(t, err)
			assert.Empty(t, messages, "the daemon owns ingestion even when a wake request cannot succeed")
		})
	}
}

func TestIdleWakeReleasesSkippedStartupMaintenance(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	cfg := config.Config{Host: "127.0.0.1", SkipInitialSync: true, AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{
		AgentDirs: cfg.AgentDirs, Machine: "test-machine", DeferStartupMaintenance: true,
	})
	t.Cleanup(engine.Close)
	tracker := server.NewIdleTracker(0, func() {})
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, tracker, nil)
	t.Cleanup(ingestion.Stop)
	require.False(t, engine.StartupReconciled())
	ingestion.Suspend()
	ingestion.applying.Wait()
	writeReloadGeminiSession(t, root, "overnight")

	require.True(t, ingestion.EnsureFresh(t.Context()))
	ingestion.applying.Wait()
	require.True(t, sessionImported(t, database, "gemini:overnight"))
	requireStartupMaintenanceReleased(t, engine)

	timeout := make(chan time.Time, 1)
	timeout <- time.Now()
	ran, err := runDeferredStartupSyncFallback(t.Context(), cfg, engine, database, nil, tracker, timeout)
	require.NoError(t, err)
	assert.False(t, ran, "the later startup fallback skips a pass already completed by idle wake")
}

func TestDaemonIngestionFailedCatchupClearsPending(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("fixture needs enforced POSIX file permissions")
	}
	f := newIngestionLifecycleFixture(t)
	f.ingestion.Suspend()
	f.ingestion.applying.Wait()
	writeReloadGeminiSession(t, f.root, "overnight")
	path := filepath.Join(f.root, "tmp", "project", "chats", "session-overnight.json")
	require.NoError(t, os.Chmod(path, 0))
	t.Cleanup(func() { require.NoError(t, os.Chmod(path, 0o600)) })
	_, err := os.ReadFile(path)
	require.ErrorIs(t, err, os.ErrPermission)
	logs := captureLogOutput(t)
	ts := serveIngestionArchive(t, f.cfg, f.database, f.engine, f.ingestion)
	headers, body := searchIngestionArchive(t, ts.URL, "overnight")
	f.ingestion.applying.Wait()
	f.ingestion.Stop()
	assert.NotContains(t, body, "hello from overnight")
	assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader))
	assert.False(t, f.ingestion.Pending(), "a finished wake pass clears pending even when a source fails")
	assert.Contains(t, logs.String(), "idle catch-up incomplete: 1 failed, aborted=false")
}

func TestIdleManualWakeRunsOneFullPass(t *testing.T) {
	var lists atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lists.Add(1)
		_, _ = io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
	}))
	defer remote.Close()
	t.Setenv("AWS_S3_ENDPOINT", remote.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_REGION", "us-east-1")
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	cfg.AgentDirs[parser.AgentClaude] = append(cfg.AgentDirs[parser.AgentClaude], "s3://bucket/sessions")
	database, lock := openTestWriteDB(t, cfg)
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
	t.Cleanup(ingestion.Stop)
	ingestion.Suspend()
	ingestion.applying.Wait()
	lists.Store(0)
	restore := stubLaunchSyncWorker(t, func(ctx context.Context, c config.Config, request syncWorkerRequest, _ func(workerLine)) (workerResult, error) {
		var wire bytes.Buffer
		workerErr := runSyncWorkerContext(ctx, c, request, &wire)
		result, err := readWorkerResult(&wire, nil)
		require.NoError(t, err)
		return result, workerErr
	})
	defer restore()
	var endpoint string
	runner := server.LocalSyncRunner(func(ctx context.Context, _ func(agentsync.Progress)) (agentsync.SyncStats, error) {
		headers, _ := searchIngestionArchive(t, endpoint, "hello")
		assert.Equal(t, "true", headers.Get(apiclient.ArchiveStaleHeader))
		assert.True(t, ingestion.Pending(), "the foreground pass has yet to complete")
		assert.Zero(t, lists.Load(), "a concurrent reader shares the foreground pass instead of starting discovery")
		stats, _, err := runForegroundWorkerSyncPass(ctx, t.Context(), cfg, engine, database, lock, nil)
		return stats, err
	})
	ts := serveIngestionArchive(t, cfg, database, engine, ingestion, server.WithLocalSyncRunner(ingestion.foregroundSync(withAttachedSources(ingestion.attached, runner))))
	endpoint = ts.URL
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/sync", nil)
	require.NoError(t, err)
	request.Header.Set("Origin", ts.URL)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.Contains(t, string(body), "event: done")
	assert.NotContains(t, string(body), "event: error")
	assert.False(t, ingestion.Pending())
	assert.True(t, ingestion.Active(), "manual wake restores collection")
	ingestion.applying.Wait()
	assert.Equal(t, int64(1), lists.Load(), "a manual sync can satisfy the pending catch-up in one pass")
}
