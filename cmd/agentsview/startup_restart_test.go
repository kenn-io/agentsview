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
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestDailyUsageServesSavedTotalsDuringScopedUpgrade(t *testing.T) {
	isolateServeTestHome(t)
	t.Setenv(syncWorkerChildEnvVar, "1")
	cfg := testConfigWithClaudeFixture(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", cfg.DataDir)
	cfg.Host = "127.0.0.1"
	cfg.SyncIdleAfter = 100 * time.Millisecond
	content := testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-01-02T00:00:00Z", "hello").
		AddClaudeAssistantUsage("2026-01-02T00:00:01Z", "reply", testjsonl.ClaudeAssistantUsage{
			MessageID: "message-a", RequestID: "request-a", Model: "claude-sonnet-4-5", InputTokens: 1234, OutputTokens: 42,
		}).String()
	require.NoError(t, os.WriteFile(filepath.Join(cfg.AgentDirs[parser.AgentClaude][0], "-home-proj0", "usage.jsonl"), []byte(content), 0o644))
	var seed bytes.Buffer
	require.NoError(t, runSyncWorkerContext(t.Context(), cfg, syncWorkerRequest{Mode: "startup"}, &seed))
	oldVersion := db.CurrentDataVersion() - 1
	require.NotEmpty(t, db.ScopedDataVersionProviders(oldVersion, db.CurrentDataVersion()))
	conn, err := sql.Open("sqlite3", cfg.DBPath)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), fmt.Sprintf(`PRAGMA user_version=%d`, oldVersion))
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	started, release := make(chan struct{}), make(chan struct{})
	restore := stubLaunchSyncWorker(t, func(ctx context.Context, _ config.Config, _ syncWorkerRequest, _ func(workerLine)) (workerResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return workerResult{}, ctx.Err()
		}
		return workerResult{}, nil
	})
	t.Cleanup(restore)
	base := startRestartServe(t, cfg, serveOptions{SkipInitialSync: true})
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		require.FailNow(t, "worker did not start")
	}
	require.Eventually(t, func() bool {
		return strings.Contains(restartGet(t, base+"/api/v1/sync/status"), `"sync_pending":true`)
	}, 30*time.Second, 100*time.Millisecond)
	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- executeCLIWithLegacyFlagCompat([]string{"report", "usage", "daily", "--all", "--json", "--timezone", "UTC"}, &stdout, &stderr)
	}()
	select {
	case err := <-done:
		close(release)
		require.NoError(t, err)
	case <-time.After(time.Second): //nolint:kennlint // deadlinetest: the report must return before the held worker's catch-up deadline
		close(release)
		<-done
		require.FailNow(t, "daily report waited for the startup worker")
	}
	assert.Contains(t, stdout.String(), `"inputTokens": 1234`)
	assert.Contains(t, stderr.String(), apiclient.ArchiveStaleWarning)
}

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
		name                          string
		scoped, skip, fail, persisted bool
	}{
		{name: "ordinary"},
		{name: "scoped upgrade", scoped: true},
		{name: "scoped upgrade with skip initial sync", scoped: true, skip: true},
		{name: "persisted scoped upgrade with skip initial sync", scoped: true, skip: true, persisted: true},
		{name: "scoped upgrade with incomplete pass", scoped: true, fail: true},
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
				oldVersion := db.CurrentDataVersion() - 1
				require.NotEmpty(t, db.ScopedDataVersionProviders(oldVersion, db.CurrentDataVersion()),
					"fixture must exercise a scoped parser upgrade")
				conn, err := sql.Open("sqlite3", cfg.DBPath)
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), `UPDATE messages SET content='old transcript' WHERE session_id IN (SELECT id FROM sessions WHERE agent='claude') AND content LIKE 'visible-seed-%'`)
				require.NoError(t, err)
				if tc.persisted {
					_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET data_version=? WHERE agent='claude'`, oldVersion)
					require.NoError(t, err)
					_, err = conn.ExecContext(t.Context(), `INSERT OR REPLACE INTO stats(key, value) VALUES ('scoped_upgrade_pending', ?)`, db.CurrentDataVersion())
					require.NoError(t, err)
					var version int
					require.NoError(t, conn.QueryRowContext(t.Context(), `PRAGMA user_version`).Scan(&version))
					require.Equal(t, db.CurrentDataVersion(), version)
				} else {
					_, err = conn.ExecContext(t.Context(), `UPDATE sessions SET data_version=?`, oldVersion)
					require.NoError(t, err)
					_, err = conn.ExecContext(t.Context(), fmt.Sprintf(`PRAGMA user_version=%d`, oldVersion))
					require.NoError(t, err)
				}
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
				if tc.fail {
					stats := syncpkg.SyncStats{Aborted: true}
					return workerResult{
						Status: "aborted", Error: "injected incomplete startup pass",
						Stats: &stats,
					}, errors.New("injected incomplete startup pass")
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
			statusBody := restartGet(t, base+"/api/v1/sync/status")
			require.Contains(t, statusBody, `"startup_pending":true`)
			if tc.scoped {
				var status struct {
					ArchiveStale   bool `json:"archive_stale"`
					UpgradePending bool `json:"upgrade_pending"`
					WritesPaused   bool `json:"writes_paused"`
				}
				require.NoError(t, json.Unmarshal([]byte(statusBody), &status))
				assert.False(t, status.ArchiveStale)
				assert.True(t, status.UpgradePending)
				assert.True(t, status.WritesPaused)
			}
			codexBefore := make(map[string][]string)
			for _, id := range codexIDs {
				contents := restartContents(t, base+"/api/v1/sessions/"+id+"/messages")
				require.Contains(t, contents, "unchanged codex transcript")
				codexBefore[id] = contents
			}
			if tc.scoped {
				for _, id := range claudeIDs {
					response, err := restartTestRequest(t, http.MethodGet,
						base+"/api/v1/sessions/"+id+"/messages", "", "")
					require.NoError(t, err)
					assert.Equal(t, "true", response.Header.Get("X-Agentsview-Archive-Stale"))
					require.NoError(t, response.Body.Close())
					contents := restartContents(t, base+"/api/v1/sessions/"+id+"/messages")
					require.Contains(t, contents, "old transcript")
					assert.NotContains(t, contents, id)
				}
			}
			if tc.fail {
				close(release)
				require.Eventually(t, func() bool {
					status := restartGet(t, base+"/api/v1/sync/status")
					return strings.Contains(status, `"startup_pending":true`) &&
						strings.Contains(status, `"aborted":true`) &&
						strings.Contains(status, `"upgrade_pending":true`) &&
						strings.Contains(status, `"writes_paused":false`)
				}, 30*time.Second, 100*time.Millisecond)
				response, err := restartTestRequest(t, http.MethodGet,
					base+"/api/v1/sessions/"+claudeIDs[0]+"/messages", "", "")
				require.NoError(t, err)
				assert.Equal(t, "true", response.Header.Get("X-Agentsview-Archive-Stale"))
				require.NoError(t, response.Body.Close())
				require.NotEmpty(t, restartContents(t,
					base+"/api/v1/sessions/"+claudeIDs[0]+"/messages"),
					"saved transcript remains readable after the incomplete pass")
				response, err = restartTestRequest(t, http.MethodPut,
					base+"/api/v1/sessions/missing/star", "{}", "")
				require.NoError(t, err)
				assert.Equal(t, http.StatusNotFound, response.StatusCode,
					"writes should resume when the incomplete worker releases the writer")
				require.NoError(t, response.Body.Close())
				return
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
				status := restartGet(t, base+"/api/v1/sync/status")
				return err == nil && !stale && strings.Contains(status, `"startup_pending":false`) &&
					strings.Contains(status, `"upgrade_pending":false`) &&
					strings.Contains(status, `"writes_paused":false`)
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
