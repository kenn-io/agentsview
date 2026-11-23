package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestServeRestartServesSessionsBeforeStartupSync(t *testing.T) {
	isolateServeTestHome(t)
	t.Setenv(syncWorkerChildEnvVar, "1")
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	root := cfg.AgentDirs[parser.AgentClaude][0]
	writeSession := func(id string) {
		t.Helper()
		content := testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-02T00:00:00Z", id).AddClaudeAssistant("2026-01-02T00:00:01Z", "reply").AddClaudeUser("2026-01-02T00:00:02Z", "follow up").String()
		require.NoError(t, os.WriteFile(filepath.Join(root, "-home-proj0", id+".jsonl"), []byte(content), 0o644))
	}
	writeSession("session0")
	var seed bytes.Buffer
	require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
	writeSession("before-worker")

	started, release, synced, finish := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	deferredStartupSyncUsesWorker = true
	defer func() { deferredStartupSyncUsesWorker = false }()
	restore := stubLaunchSyncWorker(t, func(ctx context.Context, cfg config.Config, req syncWorkerRequest, _ func(workerLine)) (workerResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return workerResult{}, ctx.Err()
		}
		var out bytes.Buffer
		err := runSyncWorkerContext(ctx, cfg, req, &out)
		var result workerResult
		scanner := bufio.NewScanner(&out)
		for scanner.Scan() {
			var line workerLine
			if decodeErr := json.Unmarshal(scanner.Bytes(), &line); decodeErr != nil {
				return result, decodeErr
			}
			if line.Result != nil {
				result = *line.Result
			}
		}
		close(synced)
		select {
		case <-finish:
		case <-ctx.Done():
			return result, ctx.Err()
		}
		return result, err
	})
	defer restore()
	ready := make(chan string, 1)
	original := writeDaemonRuntimeWithAuthAndNoSync
	writeDaemonRuntimeWithAuthAndNoSync = func(lifetime *daemonLifetime, dataDir, host string, port int, version, browserURL string, readOnly, requireAuth, noSync bool, explicitPort *int, caddyPID ...int) (string, error) {
		ready <- fmt.Sprintf("http://%s:%d", host, port)
		return original(lifetime, dataDir, host, port, version, browserURL, readOnly, requireAuth, noSync, explicitPort, caddyPID...)
	}
	defer func() { writeDaemonRuntimeWithAuthAndNoSync = original }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, serveOptions{}, 0) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("server did not stop")
		}
	}()
	var base string
	select {
	case base = <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("server did not become ready")
	}
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("worker did not start")
	}
	get := func(path string) string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		return string(body)
	}
	require.Contains(t, get("/api/v1/sessions?limit=25"), "session0")
	require.Contains(t, get("/api/v1/sync/status"), `"startup_pending":true`)
	close(release)
	select {
	case <-synced:
	case <-time.After(30 * time.Second):
		t.Fatal("worker did not sync")
	}
	writeSession("during-worker")
	close(finish)
	var body string
	if !assert.Eventually(t, func() bool {
		body = get("/api/v1/sessions?limit=25")
		return strings.Contains(body, "before-worker") && strings.Contains(body, "during-worker")
	}, 30*time.Second, 100*time.Millisecond) {
		t.Logf("last response: %s", body)
	}
}

func TestServeRestartSyncEligibility(t *testing.T) {
	for _, tc := range []struct {
		name                                          string
		probeErr, statErr                             error
		needsResync, fullRebuild, archiveOnly, noSync bool
		want                                          bool
	}{
		{name: "current archive", want: true},
		{name: "missing archive", statErr: os.ErrNotExist},
		{name: "probe failed", probeErr: os.ErrPermission},
		{name: "scoped upgrade or schema repair", needsResync: true},
		{name: "full rebuild", fullRebuild: true},
		{name: "archive only", archiveOnly: true},
		{name: "sync disabled", noSync: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, deferRestartStartupSync(tc.probeErr, tc.statErr, tc.needsResync, tc.fullRebuild, tc.archiveOnly, tc.noSync))
		})
	}
}
