package main

import (
	"context"
	"io"
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
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestIdleUnreadableClaudeProjectKeepsWarning(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("fixture needs enforced POSIX directory permissions")
	}
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	project := filepath.Join(root, "project")
	require.NoError(t, os.MkdirAll(project, 0o700))
	cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
	t.Cleanup(ingestion.Stop)
	ingestion.Suspend()
	ingestion.applying.Wait()
	content := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-02T00:00:00Z", "hello from overnight", "overnight").String()
	require.NoError(t, os.WriteFile(filepath.Join(project, "overnight.jsonl"), []byte(content), 0o600))
	require.NoError(t, os.Chmod(project, 0))
	t.Cleanup(func() { require.NoError(t, os.Chmod(project, 0o700)) })
	healthy := filepath.Join(root, "healthy")
	require.NoError(t, os.MkdirAll(healthy, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(healthy, "healthy.jsonl"), []byte(testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-02T00:00:00Z", "healthy overnight message", "healthy").String()), 0o600))
	_, err := os.ReadDir(project)
	require.Error(t, err)
	ts := serveIngestionArchive(t, cfg, database, engine, ingestion)
	headers, body := searchIngestionArchive(t, ts.URL, "overnight")
	assert.NotContains(t, body, "hello from overnight")
	assert.Contains(t, body, "healthy overnight message")
	assert.Equal(t, "true", headers.Get(apiclient.SyncPendingHeader), "inaccessible sources keep catch-up pending")
	assert.True(t, ingestion.Pending())
}

func TestIdleSuspendsAfterSlowCatchupWithoutAnotherReader(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	writeReloadGeminiSession(t, root, "before")
	cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {root}}}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	tracker := server.NewIdleTracker(0, func() {})
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, tracker, nil)
	t.Cleanup(ingestion.Stop)
	attempted := make(chan struct{}, 4)
	//nolint:kennlint // deadlinetest: the held engine must outlast the suspension deadline before release
	tracker.SetSyncIdle(100*time.Millisecond, func() {
		ingestion.Suspend()
		attempted <- struct{}{}
	})
	trackerCtx, cancel := context.WithCancel(t.Context())
	trackerDone := make(chan struct{})
	go func() { defer close(trackerDone); tracker.Run(trackerCtx) }()
	defer func() { cancel(); <-trackerDone }()
	require.Eventually(t, func() bool { return !ingestion.Active() }, time.Second, 10*time.Millisecond)
	<-attempted
	ingestion.applying.Wait()
	writeReloadGeminiSession(t, root, "overnight")
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	go func() {
		defer close(finished)
		_ = engine.RunExclusive(func() error { close(entered); <-release; return nil })
	}()
	<-entered
	ts := serveIngestionArchive(t, cfg, database, engine, ingestion, server.WithIdleTracker(tracker))
	headers, _ := searchIngestionArchive(t, ts.URL, "overnight")
	require.Equal(t, "true", headers.Get(apiclient.SyncPendingHeader))
	<-attempted
	unblock()
	<-finished
	ingestion.applying.Wait()
	require.True(t, sessionImported(t, database, "gemini:overnight"))
	assert.Eventually(t, func() bool { return !ingestion.Active() }, time.Second, 10*time.Millisecond, "catch-up completed after the idle deadline, so collection must stop without needing another request")
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
	cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {"s3://bucket/sessions"}}}
	database := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
	t.Cleanup(ingestion.Stop)
	ingestion.Suspend()
	ingestion.applying.Wait()
	lists.Store(0)
	ts := serveIngestionArchive(t, cfg, database, engine, ingestion, server.WithLocalSyncRunner(ingestion.foregroundSync(newForegroundSyncRunner(t.Context(), cfg, engine, database, nil))))
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
	assert.Equal(t, int64(1), lists.Load(), "a manual sync can satisfy the pending catch-up in one pass")
}
