package main

import (
	"bufio"
	"bytes"
	"context"
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
			t.Error("server did not stop")
		}
		writeDaemonRuntimeWithAuthAndNoSync = original
	})
	select {
	case base := <-ready:
		return base
	case <-time.After(30 * time.Second):
		t.Fatal("server did not become ready")
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
		t.Fatal("worker did not start")
	}
	page := restartGet(t, base+"/api/v1/sessions?limit=25")
	require.Contains(t, page, "session0")
	require.Contains(t, page, "offline-deleted")
	require.Contains(t, restartGet(t, base+"/api/v1/sync/status"), `"startup_pending":true`)
	eventsReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/v1/events", nil)
	require.NoError(t, err)
	events, err := http.DefaultClient.Do(eventsReq)
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
		t.Fatal("worker did not sync")
	}
	writeRestartSession(t, cfg, "during-worker")
	close(finish)
	select {
	case <-eventSeen:
	case <-time.After(30 * time.Second):
		t.Fatal("startup sync did not notify the session list")
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

func TestStartupWorkerKeepsLiveArchiveReaders(t *testing.T) {
	cfg := testConfigWithClaudeFixture(t)
	var seed bytes.Buffer
	require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
	reader, err := db.OpenReadOnly(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	defer reader.Close()
	markArchiveStale(t, cfg.DBPath)
	before, err := os.Stat(cfg.DBPath)
	require.NoError(t, err)
	request := syncWorkerRequest{Mode: "startup", LiveArchive: true}
	assert.Contains(t, syncWorkerChildArgs(nil, request), "--live-archive")
	var out bytes.Buffer
	err = runSyncWorkerContext(t.Context(), cfg, request, &out)
	require.ErrorContains(t, err, "resync")
	after, err := os.Stat(cfg.DBPath)
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after))
	session, err := reader.GetSession(t.Context(), "session0")
	require.NoError(t, err)
	require.NotNil(t, session)
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

func TestServeRestartFirstStartAndNoSync(t *testing.T) {
	for _, noSync := range []bool{false, true} {
		name := "first start"
		if noSync {
			name = "no sync"
		}
		t.Run(name, func(t *testing.T) {
			isolateServeTestHome(t)
			cfg := testConfigWithClaudeFixture(t)
			cfg.Host = "127.0.0.1"
			cfg.NoSync = noSync
			writeRestartSession(t, cfg, "session0")
			if noSync {
				var out bytes.Buffer
				require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &out))
			}
			launched := make(chan struct{}, 1)
			restore := stubLaunchSyncWorker(t, func(context.Context, config.Config, syncWorkerRequest, func(workerLine)) (workerResult, error) {
				launched <- struct{}{}
				return workerResult{}, fmt.Errorf("unexpected worker")
			})
			t.Cleanup(restore)
			base := startRestartServe(t, cfg)
			require.Contains(t, restartGet(t, base+"/api/v1/sessions?limit=25"), "session0")
			select {
			case <-launched:
				t.Fatal("post-listen worker ran")
			default:
			}
		})
	}
}
