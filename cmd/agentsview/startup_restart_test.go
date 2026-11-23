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
	"go.kenn.io/agentsview/internal/parser"
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
	ready := make(chan string, 1)
	original := writeDaemonRuntimeWithAuthAndNoSync
	writeDaemonRuntimeWithAuthAndNoSync = func(lifetime *daemonLifetime, dataDir, host string, port int, version, browserURL string, readOnly, requireAuth, noSync bool, explicitPort *int, caddyPID ...int) (string, error) {
		ready <- fmt.Sprintf("http://%s:%d", host, port)
		return original(lifetime, dataDir, host, port, version, browserURL, readOnly, requireAuth, noSync, explicitPort, caddyPID...)
	}
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
		writeDaemonRuntimeWithAuthAndNoSync = original
	})
	select {
	case base := <-ready:
		return base
	case <-time.After(30 * time.Second):
		require.FailNow(t, "server did not become ready")
		return ""
	}
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
	writeRestartSession(t, cfg, "session0")
	deleted := writeRestartSession(t, cfg, "offline-deleted")
	var seed bytes.Buffer
	require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
	require.NoError(t, os.Remove(deleted))
	writeRestartSession(t, cfg, "before-worker")

	started, release, synced, finish := make(chan syncWorkerRequest, 1), make(chan struct{}), make(chan struct{}), make(chan struct{})
	restore := stubLaunchSyncWorker(t, func(ctx context.Context, cfg config.Config, req syncWorkerRequest, _ func(workerLine)) (workerResult, error) {
		started <- req
		select {
		case <-release:
		case <-ctx.Done():
			return workerResult{}, ctx.Err()
		}
		var out bytes.Buffer
		err := runSyncWorkerContext(ctx, cfg, req, &out)
		result := decodeSingleResult(t, &out)
		close(synced)
		select {
		case <-finish:
		case <-ctx.Done():
			return result, ctx.Err()
		}
		return result, err
	})
	t.Cleanup(restore)
	base := startRestartServe(t, cfg)
	select {
	case request := <-started:
		require.Equal(t, "startup", request.Mode)
		require.True(t, request.LiveArchive)
	case <-time.After(30 * time.Second):
		require.FailNow(t, "worker did not start")
	}
	page := restartGet(t, base+"/api/v1/sessions?limit=25")
	require.Contains(t, page, "session0")
	require.Contains(t, page, "offline-deleted")
	require.Contains(t, restartGet(t, base+"/api/v1/sync/status"), `"startup_pending":true`)
	for _, panel := range []string{"activity", "hour-of-week", "velocity"} {
		restartGet(t, base+"/api/v1/analytics/"+panel+"?timezone=America%2FChicago&from=2026-01-01&to=2026-12-31")
	}
	eventsReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/v1/events", nil)
	require.NoError(t, err)
	events, err := http.DefaultClient.Do(eventsReq) //nolint:bodyclose // The event stream is closed after the assertions.
	require.NoError(t, err)
	defer events.Body.Close()
	eventSeen := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(events.Body)
		for scanner.Scan() {
			if scanner.Text() == "event: data_changed" {
				close(eventSeen)
				return
			}
		}
	}()
	close(release)
	select {
	case <-synced:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "worker did not sync")
	}
	writeRestartSession(t, cfg, "during-worker")
	close(finish)
	select {
	case <-eventSeen:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "startup sync did not notify the session list")
	}
	var body string
	if !assert.Eventually(t, func() bool {
		body = restartGet(t, base+"/api/v1/sessions?limit=25")
		return strings.Contains(body, "before-worker") && strings.Contains(body, "during-worker") && strings.Contains(restartGet(t, base+"/api/v1/sync/status"), `"startup_pending":false`)
	}, 30*time.Second, 100*time.Millisecond) {
		t.Logf("last response: %s", body)
	}
	database, err := db.OpenReadOnly(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	defer database.Close()
	missing, err := database.GetSessionFull(t.Context(), "offline-deleted")
	require.NoError(t, err)
	require.NotNil(t, missing)
	require.NotNil(t, missing.SourceMissingAt)
	require.Nil(t, missing.DeletedAt)
}

func TestServeRestartWorkerFailureRestoresWrites(t *testing.T) {
	isolateServeTestHome(t)
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	writeRestartSession(t, cfg, "session0")
	var seed bytes.Buffer
	require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
	started, release := make(chan struct{}), make(chan struct{})
	restore := stubLaunchSyncWorker(t, func(ctx context.Context, _ config.Config, _ syncWorkerRequest, _ func(workerLine)) (workerResult, error) {
		close(started)
		select {
		case <-release:
			return workerResult{Status: "aborted"}, errors.New("injected worker failure")
		case <-ctx.Done():
			return workerResult{}, ctx.Err()
		}
	})
	t.Cleanup(restore)
	base := startRestartServe(t, cfg)
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "worker did not start")
	}
	require.Contains(t, restartGet(t, base+"/api/v1/sessions?limit=25"), "session0")
	rename := func() *http.Response {
		t.Helper()
		resp, err := restartTestRequest(t, http.MethodPatch, base+"/api/v1/sessions/session0/rename", `{"display_name":"Renamed"}`, "")
		require.NoError(t, err)
		return resp
	}
	resp := rename()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("Retry-After"))
	require.NoError(t, resp.Body.Close())
	close(release)
	require.Eventually(t, func() bool {
		status := restartGet(t, base+"/api/v1/sync/status")
		return strings.Contains(status, `"startup_pending":true`) && strings.Contains(status, `"aborted":true`)
	}, 30*time.Second, 100*time.Millisecond)
	resp = rename()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, resp.Body.Close())
}

func TestServeRestartSpawnFailureFallsBack(t *testing.T) {
	isolateServeTestHome(t)
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	missingPath := writeRestartSession(t, cfg, "offline-missing")
	var seed bytes.Buffer
	require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
	require.NoError(t, os.Remove(missingPath))
	writeRestartSession(t, cfg, "fallback")
	restore := stubLaunchSyncWorker(t, func(context.Context, config.Config, syncWorkerRequest, func(workerLine)) (workerResult, error) {
		return workerResult{}, fmt.Errorf("%w: test", errWorkerSpawn)
	})
	t.Cleanup(restore)
	base := startRestartServe(t, cfg)
	require.Eventually(t, func() bool {
		return strings.Contains(restartGet(t, base+"/api/v1/sync/status"), `"startup_pending":false`)
	}, 30*time.Second, 100*time.Millisecond)
	require.Contains(t, restartGet(t, base+"/api/v1/sessions?limit=25"), "fallback")
	database, err := db.OpenReadOnly(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	defer database.Close()
	assert.Eventually(t, func() bool {
		missing, err := database.GetSessionFull(t.Context(), "offline-missing")
		return err == nil && missing != nil && missing.SourceMissingAt != nil && missing.DeletedAt == nil
	}, 5*time.Second, 100*time.Millisecond)
}

func TestServeFirstStart(t *testing.T) {
	isolateServeTestHome(t)
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	writeRestartSession(t, cfg, "session0")
	launched := make(chan struct{}, 1)
	restore := stubLaunchSyncWorker(t, func(context.Context, config.Config, syncWorkerRequest, func(workerLine)) (workerResult, error) {
		launched <- struct{}{}
		return workerResult{}, errors.New("unexpected worker")
	})
	t.Cleanup(restore)
	base := startRestartServe(t, cfg)
	require.Contains(t, restartGet(t, base+"/api/v1/sessions?limit=25"), "session0")
	select {
	case <-launched:
		require.FailNow(t, "post-listen worker ran")
	default:
	}
}
