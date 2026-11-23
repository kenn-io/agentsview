package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
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

func TestServeSessionsWhileUsageCounterFillPending(t *testing.T) {
	isolateServeTestHome(t)
	cfg := testConfigWithClaudeFixture(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", cfg.DataDir)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, "config.toml"), []byte(fmt.Sprintf("[agents.claude]\ndirs = [%q]\n", cfg.AgentDirs[parser.AgentClaude][0])), 0o600))
	var seed bytes.Buffer
	require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
	database, err := db.Open(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DROP INDEX idx_messages_usage_session_covering;
ALTER TABLE messages DROP COLUMN usage_output_tokens;
ALTER TABLE messages DROP COLUMN usage_web_search_requests;
CREATE INDEX idx_messages_usage_session_covering ON messages(session_id, ordinal, timestamp, role, model, provider_id, claude_message_id, claude_request_id, token_usage, source_uuid) WHERE token_usage != '' AND model != '' AND model != '<synthetic>';
PRAGMA user_version = 167;
CREATE TRIGGER test_hold_usage_fill BEFORE DELETE ON stats WHEN OLD.key='message_usage_counters_pending' BEGIN SELECT RAISE(ABORT, 'hold usage fill'); END`)
		return err
	}))
	require.NoError(t, database.Close())
	needsResync, _, err := db.ArchiveResyncState(t.Context(), cfg.DBPath)
	require.NoError(t, err)
	require.True(t, needsResync)
	output, err := os.CreateTemp(t.TempDir(), "server-output-*")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, output.Close()) })
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "serve", "--host=127.0.0.1", "--port=0", "--no-browser", "--no-update-check", "--require-auth=false")
	cmd.Stdout, cmd.Stderr = output, output
	configureRestartTestProcess(cmd)
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		ctx, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer stop()
		_, err := stopWritableDaemonsForUpdate(ctx, config.Config{DataDir: cfg.DataDir})
		assert.NoError(t, err)
		select {
		case <-done:
		case <-ctx.Done():
			assert.Fail(t, "foreground server did not stop")
		}
	})
	runtime := waitForRestartRuntime(t, cfg.DataDir)
	base := fmt.Sprintf("http://127.0.0.1:%d", runtime.Port)
	page := restartGet(t, base+"/api/v1/sessions?limit=25&include_one_shot=true")
	require.Contains(t, page, "session0")
	log, err := os.ReadFile(output.Name())
	require.NoError(t, err)
	require.Contains(t, string(log), "Sync complete:")
}

func TestReadOnlyCommandsReportPendingUsageUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.Open(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO stats(key, value) VALUES ('message_usage_counters_pending', 1)`)
		return err
	}))
	require.NoError(t, database.Close())
	cfg := config.Config{DBPath: path}
	for name, open := range map[string]func(context.Context, config.Config) (*db.DB, error){
		"read-only": openReadOnlyDB,
		"export":    openExportReadOnlyDB,
	} {
		t.Run(name, func(t *testing.T) {
			reader, err := open(t.Context(), cfg)
			if reader != nil {
				defer reader.Close()
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "upgrade")
			assert.Contains(t, err.Error(), "retry")
			assert.NotContains(t, err.Error(), "restart")
		})
	}
}

func TestServeStartupWorkerFailure(t *testing.T) {
	isolateServeTestHome(t)
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	cfg.EventsCoalesceInterval = 0
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

func TestServeFirstStartDoesNotLaunchPostListenWorker(t *testing.T) {
	isolateServeTestHome(t)
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
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
		require.FailNow(t, "post-listen worker ran")
	default:
	}
}
