package main

import (
	"bytes"
	"context"
	"errors"
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
			} else {
				assert.Empty(t, stderr)
			}
			unblock()
			f.ingestion.applying.Wait()
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

			assert.Equal(t, apiclient.ArchiveStaleWarning+"\n", stderr)
			if state == "request fails" {
				assert.Equal(t, int64(1), reads.Load(), "the reader attempts to wake the running daemon")
			}
			messages, err := database.GetAllMessages(t.Context(), "gemini:overnight")
			require.NoError(t, err)
			assert.Empty(t, messages, "the daemon owns ingestion even when a wake request cannot succeed")
		})
	}
}

func TestIdleAutomaticWakeReconcilesDeletedSource(t *testing.T) {
	f := newIngestionLifecycleFixture(t)
	f.ingestion.Suspend()
	f.ingestion.applying.Wait()
	path := filepath.Join(f.root, "tmp", "project", "chats", "session-retained.json")
	require.NoError(t, os.Remove(path))
	before, err := f.database.GetSessionFull(t.Context(), "gemini:retained")
	require.NoError(t, err)
	require.NotNil(t, before)
	require.Nil(t, before.SourceMissingAt)

	require.True(t, f.ingestion.EnsureFresh(t.Context()))
	f.ingestion.applying.Wait()

	after, err := f.database.GetSessionFull(t.Context(), "gemini:retained")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.NotNil(t, after.SourceMissingAt, "wake reconciles sources removed while collection was suspended")
	assert.Nil(t, after.DeletedAt, "missing sources retain their archived history")
	messages, err := f.database.GetAllMessages(t.Context(), "gemini:retained")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "hello from retained", messages[0].Content)
}

func TestIdleAutomaticWakePreservesUnavailableRoot(t *testing.T) {
	f := newIngestionLifecycleFixture(t)
	f.ingestion.Suspend()
	f.ingestion.applying.Wait()
	require.NoError(t, os.Rename(f.root, filepath.Join(t.TempDir(), "offline")))

	require.True(t, f.ingestion.EnsureFresh(t.Context()))
	f.ingestion.applying.Wait()

	after, err := f.database.GetSessionFull(t.Context(), "gemini:retained")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Nil(t, after.SourceMissingAt, "an unavailable root leaves source state unchanged")
	assert.Nil(t, after.DeletedAt)
	messages, err := f.database.GetAllMessages(t.Context(), "gemini:retained")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "hello from retained", messages[0].Content)
}

func TestIdleManualWakeReconcilesDeletedSource(t *testing.T) {
	f := newIngestionLifecycleFixture(t)
	f.ingestion.Suspend()
	f.ingestion.applying.Wait()
	require.NoError(t, os.Remove(filepath.Join(f.root, "tmp", "project", "chats", "session-retained.json")))
	runner := server.LocalSyncRunner(func(ctx context.Context, progress func(agentsync.Progress)) (agentsync.SyncStats, error) {
		return syncAllReleasingStartupMaintenance(ctx, f.engine, progress), nil
	})
	ts := serveIngestionArchive(t, f.cfg, f.database, f.engine, f.ingestion, server.WithLocalSyncRunner(f.ingestion.foregroundSync(runner)))
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/sync", nil)
	require.NoError(t, err)
	request.Header.Set("Origin", ts.URL)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NotContains(t, string(body), "event: error")
	f.ingestion.applying.Wait()

	after, err := f.database.GetSessionFull(t.Context(), "gemini:retained")
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.NotNil(t, after.SourceMissingAt, "manual wake reconciles sources deleted while suspended")
	assert.Nil(t, after.DeletedAt)
	messages, err := f.database.GetAllMessages(t.Context(), "gemini:retained")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "hello from retained", messages[0].Content)
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

	assert.True(t, engine.StartupReconciled())
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
		return result, errors.Join(workerErr, err)
	})
	defer restore()
	type readerResult struct {
		headers http.Header
		status  int
		lists   int64
		err     error
	}
	readers := make(chan readerResult, 1)
	var client *apiclient.Client
	runner := server.LocalSyncRunner(func(ctx context.Context, _ func(agentsync.Progress)) (agentsync.SyncStats, error) {
		response, err := client.GetAPIV1SearchWithResponse(ctx, &apiclient.GetAPIV1SearchRequestOptions{Query: &apiclient.GetAPIV1SearchQuery{Q: "hello"}})
		if err != nil {
			readers <- readerResult{err: err}
			return agentsync.SyncStats{}, err
		}
		readers <- readerResult{headers: response.HTTPResponse.Header, status: response.StatusCode, lists: lists.Load()}
		stats, _, err := runForegroundWorkerSyncPass(ctx, t.Context(), cfg, engine, database, lock, nil)
		return stats, err
	})
	ts := serveIngestionArchive(t, cfg, database, engine, ingestion, server.WithLocalSyncRunner(ingestion.foregroundSync(withAttachedSources(ingestion.attached, runner))))
	client, err := apiclient.NewHTTPClient(ts.URL, "", ts.Client())
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/sync", nil)
	require.NoError(t, err)
	request.Header.Set("Origin", ts.URL)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	assert.NotContains(t, string(body), "event: error")
	reader := <-readers
	require.NoError(t, reader.err)
	assert.Equal(t, http.StatusOK, reader.status)
	assert.Equal(t, "true", reader.headers.Get(apiclient.ArchiveStaleHeader))
	assert.Zero(t, reader.lists, "a concurrent reader shares the foreground pass instead of starting discovery")
	ingestion.applying.Wait()
	assert.Equal(t, int64(1), lists.Load(), "a manual sync can satisfy the pending catch-up in one pass")
}
