package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
func startRestartServe(t *testing.T, cfg config.Config, opts serveOptions) string {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, opts, 0) }()
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

func restartContents(t *testing.T, url string) []string {
	t.Helper()
	var response struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(restartGet(t, url)), &response))
	contents := make([]string, 0, len(response.Messages))
	for _, message := range response.Messages {
		contents = append(contents, message.Content)
	}
	return contents
}

func TestServeRestartServesSessionsBeforeStartupSync(t *testing.T) {
	for _, tc := range []struct {
		name         string
		scoped, skip bool
	}{
		{name: "ordinary"},
		{name: "scoped upgrade", scoped: true},
		{name: "scoped upgrade with skip initial sync", scoped: true, skip: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateServeTestHome(t)
			t.Setenv(syncWorkerChildEnvVar, "1")
			cfg := testConfigWithClaudeFixture(t)
			cfg.Host = "127.0.0.1"
			cfg.EventsCoalesceInterval = 0
			codexRoot := t.TempDir()
			cfg.AgentDirs[parser.AgentCodex] = []string{codexRoot}
			var claudeIDs, codexIDs []string
			for i := range 3 {
				id := fmt.Sprintf("visible-seed-%d", i)
				claudeIDs = append(claudeIDs, id)
				writeRestartSession(t, cfg, id)
				uuid := fmt.Sprintf("a0000000-0000-4000-8000-%012d", i)
				codexIDs = append(codexIDs, "codex:"+uuid)
				content := testjsonl.NewSessionBuilder().
					AddCodexMeta("2026-01-02T00:00:00Z", uuid, "/project", "codex_cli_rs").
					AddCodexMessage("2026-01-02T00:00:01Z", "user", "unchanged codex transcript").String()
				path := filepath.Join(codexRoot, "rollout-2026-01-02T00-00-00-"+uuid+".jsonl")
				require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
			}
			var seed bytes.Buffer
			require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
			if tc.scoped {
				conn, err := sql.Open("sqlite3", cfg.DBPath)
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), `UPDATE messages SET content='old transcript' WHERE session_id IN (SELECT id FROM sessions WHERE agent='claude') AND content LIKE 'visible-seed-%'; UPDATE sessions SET data_version=188; PRAGMA user_version=188`)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
			}
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
			base := startRestartServe(t, cfg, serveOptions{SkipInitialSync: tc.skip})
			select {
			case request := <-started:
				require.Equal(t, "startup", request.Mode)
			case <-time.After(30 * time.Second):
				require.FailNow(t, "worker did not start")
			}
			require.Contains(t, restartGet(t, base+"/api/v1/sessions/sidebar-index?limit=500"), "visible-seed-0")
			require.Contains(t, restartGet(t, base+"/api/v1/sync/status"), `"startup_pending":true`)
			codexBefore := make(map[string][]string)
			for _, id := range codexIDs {
				contents := restartContents(t, base+"/api/v1/sessions/"+id+"/messages")
				require.Contains(t, contents, "unchanged codex transcript")
				codexBefore[id] = contents
			}
			if tc.scoped {
				for _, id := range claudeIDs {
					contents := restartContents(t, base+"/api/v1/sessions/"+id+"/messages")
					require.Contains(t, contents, "old transcript")
					require.NotContains(t, contents, id)
				}
			}
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
				if strings.Contains(scanner.Text(), `"scope":"sync"`) {
					found = true
					break
				}
			}
			require.True(t, found, "committed sessions should emit an event: %v", scanner.Err())
			require.Eventually(t, func() bool {
				stale, _, _, err := db.ArchiveResyncState(t.Context(), cfg.DBPath)
				return err == nil && !stale && strings.Contains(restartGet(t, base+"/api/v1/sync/status"), `"startup_pending":false`)
			}, 30*time.Second, 100*time.Millisecond)
			require.Contains(t, restartGet(t, base+"/api/v1/sessions/sidebar-index?limit=500"), "before-worker")
			for _, id := range claudeIDs {
				contents := restartContents(t, base+"/api/v1/sessions/"+id+"/messages")
				require.Contains(t, contents, id)
				require.NotContains(t, contents, "old transcript")
			}
			for _, id := range codexIDs {
				require.Equal(t, codexBefore[id], restartContents(t, base+"/api/v1/sessions/"+id+"/messages"))
			}
		})
	}
}

func TestServeStartupWorkerFailure(t *testing.T) {
	isolateServeTestHome(t)
	t.Setenv(syncWorkerChildEnvVar, "1")
	cfg := testConfigWithClaudeFixture(t)
	cfg.Host = "127.0.0.1"
	cfg.EventsCoalesceInterval = 0
	path := writeRestartSession(t, cfg, "visible-seed")
	var seed bytes.Buffer
	require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.NewSessionBuilder().AddClaudeUser("2026-01-02T00:00:03Z", "partial-commit").String())
	require.NoError(t, err)
	require.NoError(t, f.Close())
	release := make(chan struct{})
	restore := stubLaunchSyncWorker(t, func(ctx context.Context, cfg config.Config, req syncWorkerRequest, _ func(workerLine)) (workerResult, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return workerResult{}, ctx.Err()
		}
		var out bytes.Buffer
		if err := runSyncWorkerContext(ctx, cfg, req, &out); err != nil {
			return workerResult{}, err
		}
		return workerResult{}, errors.New("injected worker failure")
	})
	t.Cleanup(restore)
	base := startRestartServe(t, cfg, serveOptions{})
	messagesURL := base + "/api/v1/sessions/visible-seed/messages"
	require.NotContains(t, restartGet(t, messagesURL), "partial-commit")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
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
		if strings.Contains(scanner.Text(), `"scope":"sync"`) {
			found = true
			break
		}
	}
	require.True(t, found, "failed worker should invalidate session details: %v", scanner.Err())
	require.Contains(t, restartGet(t, messagesURL), "partial-commit")
}

func TestServeStartupWorkerWithoutChangesDoesNotEmitSessions(t *testing.T) {
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
		return workerResult{Status: "ok", DiscoveryComplete: true}, nil
	})
	t.Cleanup(restore)
	base := startRestartServe(t, cfg, serveOptions{})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
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
		return strings.Contains(restartGet(t, base+"/api/v1/sync/status"), `"startup_pending":false`)
	}, 2*time.Second, 100*time.Millisecond)
	for scanner.Scan() {
		assert.NotContains(t, scanner.Text(), `"scope":"sessions"`)
	}
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
	base := startRestartServe(t, cfg, serveOptions{})
	require.Eventually(t, func() bool {
		return strings.Contains(restartGet(t, base+"/api/v1/sync/status"), `"startup_pending":false`)
	}, 30*time.Second, 100*time.Millisecond)
	select {
	case <-launched:
		require.FailNow(t, "startup worker ran during first import")
	default:
	}
}
