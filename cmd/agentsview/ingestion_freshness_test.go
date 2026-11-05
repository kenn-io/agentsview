package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

func TestDaemonIngestionFailedCatchupKeepsWarning(t *testing.T) {
	for _, source := range []parser.AgentType{parser.AgentGemini, parser.AgentClaude} {
		t.Run(string(source), func(t *testing.T) {
			if source == parser.AgentGemini && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
				t.Skip("fixture needs enforced POSIX file permissions")
			}
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
			var denied atomic.Bool
			var published atomic.Bool
			if source != parser.AgentGemini {
				content := `{"type":"user","sessionId":"overnight","timestamp":"2026-11-05T10:00:00Z","message":{"role":"user","content":"hello from overnight"}}` + "\n"
				remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "overnight.jsonl") {
						w.Header().Set("Last-Modified", "Wed, 02 Jan 2030 00:00:00 GMT")
						w.Header().Set("ETag", `"overnight"`)
						w.Header().Set("Content-Length", strconv.Itoa(len(content)))
						_, _ = w.Write([]byte(content))
						return
					}
					if denied.Load() {
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte("<Error><Code>AccessDenied</Code></Error>"))
						return
					}
					if !published.Load() {
						_, _ = w.Write([]byte("<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>"))
						return
					}
					_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents><Key>sessions/project/overnight.jsonl</Key><LastModified>2030-01-02T00:00:00.000Z</LastModified><ETag>&quot;overnight&quot;</ETag><Size>%d</Size></Contents></ListBucketResult>`, len(content))
				}))
				t.Cleanup(remote.Close)
				t.Setenv("AWS_S3_ENDPOINT", remote.URL)
				t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
				t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
				t.Setenv("AWS_REGION", "us-east-1")
				cfg.AgentDirs[source] = []string{"s3://bucket/sessions"}
			}
			writeReloadGeminiSession(t, root, "before")
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			engine.SyncAll(t.Context(), nil)
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
			t.Cleanup(ingestion.Stop)
			ingestion.Suspend()
			ingestion.applying.Wait()
			if source == parser.AgentGemini {
				writeReloadGeminiSession(t, root, "overnight")
				path := filepath.Join(root, "tmp", "project", "chats", "session-overnight.json")
				require.NoError(t, os.Chmod(path, 0))
				t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
				_, err := os.ReadFile(path)
				require.Error(t, err, "fixture must cause an actual source-read failure")
			} else {
				published.Store(true)
				denied.Store(true)
			}
			ts := serveIngestionArchive(t, cfg, database, engine, ingestion)
			headers, body := searchIngestionArchive(t, ts.URL, "overnight")
			assert.NotContains(t, body, "hello from overnight")
			assert.Equal(t, "true", headers.Get(apiclient.SyncPendingHeader))
			assert.Equal(t, "true", headers.Get(apiclient.ArchiveStaleHeader), "failed source catch-up must warn the reader about missing overnight messages")
			assert.True(t, ingestion.Pending())
			if source == parser.AgentGemini {
				require.NoError(t, os.Chmod(filepath.Join(root, "tmp", "project", "chats", "session-overnight.json"), 0o600))
			} else {
				denied.Store(false)
			}
			require.True(t, ingestion.ensureFresh(t.Context(), true))
			headers, body = searchIngestionArchive(t, ts.URL, "overnight")
			assert.Empty(t, headers.Get(apiclient.ArchiveStaleHeader), "successful scheduled recovery clears the warning")
			assert.Contains(t, body, "hello from overnight")
		})
	}
}

func TestForegroundSyncSettlesFailedIdleRecovery(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	var denied atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if denied.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<Error><Code>AccessDenied</Code></Error>"))
			return
		}
		_, _ = w.Write([]byte("<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>"))
	}))
	t.Cleanup(remote.Close)
	t.Setenv("AWS_S3_ENDPOINT", remote.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_REGION", "us-east-1")
	cfg.AgentDirs[parser.AgentClaude] = append(cfg.AgentDirs[parser.AgentClaude], "s3://bucket/sessions")
	database, lock := openTestWriteDB(t, cfg)
	engine := agentsync.NewEngine(t.Context(), database, workerEngineConfig(cfg))
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
	t.Cleanup(ingestion.Stop)
	ingestion.Suspend()
	ingestion.applying.Wait()
	denied.Store(true)
	require.False(t, ingestion.EnsureFresh(t.Context()))
	ingestion.applying.Wait()
	require.True(t, ingestion.Pending())
	denied.Store(false)
	restore := stubLaunchSyncWorker(t, func(ctx context.Context, c config.Config, request syncWorkerRequest, _ func(workerLine)) (workerResult, error) {
		var wire bytes.Buffer
		workerErr := runSyncWorkerContext(ctx, c, request, &wire)
		result, err := readWorkerResult(&wire, nil)
		require.NoError(t, err)
		return result, workerErr
	})
	defer restore()
	runner := server.LocalSyncRunner(func(ctx context.Context, _ func(agentsync.Progress)) (agentsync.SyncStats, error) {
		stats, _, err := runForegroundWorkerSyncPass(ctx, t.Context(), cfg, engine, database, lock, nil)
		return stats, err
	})
	ts := serveIngestionArchive(t, cfg, database, engine, ingestion, server.WithLocalSyncRunner(ingestion.foregroundSync(withAttachedSources(ingestion.attached, runner))))
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/api/v1/sync", nil)
	require.NoError(t, err)
	request.Header.Set("Origin", ts.URL)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Contains(t, string(body), "event: done", "the real worker must finish successfully")
	t.Logf("pending after successful worker=%v body=%s", ingestion.Pending(), body)
	assert.False(t, ingestion.Pending(), "a complete manual pass should settle the idle obligation")
}
