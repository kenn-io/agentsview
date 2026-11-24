package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
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
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func writeRestartSession(t *testing.T, cfg config.Config, id string) string {
	t.Helper()
	path := filepath.Join(cfg.AgentDirs[parser.AgentClaude][0], "-home-proj0", id+".jsonl")
	content := testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-02T00:00:00Z", id).AddClaudeAssistant("2026-01-02T00:00:01Z", "reply").AddClaudeUser("2026-01-02T00:00:02Z", "follow up").String()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

// startRestartServe runs an ordinary serve and returns its base URL once the
// runtime record is published.
func startRestartServe(t *testing.T, cfg config.Config) string {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, serveOptions{}, 0) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			assert.Fail(t, "server did not stop")
		}
	})
	return fmt.Sprintf("http://127.0.0.1:%d", waitForRestartRuntime(t, cfg.DataDir).Port)
}

func restartGet(t *testing.T, url string) string {
	t.Helper()
	resp, err := restartTestRequest(t, http.MethodGet, url, "", "")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	return string(body)
}

func TestServeRestartServesSessionsBeforeStartupSync(t *testing.T) {
	isolateServeTestHome(t)
	t.Setenv(syncWorkerChildEnvVar, "1")
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	cfg.EventsCoalesceInterval = 0
	writeRestartSession(t, cfg, "visible-seed")
	var seed bytes.Buffer
	require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
	writeRestartSession(t, cfg, "before-worker")

	started, release := make(chan syncWorkerRequest, 1), make(chan struct{})
	restore := stubLaunchSyncWorker(t, func(ctx context.Context, cfg config.Config, req syncWorkerRequest, _ func(workerLine)) (workerResult, error) {
		started <- req
		select {
		case <-release:
		case <-ctx.Done():
			return workerResult{}, ctx.Err()
		}
		var out bytes.Buffer
		err := runSyncWorkerContext(ctx, cfg, req, &out)
		return decodeSingleResult(t, &out), err
	})
	t.Cleanup(restore)
	base := startRestartServe(t, cfg)
	select {
	case request := <-started:
		require.Equal(t, "startup", request.Mode)
	case <-time.After(30 * time.Second):
		require.FailNow(t, "worker did not start")
	}
	page := restartGet(t, base+"/api/v1/sessions/sidebar-index?limit=500")
	require.Contains(t, page, "visible-seed")
	require.Contains(t, restartGet(t, base+"/api/v1/sync/status"), `"startup_pending":true`)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/events", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	require.True(t, scanner.Scan())
	require.Contains(t, scanner.Text(), "events_ready")
	close(release)
	found := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), `"scope":"sessions"`) {
			found = true
			break
		}
	}
	require.True(t, found, "committed sessions should emit an event: %v", scanner.Err())
	require.Contains(t, restartGet(t, base+"/api/v1/sessions/sidebar-index?limit=500"), "before-worker")
	var body string
	if !assert.Eventually(t, func() bool {
		body = restartGet(t, base+"/api/v1/sessions/sidebar-index?limit=500")
		return strings.Contains(body, "before-worker") && strings.Contains(restartGet(t, base+"/api/v1/sync/status"), `"startup_pending":false`)
	}, 30*time.Second, 100*time.Millisecond) {
		t.Logf("last response: %s", body)
	}
}

func TestServeStartupWorkerFailure(t *testing.T) {
	isolateServeTestHome(t)
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	cfg.EventsCoalesceInterval = 0
	writeRestartSession(t, cfg, "visible-seed")
	var seed bytes.Buffer
	require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
	release := make(chan struct{})
	restore := stubLaunchSyncWorker(t, func(ctx context.Context, _ config.Config, _ syncWorkerRequest, _ func(workerLine)) (workerResult, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return workerResult{}, ctx.Err()
		}
		return workerResult{}, errors.New("injected worker failure")
	})
	t.Cleanup(restore)
	base := startRestartServe(t, cfg)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/events", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	require.True(t, scanner.Scan())
	require.Contains(t, scanner.Text(), "events_ready")
	close(release)
	require.Eventually(t, func() bool {
		status := restartGet(t, base+"/api/v1/sync/status")
		return strings.Contains(status, `"startup_pending":true`) && strings.Contains(status, `"aborted":true`)
	}, 30*time.Second, 100*time.Millisecond)
	found := false
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), `"scope":"sessions"`) {
			found = true
			break
		}
	}
	require.True(t, found, "failed worker should emit an event: %v", scanner.Err())
}

func TestServeResumesFirstImportWithoutStartupWorker(t *testing.T) {
	isolateServeTestHome(t)
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "saved", Agent: "claude", Machine: "local"}))
	require.NoError(t, database.SetSyncState(t.Context(), syncpkg.FirstImportPendingKey, "1"))
	require.NoError(t, database.Close())
	launched := make(chan struct{}, 1)
	restore := stubLaunchSyncWorker(t, func(context.Context, config.Config, syncWorkerRequest, func(workerLine)) (workerResult, error) {
		launched <- struct{}{}
		return workerResult{}, nil
	})
	t.Cleanup(restore)
	base := startRestartServe(t, cfg)
	require.Eventually(t, func() bool {
		return strings.Contains(restartGet(t, base+"/api/v1/sync/status"), `"startup_pending":false`)
	}, 30*time.Second, 100*time.Millisecond)
	select {
	case <-launched:
		require.FailNow(t, "startup worker ran during first import")
	default:
	}
}
