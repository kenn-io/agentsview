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
	"go.kenn.io/agentsview/internal/db"
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
	writeSession("offline-deleted")
	var seed bytes.Buffer
	require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
	require.NoError(t, os.Remove(filepath.Join(root, "-home-proj0", "offline-deleted.jsonl")))
	writeSession("before-worker")

	started, release, synced, finish := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	requests := make(chan syncWorkerRequest, 1)
	restore := stubLaunchSyncWorker(t, func(ctx context.Context, cfg config.Config, req syncWorkerRequest, _ func(workerLine)) (workerResult, error) {
		requests <- req
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
	go func() { done <- runServe(ctx, cfg, serveOptions{StartupWorkerAfterListen: true}, 0) }()
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
	request := <-requests
	require.Equal(t, "startup", request.Mode)
	require.True(t, request.LiveArchive)
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
	require.Contains(t, get("/api/v1/sessions?limit=25"), "offline-deleted")
	require.Contains(t, get("/api/v1/sync/status"), `"startup_pending":true`)
	eventsReq, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/events", nil)
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
	rename := func() *http.Response {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, base+"/api/v1/sessions/session0/rename", strings.NewReader(`{"display_name":"Renamed"}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", base)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}
	resp := rename()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.NotEmpty(t, resp.Header.Get("Retry-After"))
	require.NoError(t, resp.Body.Close())
	close(release)
	select {
	case <-synced:
	case <-time.After(30 * time.Second):
		t.Fatal("worker did not sync")
	}
	require.Contains(t, get("/api/v1/sessions?limit=25"), "before-worker")
	require.Contains(t, get("/api/v1/sync/status"), `"startup_pending":true`)
	writeSession("during-worker")
	close(finish)
	select {
	case <-eventSeen:
	case <-time.After(30 * time.Second):
		t.Fatal("startup sync did not notify the session list")
	}
	var renameStatus int
	require.Eventually(t, func() bool {
		resp := rename()
		renameStatus = resp.StatusCode
		resp.Body.Close()
		return renameStatus == http.StatusOK
	}, 30*time.Second, 100*time.Millisecond)
	require.Contains(t, get("/api/v1/sessions?limit=25"), "Renamed")
	var body string
	if !assert.Eventually(t, func() bool {
		body = get("/api/v1/sessions?limit=25")
		return strings.Contains(body, "before-worker") && strings.Contains(body, "during-worker") && strings.Contains(get("/api/v1/sync/status"), `"startup_pending":false`)
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

func TestServeRestartSpawnFailureFallsBack(t *testing.T) {
	isolateServeTestHome(t)
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	var seed bytes.Buffer
	require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
	path := filepath.Join(cfg.AgentDirs[parser.AgentClaude][0], "-home-proj0", "fallback.jsonl")
	content := testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-02T00:00:00Z", "fallback").AddClaudeAssistant("2026-01-02T00:00:01Z", "reply").AddClaudeUser("2026-01-02T00:00:02Z", "follow up").String()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	restore := stubLaunchSyncWorker(t, func(context.Context, config.Config, syncWorkerRequest, func(workerLine)) (workerResult, error) {
		return workerResult{}, fmt.Errorf("%w: test", errWorkerSpawn)
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
	go func() { done <- runServe(ctx, cfg, serveOptions{StartupWorkerAfterListen: true}, 0) }()
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
	require.Eventually(t, func() bool {
		for _, path := range []string{"/api/v1/sessions?limit=25", "/api/v1/sync/status"} {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
			if err != nil {
				return false
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return false
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				return false
			}
			if path == "/api/v1/sessions?limit=25" && !strings.Contains(string(body), "fallback") {
				return false
			}
			if path == "/api/v1/sync/status" && !strings.Contains(string(body), `"startup_pending":false`) {
				return false
			}
		}
		return true
	}, 30*time.Second, 100*time.Millisecond)
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
			path := filepath.Join(cfg.AgentDirs[parser.AgentClaude][0], "-home-proj0", "session0.jsonl")
			content := testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-02T00:00:00Z", "first").AddClaudeAssistant("2026-01-02T00:00:01Z", "reply").AddClaudeUser("2026-01-02T00:00:02Z", "follow up").String()
			require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
			if noSync {
				var out bytes.Buffer
				require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &out))
			}
			launched := make(chan struct{}, 1)
			restore := stubLaunchSyncWorker(t, func(context.Context, config.Config, syncWorkerRequest, func(workerLine)) (workerResult, error) {
				launched <- struct{}{}
				return workerResult{}, fmt.Errorf("unexpected worker")
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
			go func() { done <- runServe(ctx, cfg, serveOptions{StartupWorkerAfterListen: true}, 0) }()
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
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/v1/sessions?limit=25", nil)
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, resp.Body.Close())
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Contains(t, string(body), "session0")
			select {
			case <-launched:
				t.Fatal("post-listen worker ran")
			default:
			}
		})
	}
}
