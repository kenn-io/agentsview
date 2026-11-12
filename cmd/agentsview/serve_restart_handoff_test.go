package main

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"image/color"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

func TestServeRestartArgsPreserveLaunchOptions(t *testing.T) {
	flags := newServeCommand().Flags()
	require.NoError(t, flags.Parse([]string{"--host=127.0.0.1", "--base-path=/example", "--public-origin=https://example.com", "--public-origin=https://example.org", "--allowed-subnet=192.0.2.0/24", "--no-sync", "--background", "--port=1234", "--skip-initial-sync"}))
	args := serveRestartArgs(flags)
	assert.Equal(t, []string{"--allowed-subnet=192.0.2.0/24", "--base-path=/example", "--host=127.0.0.1", "--no-sync=true", "--public-origin=https://example.com,https://example.org", "--skip-initial-sync=true"}, args)
	replacement := newServeCommand()
	require.NoError(t, replacement.Flags().Parse(args))
	skip, err := replacement.Flags().GetBool("skip-initial-sync")
	require.NoError(t, err)
	assert.True(t, skip)
}

// isolateServeTestHome points every config, data, and agent directory into a
// fresh temp dir and returns it with the data dir inside it.
func isolateServeTestHome(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	dataDir := filepath.Join(dir, "data")
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_TEST_REAL_CLI", "1")
	for _, agent := range parser.Registry {
		if agent.EnvVar != "" {
			t.Setenv(agent.EnvVar, filepath.Join(dir, "empty", string(agent.Type)))
		}
		if agent.DefaultRootEnvVar != "" {
			t.Setenv(agent.DefaultRootEnvVar, filepath.Join(dir, "empty-root", string(agent.Type)))
		}
	}
	return dir, dataDir
}

// startRestartFixture starts an isolated server and stops it at cleanup. The
// returned channel yields the foreground process's exit, then stays closed.
func startRestartFixture(t *testing.T, dataDir, mode string, args ...string) (*os.Process, <-chan error) {
	t.Helper()
	args = append([]string{"serve", "--host=127.0.0.1", "--port=0", "--no-browser", "--no-update-check"}, args...)
	processCtx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	t.Cleanup(cancel)
	var done chan error
	var process *os.Process
	if mode == "background" {
		out, err := exec.CommandContext(processCtx, os.Args[0], append(args, "--background")...).CombinedOutput()
		require.NoError(t, err, "%s", out)
	} else {
		cmd := exec.CommandContext(processCtx, os.Args[0], args...)
		configureRestartTestProcess(cmd)
		require.NoError(t, cmd.Start())
		process = cmd.Process
		done = make(chan error, 1)
		go func() { done <- cmd.Wait(); close(done) }()
	}
	t.Cleanup(func() {
		if mode == "background" {
			waitForBackgroundLaunchLock(t, dataDir)
		}
		ctx, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer stop()
		cfg, err := config.Load(nil)
		assert.NoError(t, err)
		_, err = stopWritableDaemonsForUpdate(ctx, config.Config{DataDir: dataDir, AuthToken: cfg.AuthToken})
		assert.NoError(t, err)
		if done != nil {
			select {
			case <-done:
			case <-ctx.Done():
				assert.Fail(t, "foreground server did not stop")
			}
		}
	})
	return process, done
}

// waitForBackgroundLaunchLock waits until a restarted background server has
// released the launch lock it holds while starting.
func waitForBackgroundLaunchLock(t *testing.T, dataDir string) {
	t.Helper()
	require.Eventually(t, func() bool {
		lock, acquired := acquireBackgroundLaunchLock(dataDir)
		if acquired {
			assert.NoError(t, lock.Unlock())
		}
		return acquired
	}, 30*time.Second, 100*time.Millisecond)
}

func waitForRestartRuntime(t *testing.T, dataDir string, token ...string) *DaemonRuntime {
	t.Helper()
	var rt *DaemonRuntime
	require.Eventually(t, func() bool { rt = FindDaemonRuntime(dataDir, token...); return rt != nil }, 30*time.Second, 100*time.Millisecond)
	return rt
}

func restartTestRequest(t *testing.T, method, url, body, token string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Origin", strings.Join(strings.SplitN(url, "/", 4)[:3], "/"))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	// Never reuse a connection to a server that may have just shut down.
	return (&http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
}

// restartTestSend sends a request that must succeed with the given status.
func restartTestSend(t *testing.T, method, url, body, token string, status int) {
	t.Helper()
	resp, err := restartTestRequest(t, method, url, body, token)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, status, resp.StatusCode, "%s", data)
}

// restartTestInstance returns the answering server's instance, or "" when no
// server answers with the given token.
func restartTestInstance(t *testing.T, base, token string) string {
	t.Helper()
	resp, err := restartTestRequest(t, http.MethodGet, base+"/api/ping", "", token)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var ping struct {
		InstanceID string `json:"instance_id"`
	}
	if resp.StatusCode != http.StatusOK || json.UnmarshalRead(resp.Body, &ping) != nil {
		return ""
	}
	return ping.InstanceID
}

// restartAndWait requests a restart and returns the replacement once it
// answers and has published its runtime.
func restartAndWait(t *testing.T, dataDir, base, token string) *DaemonRuntime {
	t.Helper()
	previous := restartTestInstance(t, base, token)
	require.NotEmpty(t, previous)
	oldRuntime := waitForRestartRuntime(t, dataDir, token)
	restartTestSend(t, http.MethodPost, base+"/api/v1/daemon/restart", "", token, http.StatusOK)
	var runtime *DaemonRuntime
	require.Eventually(t, func() bool {
		replacement := restartTestInstance(t, base, token)
		runtime = FindDaemonRuntime(dataDir, token)
		return replacement != "" && replacement != previous && runtime != nil && runtime.Record.StartedAt.After(oldRuntime.Record.StartedAt)
	}, 30*time.Second, 100*time.Millisecond)
	return runtime
}

func TestServeRestartHandoffAppliesImagePolicy(t *testing.T) {
	for _, mode := range []string{"foreground", "background"} {
		t.Run(mode, func(t *testing.T) {
			dir, dataDir := isolateServeTestHome(t)
			dir, err := filepath.EvalSymlinks(dir)
			require.NoError(t, err)
			codexDir := filepath.Join(dir, "empty", string(parser.AgentCodex))
			require.NoError(t, os.MkdirAll(filepath.Join(codexDir, "2024", "01", "01"), 0o755))
			process, done := startRestartFixture(t, dataDir, mode, "--base-path=/example", "--skip-initial-sync", "--require-auth=false")
			rt := waitForRestartRuntime(t, dataDir)
			if mode == "background" {
				output, err := exec.CommandContext(t.Context(), os.Args[0], "daemon", "start").CombinedOutput()
				require.NoError(t, err, "%s", output)
				rt = waitForRestartRuntime(t, dataDir)
				require.True(t, rt.Persistent)
			}
			base := fmt.Sprintf("http://127.0.0.1:%d/example", rt.Port)
			restartTestSend(t, http.MethodPut, base+"/api/v1/settings", `{"tool_result_images":"drop"}`, "", http.StatusOK)
			resp, err := restartTestRequest(t, http.MethodGet, base+"/api/v1/settings", "", "")
			require.NoError(t, err)
			var settings struct {
				AuthToken        string `json:"auth_token"`
				RestartSupported bool   `json:"restart_supported"`
			}
			require.NoError(t, json.UnmarshalRead(resp.Body, &settings))
			require.NoError(t, resp.Body.Close())
			assert.True(t, settings.RestartSupported)
			// Auth applies live; the replacement keeps it over the launch flag.
			restartTestSend(t, http.MethodPut, base+"/api/v1/settings", `{"require_auth":true}`, "", http.StatusOK)
			token := settings.AuthToken
			replacement := restartAndWait(t, dataDir, base, token)
			assert.Equal(t, rt.Record.PID, replacement.Record.PID)
			assert.Equal(t, rt.Port, replacement.Port)
			assert.Equal(t, "/example", replacement.BasePath)
			assert.True(t, replacement.RequireAuth)
			assert.Equal(t, rt.Persistent, replacement.Persistent)

			// A later file edit wins after Settings retired the original flag.
			disk, err := config.Load(nil)
			require.NoError(t, err)
			require.NoError(t, disk.SaveSettings(map[string]any{"require_auth": false}))
			if mode == "background" {
				waitForBackgroundLaunchLock(t, dataDir)
			}
			restartAndWait(t, dataDir, base, token)
			assert.NotEmpty(t, restartTestInstance(t, base, ""))

			const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b07"
			png := testjsonl.SolidPNGBase64(4, 3, color.RGBA{R: 30, G: 120, B: 200, A: 255})
			content := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "user", "2024-01-01T10:00:00Z"),
				testjsonl.CodexMsgJSON("user", "draw a blue square", "2024-01-01T10:00:01Z"),
				testjsonl.CodexImageGenerationCallJSON("ig_1", "A blue square", png, "2024-01-01T10:00:02Z"),
			)
			path := filepath.Join(codexDir, "2024", "01", "01", "rollout-2024-01-01T10-00-00-"+uuid+".jsonl")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
			body, err := json.Marshal(map[string]string{"path": path})
			require.NoError(t, err)
			restartTestSend(t, http.MethodPost, base+"/api/v1/sessions/sync", string(body), "", http.StatusOK)
			database, err := db.OpenReadOnly(t.Context(), filepath.Join(dataDir, "sessions.db"))
			require.NoError(t, err)
			defer func() { _ = database.Close() }()
			page, err := database.ListSessions(t.Context(), db.SessionFilter{})
			require.NoError(t, err)
			var found bool
			for _, session := range page.Sessions {
				messages, err := database.GetAllMessages(t.Context(), session.ID)
				require.NoError(t, err)
				for _, message := range messages {
					for _, call := range message.ToolCalls {
						if call.ToolUseID == "ig_1" {
							found = true
							assert.NotContains(t, call.ResultContent, "input_image")
							assert.NotContains(t, call.ResultContent, "data:image")
							require.NotEmpty(t, call.ResultEvents)
							for _, event := range call.ResultEvents {
								assert.NotContains(t, event.Content, "input_image")
							}
						}
					}
				}
			}
			assert.True(t, found, "fixture must reach the tool-result tables")
			if mode == "foreground" {
				require.NoError(t, database.Close())
				require.NoError(t, interruptRestartTestProcess(process))
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(30 * time.Second):
					require.FailNow(t, "foreground did not exit after interrupt")
				}
			}
		})
	}
}

func TestServeRestartRefusesInvalidConfig(t *testing.T) {
	_, dataDir := isolateServeTestHome(t)
	startRestartFixture(t, dataDir, "foreground", "--no-sync")
	rt := waitForRestartRuntime(t, dataDir)
	path := filepath.Join(dataDir, "config.toml")
	saved, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("[invalid TOML"), 0o600))
	resp, err := restartTestRequest(t, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/api/v1/daemon/restart", rt.Port), "", "")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	assert.Contains(t, string(body), "parsing config")
	lock, acquired := acquireBackgroundLaunchLock(dataDir)
	require.True(t, acquired, "refused restart must release launch ownership")
	require.NoError(t, lock.Unlock())
	require.NoError(t, os.WriteFile(path, saved, 0o600))
	current := FindDaemonRuntime(dataDir)
	require.NotNil(t, current)
	assert.Equal(t, rt.Record.PID, current.Record.PID)
}

func TestRestartKeepsAdoptedNoSync(t *testing.T) {
	_, dataDir := isolateServeTestHome(t)
	startRestartFixture(t, dataDir, "background", "--no-sync")
	old := waitForRestartRuntime(t, dataDir)
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "serve", "--replace", "--host=127.0.0.1", "--port=0", "--no-browser", "--no-update-check")
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer stop()
		_, _ = stopWritableDaemonsForUpdate(stopCtx, config.Config{DataDir: dataDir})
		select {
		case <-done:
		case <-stopCtx.Done():
			assert.Fail(t, "replacement did not exit")
		}
	})
	var current *DaemonRuntime
	require.Eventually(t, func() bool {
		current = FindDaemonRuntime(dataDir)
		return current != nil && current.Record.PID != old.Record.PID
	}, 30*time.Second, 100*time.Millisecond)
	require.True(t, current.NoSync)
	current = restartAndWait(t, dataDir, fmt.Sprintf("http://127.0.0.1:%d", current.Port), "")
	assert.True(t, current.NoSync)
}

func TestRestartClosesStalledRequest(t *testing.T) {
	for _, mode := range []string{"foreground", "background"} {
		t.Run(mode, func(t *testing.T) {
			_, dataDir := isolateServeTestHome(t)
			startRestartFixture(t, dataDir, mode, "--no-sync", "--base-path=/example")
			rt := waitForRestartRuntime(t, dataDir)
			base := fmt.Sprintf("http://127.0.0.1:%d/example", rt.Port)
			conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", fmt.Sprintf("127.0.0.1:%d", rt.Port))
			require.NoError(t, err)
			defer conn.Close()
			_, err = fmt.Fprintf(conn, "PUT /example/api/v1/settings HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nOrigin: http://127.0.0.1:%d\r\nContent-Type: application/json\r\nContent-Length: 1000\r\nExpect: 100-continue\r\n\r\n", rt.Port, rt.Port)
			require.NoError(t, err)
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
			reader := bufio.NewReader(conn)
			line, err := reader.ReadString('\n')
			require.NoError(t, err)
			require.Contains(t, line, "100 Continue")
			line, err = reader.ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, "\r\n", line)
			_, err = io.WriteString(conn, "{")
			require.NoError(t, err)
			require.NoError(t, conn.SetReadDeadline(time.Time{}))
			previous := restartTestInstance(t, base, "")
			restartTestSend(t, http.MethodPost, base+"/api/v1/daemon/restart", "", "", http.StatusOK)
			lock, acquired := acquireBackgroundLaunchLock(dataDir)
			if acquired {
				require.NoError(t, lock.Unlock())
			}
			assert.False(t, acquired, "restart must reserve the launch lock while the old server drains")

			type result struct {
				runtime *DaemonRuntime
				err     error
			}
			resultCh := make(chan result, 1)
			go func() {
				cfg, err := config.Load(nil)
				if err != nil {
					resultCh <- result{err: err}
					return
				}
				current, err := ensureBackgroundServe(t.Context(), &cfg, 30*time.Second, false)
				resultCh <- result{runtime: current, err: err}
			}()
			require.Eventually(t, func() bool {
				current := restartTestInstance(t, base, "")
				return current != "" && current != previous
			}, 30*time.Second, 100*time.Millisecond)
			outcome := <-resultCh
			require.NoError(t, outcome.err)
			require.NotNil(t, outcome.runtime)
			assert.Equal(t, rt.Record.PID, outcome.runtime.Record.PID)
			assert.Equal(t, "/example", outcome.runtime.BasePath)
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
			body, err := io.ReadAll(reader)
			require.NoError(t, err)
			assert.Empty(t, body)
		})
	}
}

func TestServeRestartPortChoiceWhenOccupied(t *testing.T) {
	dataDir := testDataDir(t)
	t.Setenv("AGENTSVIEW_TEST_REAL_CLI", "1")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, listener.Close()) })
	port := listener.Addr().(*net.TCPAddr).Port
	processCtx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	defer cancel()
	child := exec.CommandContext(processCtx, os.Args[0], "serve", "--host=127.0.0.1", "--restart-port="+strconv.Itoa(port), "--no-sync", "--no-browser", "--no-update-check")
	require.NoError(t, child.Start())
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	t.Cleanup(func() {
		ctx, stop := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer stop()
		_, err := stopWritableDaemonsForUpdate(ctx, config.Config{DataDir: dataDir})
		assert.NoError(t, err)
		select {
		case <-done:
		case <-ctx.Done():
			assert.Fail(t, "replacement did not exit")
		}
	})
	var output []byte
	require.Eventually(t, func() bool {
		output, err = exec.CommandContext(t.Context(), os.Args[0], "serve", "status").CombinedOutput()
		return err == nil && strings.Contains(string(output), "agentsview running at ")
	}, 30*time.Second, 100*time.Millisecond)
	line := strings.SplitN(string(output), "agentsview running at ", 2)[1]
	address := strings.Fields(line)[0]
	assert.NotContains(t, address, ":"+strconv.Itoa(port))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, address+"/api/ping", nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func startRestartAuthFixture(t *testing.T, mode string, args ...string) (config.Config, string, string) {
	t.Helper()
	_, dataDir := isolateServeTestHome(t)
	startRestartFixture(t, dataDir, mode, append([]string{"--no-sync"}, args...)...)
	cfg, err := config.Load(nil)
	require.NoError(t, err)
	require.NoError(t, cfg.EnsureAuthToken())
	token := cfg.AuthToken
	return cfg, dataDir, token
}

func checkServeRestartUsesEditedAuthConfig(t *testing.T) {
	t.Helper()
	for _, mode := range []string{"foreground", "background"} {
		t.Run(mode, func(t *testing.T) {
			cfg, dataDir, token := startRestartAuthFixture(t, mode)
			var rt *DaemonRuntime
			require.Eventually(t, func() bool { rt = FindDaemonRuntime(dataDir); return rt != nil }, 30*time.Second, 100*time.Millisecond)
			base := fmt.Sprintf("http://127.0.0.1:%d", rt.Port)
			require.NoError(t, cfg.SaveSettings(map[string]any{"require_auth": true}))
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/api/v1/daemon/restart", nil)
			require.NoError(t, err)
			req.Header.Set("Origin", base)
			client := &http.Client{Timeout: time.Second}
			resp, err := client.Do(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
			require.Eventually(t, func() bool {
				resp, err := restartTestPing(t, client, base)
				if err != nil {
					return false
				}
				defer resp.Body.Close()
				return resp.StatusCode == http.StatusUnauthorized
			}, 30*time.Second, 100*time.Millisecond)
			require.Eventually(t, func() bool { return FindDaemonRuntime(dataDir, token) != nil }, 30*time.Second, 100*time.Millisecond)
			req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/ping", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err = client.Do(req)
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
		})
	}
}

func TestServeRestartUsesEditedAuthAfterSettingsSave(t *testing.T) {
	t.Run("file edit with unretired launch flag", func(t *testing.T) {
		cfg, dataDir, _ := startRestartAuthFixture(t, "background", "--require-auth=false")
		rt := waitForRestartRuntime(t, dataDir)
		base := fmt.Sprintf("http://127.0.0.1:%d", rt.Port)
		require.NoError(t, cfg.SaveSettings(map[string]any{"require_auth": true}))
		replacement := restartAndWait(t, dataDir, base, "")
		assert.Equal(t, rt.Record.PID, replacement.Record.PID)
		assert.True(t, replacement.NoSync)
		assert.NotEmpty(t, restartTestInstance(t, base, ""))
		assert.False(t, replacement.RequireAuth)
	})
	t.Run("file edit without launch flag", checkServeRestartUsesEditedAuthConfig)
	for _, mode := range []string{"foreground", "background"} {
		t.Run(mode, func(t *testing.T) {
			cfg, dataDir, token := startRestartAuthFixture(t, mode, "--require-auth=true")
			var rt *DaemonRuntime
			require.Eventually(t, func() bool { rt = FindDaemonRuntime(dataDir, token); return rt != nil }, 30*time.Second, 100*time.Millisecond)
			base := fmt.Sprintf("http://127.0.0.1:%d", rt.Port)
			client := &http.Client{Timeout: time.Second}
			save, err := http.NewRequestWithContext(t.Context(), http.MethodPut, base+"/api/v1/settings", strings.NewReader(`{"require_auth":false}`))
			require.NoError(t, err)
			save.Header.Set("Origin", base)
			save.Header.Set("Content-Type", "application/json")
			save.Header.Set("Authorization", "Bearer "+token)
			saved, err := client.Do(save)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, saved.StatusCode)
			require.NoError(t, saved.Body.Close())
			visible, err := restartTestPing(t, client, base)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, visible.StatusCode)
			var first struct {
				InstanceID string `json:"instance_id"`
			}
			require.NoError(t, json.UnmarshalRead(visible.Body, &first))
			require.NoError(t, visible.Body.Close())
			restart, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/api/v1/daemon/restart", nil)
			require.NoError(t, err)
			restart.Header.Set("Origin", base)
			started, err := client.Do(restart)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, started.StatusCode)
			require.NoError(t, started.Body.Close())
			var secondInstance string
			require.Eventually(t, func() bool {
				response, err := restartTestPing(t, client, base)
				if err != nil {
					return false
				}
				defer response.Body.Close()
				var ping struct {
					InstanceID string `json:"instance_id"`
				}
				if response.StatusCode == http.StatusOK && json.UnmarshalRead(response.Body, &ping) == nil && ping.InstanceID != "" && ping.InstanceID != first.InstanceID {
					secondInstance = ping.InstanceID
					return true
				}
				return false
			}, 30*time.Second, 100*time.Millisecond)
			if mode == "background" {
				waitForBackgroundLaunchLock(t, dataDir)
			}
			save, err = http.NewRequestWithContext(t.Context(), http.MethodPut, base+"/api/v1/settings", strings.NewReader(`{"require_auth":true}`))
			require.NoError(t, err)
			save.Header.Set("Origin", base)
			save.Header.Set("Content-Type", "application/json")
			saved, err = client.Do(save)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, saved.StatusCode)
			require.NoError(t, saved.Body.Close())
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/api/v1/daemon/restart", nil)
			require.NoError(t, err)
			req.Header.Set("Origin", base)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := client.Do(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
			req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/ping", nil)
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+token)
			require.Eventually(t, func() bool {
				resp, err := client.Do(req)
				if err != nil {
					return false
				}
				defer resp.Body.Close()
				var ping struct {
					InstanceID string `json:"instance_id"`
				}
				return resp.StatusCode == http.StatusOK && json.UnmarshalRead(resp.Body, &ping) == nil && ping.InstanceID != "" && ping.InstanceID != secondInstance
			}, 30*time.Second, 100*time.Millisecond)
			resp, err = restartTestPing(t, client, base)
			require.NoError(t, err)
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
			if mode == "background" {
				waitForBackgroundLaunchLock(t, dataDir)
			}
			require.NoError(t, cfg.SaveSettings(map[string]any{"require_auth": false}))
			req, err = http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/api/v1/daemon/restart", nil)
			require.NoError(t, err)
			req.Header.Set("Origin", base)
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err = client.Do(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
			require.Eventually(t, func() bool {
				resp, err := restartTestPing(t, client, base)
				if err != nil {
					return false
				}
				defer resp.Body.Close()
				return resp.StatusCode == http.StatusOK
			}, 30*time.Second, 100*time.Millisecond)
			if mode == "background" {
				waitForBackgroundLaunchLock(t, dataDir)
			}
			require.NoError(t, cfg.SaveSettings(map[string]any{"require_auth": true}))
			req, err = http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/api/v1/daemon/restart", nil)
			require.NoError(t, err)
			req.Header.Set("Origin", base)
			resp, err = client.Do(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
			require.Eventually(t, func() bool {
				resp, err := restartTestPing(t, client, base)
				if err != nil {
					return false
				}
				defer resp.Body.Close()
				return resp.StatusCode == http.StatusUnauthorized
			}, 30*time.Second, 100*time.Millisecond)
			require.Eventually(t, func() bool {
				rt := FindDaemonRuntime(dataDir, token)
				return rt != nil && rt.RequireAuth
			}, 30*time.Second, 100*time.Millisecond)
		})
	}
}

func TestRestartPreservesDeploymentAuthOverride(t *testing.T) {
	for _, mode := range []string{"foreground", "background"} {
		for _, initial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/initial-%t", mode, initial), func(t *testing.T) {
				t.Setenv("AGENTSVIEW_MODE", "serve")
				t.Setenv("AGENTSVIEW_REQUIRE_AUTH", strconv.FormatBool(initial))
				cfg, dataDir, token := startRestartAuthFixture(t, mode)
				rt := waitForRestartRuntime(t, dataDir, token)
				base := fmt.Sprintf("http://127.0.0.1:%d", rt.Port)
				body := fmt.Sprintf(`{"require_auth":%t}`, !initial)
				restartTestSend(t, http.MethodPut, base+"/api/v1/settings", body, token, http.StatusOK)
				want := http.StatusOK
				if !initial {
					want = http.StatusUnauthorized
				}
				restartTestSend(t, http.MethodGet, base+"/api/ping", "", "", want)
				replacement := restartAndWait(t, dataDir, base, token)
				assert.Equal(t, !initial, replacement.RequireAuth)
				restartTestSend(t, http.MethodGet, base+"/api/ping", "", "", want)
				restartTestSend(t, http.MethodGet, base+"/api/ping", "", token, http.StatusOK)
				if mode == "background" {
					waitForBackgroundLaunchLock(t, dataDir)
				}
				replacement = restartAndWait(t, dataDir, base, token)
				assert.Equal(t, !initial, replacement.RequireAuth)
				restartTestSend(t, http.MethodGet, base+"/api/ping", "", "", want)
				restartTestSend(t, http.MethodGet, base+"/api/ping", "", token, http.StatusOK)
				if mode == "background" {
					waitForBackgroundLaunchLock(t, dataDir)
				}
				require.NoError(t, cfg.SaveSettings(map[string]any{"require_auth": initial}))
				replacement = restartAndWait(t, dataDir, base, token)
				assert.Equal(t, initial, replacement.RequireAuth)
			})
		}
	}
}

func TestRestartDeploymentAuthSaveAuthority(t *testing.T) {
	for _, mode := range []string{"foreground", "background"} {
		for _, scenario := range []string{"untouched", "matching save", "external restart"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				t.Setenv("AGENTSVIEW_MODE", "serve")
				t.Setenv("AGENTSVIEW_REQUIRE_AUTH", "false")
				cfg, dataDir, token := startRestartAuthFixture(t, mode)
				rt := waitForRestartRuntime(t, dataDir, token)
				base := fmt.Sprintf("http://127.0.0.1:%d", rt.Port)
				if scenario == "external restart" {
					restartTestSend(t, http.MethodPut, base+"/api/v1/settings", `{"require_auth":true}`, token, http.StatusOK)
					output, err := exec.CommandContext(t.Context(), os.Args[0], "daemon", "restart").CombinedOutput()
					require.NoError(t, err, "%s", output)
					replacement := waitForRestartRuntime(t, dataDir, token)
					assert.False(t, replacement.RequireAuth)
					restartTestSend(t, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/ping", replacement.Port), "", "", http.StatusOK)
					return
				}
				if scenario == "matching save" {
					restartTestSend(t, http.MethodPut, base+"/api/v1/settings", `{"require_auth":false}`, token, http.StatusOK)
				}
				require.NoError(t, cfg.SaveSettings(map[string]any{"require_auth": true}))
				replacement := restartAndWait(t, dataDir, base, token)
				assert.Equal(t, scenario == "matching save", replacement.RequireAuth)
			})
		}
		t.Run(mode+"/external restart with auth", func(t *testing.T) {
			t.Setenv("AGENTSVIEW_MODE", "serve")
			t.Setenv("AGENTSVIEW_REQUIRE_AUTH", "true")
			_, dataDir, token := startRestartAuthFixture(t, mode)
			rt := waitForRestartRuntime(t, dataDir, token)
			base := fmt.Sprintf("http://127.0.0.1:%d", rt.Port)
			restartTestSend(t, http.MethodPut, base+"/api/v1/settings", `{"require_auth":false}`, token, http.StatusOK)
			output, err := exec.CommandContext(t.Context(), os.Args[0], "daemon", "restart").CombinedOutput()
			require.NoError(t, err, "%s", output)
			replacement := waitForRestartRuntime(t, dataDir, token)
			assert.True(t, replacement.RequireAuth)
			restartTestSend(t, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/ping", replacement.Port), "", "", http.StatusUnauthorized)
		})
	}
}

func restartTestPing(t *testing.T, client *http.Client, base string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/ping", nil)
	require.NoError(t, err)
	return client.Do(req)
}

func TestServeHandoffPortArgs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		port     int
		explicit *int
		want     []string
	}{
		{name: "fixed", port: 8080, explicit: new(9090), want: []string{"--port", "9090"}},
		{name: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, serveHandoffPortArgs(tc.port, tc.explicit)) })
	}
}
