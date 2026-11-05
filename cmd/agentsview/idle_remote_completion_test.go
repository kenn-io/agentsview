package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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

func TestIdleRemoteSyncIncludingLocalSettlesRecovery(t *testing.T) {
	var denied atomic.Bool
	objects := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if denied.Load() {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code></Error>")
			return
		}
		_, _ = io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
	}))
	t.Cleanup(objects.Close)
	t.Setenv("AWS_S3_ENDPOINT", objects.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_REGION", "us-east-1")
	remoteDB := dbtest.OpenTestDB(t)
	remote := httptest.NewUnstartedServer(nil)
	remoteConfig := config.Config{Host: "127.0.0.1", Port: remote.Listener.Addr().(*net.TCPAddr).Port, AuthToken: "test-token", DataDir: t.TempDir(), AgentDirs: map[parser.AgentType][]string{parser.AgentGemini: {t.TempDir()}}}
	remote.Config.Handler = server.New(remoteConfig, remoteDB, nil, server.WithBaseContext(t.Context())).Handler()
	remote.Start()
	t.Cleanup(remote.Close)
	root := t.TempDir()
	cfg := config.Config{Host: "127.0.0.1", DataDir: t.TempDir(), AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {"s3://bucket/sessions"}, parser.AgentGemini: {root}}, RemoteHosts: []config.RemoteHost{{Host: "fixture-source", Transport: config.RemoteTransportHTTP, URL: remote.URL, Token: "test-token"}}}
	database := dbtest.OpenTestDB(t)
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
	t.Cleanup(ingestion.Stop)
	ingestion.Suspend()
	ingestion.applying.Wait()
	denied.Store(true)
	require.False(t, ingestion.EnsureFresh(t.Context()))
	ingestion.applying.Wait()
	ts := serveIngestionArchive(t, cfg, database, engine, ingestion)
	writeReloadGeminiSession(t, root, "after-repair")
	_, err := runDaemonRemoteSync(t.Context(), transport{URL: ts.URL}, "", cfg.RemoteHosts, false, true, nil)
	require.Error(t, err, "denied S3 discovery leaves the full local pass incomplete")
	headers, _ := searchIngestionArchive(t, ts.URL, "after-repair")
	require.Equal(t, "true", headers.Get(apiclient.SyncPendingHeader), "healthy local imports cannot settle failed S3 recovery")
	denied.Store(false)
	failures, err := runDaemonRemoteSync(t.Context(), transport{URL: ts.URL}, "", cfg.RemoteHosts, false, true, nil)
	require.NoError(t, err)
	require.Empty(t, failures)
	require.True(t, sessionImported(t, database, "gemini:after-repair"))
	headers, body := searchIngestionArchive(t, ts.URL, "after-repair")
	assert.Contains(t, body, "hello from after-repair")
	assert.Empty(t, headers.Get(apiclient.SyncPendingHeader), "a completed full manual import must settle its idle warning")
}
