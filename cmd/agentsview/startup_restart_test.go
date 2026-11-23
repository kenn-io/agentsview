package main

import (
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
	restartGet(t, base+"/api/v1/analytics/activity?timezone=America%2FChicago&from=2026-01-01&to=2026-12-31")
	close(release)
	select {
	case <-synced:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "worker did not sync")
	}
	writeRestartSession(t, cfg, "during-worker")
	close(finish)
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

func TestServeStartupWorkerFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		firstStart bool
		spawnError bool
	}{
		{name: "worker failed"},
		{name: "spawn failed", spawnError: true},
		{name: "first start", firstStart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateServeTestHome(t)
			cfg := testConfigWithClaudeFixture(t)
			cfg.Host = "127.0.0.1"
			writeRestartSession(t, cfg, "session0")
			if !tc.firstStart {
				var seed bytes.Buffer
				require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
				if tc.spawnError {
					writeRestartSession(t, cfg, "fallback")
				}
			}
			launched := make(chan struct{}, 1)
			restore := stubLaunchSyncWorker(t, func(context.Context, config.Config, syncWorkerRequest, func(workerLine)) (workerResult, error) {
				launched <- struct{}{}
				if tc.spawnError {
					return workerResult{}, fmt.Errorf("%w: test", errWorkerSpawn)
				}
				return workerResult{Status: "aborted"}, errors.New("injected worker failure")
			})
			t.Cleanup(restore)
			base := startRestartServe(t, cfg)
			var status string
			require.Eventually(t, func() bool {
				status = restartGet(t, base+"/api/v1/sync/status")
				if tc.firstStart || tc.spawnError {
					return strings.Contains(status, `"startup_pending":false`)
				}
				return strings.Contains(status, `"startup_pending":true`) && strings.Contains(status, `"aborted":true`)
			}, 30*time.Second, 100*time.Millisecond)
			if tc.firstStart {
				select {
				case <-launched:
					require.FailNow(t, "post-listen worker ran")
				default:
				}
			} else {
				select {
				case <-launched:
				default:
					require.FailNow(t, "worker did not start")
				}
			}
			if tc.spawnError {
				assert.Contains(t, status, `"synced":1`)
			}
		})
	}
}
