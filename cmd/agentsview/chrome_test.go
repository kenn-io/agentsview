package main

import (
	"context"
	"encoding/json/v2"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/chromehost"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
)

func chromeTestExecutable(t *testing.T) string {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "bin with spaces", "agentsview")
	program := "#!/bin/sh\nprintf '%s\\n' \"$@\"\n"
	if runtime.GOOS == "windows" {
		executable += ".cmd"
		program = "@echo off\r\necho %~1\r\necho %~2\r\necho %~3\r\n"
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(executable), 0700))
	require.NoError(t, os.WriteFile(executable, []byte(program), 0700))
	return executable
}

func TestChromeSetup(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	executable := chromeTestExecutable(t)
	assets := fstest.MapFS{
		"chrome-extension/manifest.json": {Data: []byte(`{"key":"dGVzdA=="}`)},
		"chrome-extension/worker.js":     {Data: []byte("worker")},
	}
	var registered string
	folder, err := setupChrome(dir, home, executable, assets, func(path string) error { registered = path; return nil })
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "chrome", "extension"), folder)
	worker, err := os.ReadFile(filepath.Join(folder, "worker.js"))
	require.NoError(t, err)
	assert.Equal(t, "worker", string(worker))
	body, err := os.ReadFile(registered)
	require.NoError(t, err)
	var manifest struct {
		Name           string   `json:"name"`
		Path           string   `json:"path"`
		Type           string   `json:"type"`
		AllowedOrigins []string `json:"allowed_origins"`
	}
	require.NoError(t, json.Unmarshal(body, &manifest))
	assert.Equal(t, "io.kenn.agentsview", manifest.Name)
	assert.Equal(t, "stdio", manifest.Type)
	assert.Equal(t, []string{"chrome-extension://jpignaibiiemhngfjkcpokkamffknabf/"}, manifest.AllowedOrigins)
	assert.True(t, filepath.IsAbs(manifest.Path))
	command := exec.CommandContext(t.Context(), manifest.Path)
	if runtime.GOOS == "windows" {
		command = exec.CommandContext(t.Context(), "cmd", "/c", manifest.Path)
	}
	command.Dir = t.TempDir()
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	socket, err := chromeSocketPath(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"chrome-host", "--socket", socket}, strings.FieldsFunc(strings.TrimSpace(string(output)), func(r rune) bool { return r == '\r' || r == '\n' }))
	if runtime.GOOS == "linux" {
		assert.Equal(t, filepath.Join(home, ".config/google-chrome/NativeMessagingHosts/io.kenn.agentsview.json"), registered)
	}
	if runtime.GOOS == "darwin" {
		assert.Equal(t, filepath.Join(home, "Library/Application Support/Google/Chrome/NativeMessagingHosts/io.kenn.agentsview.json"), registered)
	}
}

func TestChromeSetupPreservesInstalledExtension(t *testing.T) {
	dir := t.TempDir()
	assets := fstest.MapFS{
		"chrome-extension/manifest.json": {Data: []byte(`{"key":"dGVzdA=="}`)},
		"chrome-extension/worker.js":     {Data: []byte("worker")},
	}
	executable := chromeTestExecutable(t)
	register := func(string) error { return nil }
	folder, err := setupChrome(dir, dir, executable, assets, register)
	require.NoError(t, err)
	_, err = setupChrome(dir, dir, executable, fstest.MapFS{}, register)
	require.Error(t, err)
	worker, err := os.ReadFile(filepath.Join(folder, "worker.js"))
	require.NoError(t, err)
	assert.Equal(t, "worker", string(worker))
}

func TestChromeHostRelay(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, safefileio.EnsurePrivateDir(dir))
	socket := filepath.Join(dir, "host.sock")
	listener, err := daemon.Listen(t.Context(), daemon.Endpoint{Network: daemon.NetworkUnix, Address: socket})
	require.NoError(t, err)
	defer listener.Close()
	input, writeInput := io.Pipe()
	defer input.Close()
	defer writeInput.Close()
	readOutput, output := io.Pipe()
	defer readOutput.Close()
	defer output.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- relayChromeHost(ctx, socket, input, output) }()
	conn, err := listener.Accept()
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	request := []byte(`{"id":"a","path":"/api/organizations"}`)
	require.NoError(t, chromehost.WriteFrame(conn, request))
	got, err := chromehost.ReadFrame(readOutput)
	require.NoError(t, err)
	assert.Equal(t, request, got)
	answer := []byte(`{"id":"a","status":200,"body":"[]"}`)
	require.NoError(t, chromehost.WriteFrame(writeInput, answer))
	got, err = chromehost.ReadFrame(conn)
	require.NoError(t, err)
	assert.Equal(t, answer, got)
	require.NoError(t, writeInput.Close())
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("relay did not exit on stdin EOF")
	}
}

func TestChromeHostRelayEOFWhileServerDown(t *testing.T) {
	require.NoError(t, relayChromeHost(t.Context(), filepath.Join(t.TempDir(), "missing.sock"), strings.NewReader(""), io.Discard))
}

func TestChromeHostRelayRetriesRefusedConnection(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, safefileio.EnsurePrivateDir(dir))
	socket := filepath.Join(dir, "host.sock")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	listener, err := daemon.Listen(ctx, daemon.Endpoint{Network: daemon.NetworkUnix, Address: socket})
	require.NoError(t, err)
	defer listener.Close()
	input, writeInput := io.Pipe()
	defer input.Close()
	defer writeInput.Close()
	readOutput, output := io.Pipe()
	defer readOutput.Close()
	defer output.Close()
	finished := make(chan error, 1)
	go func() { finished <- relayChromeHost(ctx, socket, input, output) }()
	first, err := listener.Accept()
	require.NoError(t, err)
	require.NoError(t, first.Close())
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	var second net.Conn
	select {
	case second = <-accepted:
	case <-ctx.Done():
		t.Fatal("refused relay did not reconnect")
	}
	defer second.Close()
	require.NoError(t, chromehost.WriteFrame(second, []byte(`{"id":"b","path":"/api/organizations"}`)))
	frame, err := chromehost.ReadFrame(readOutput)
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":"b","path":"/api/organizations"}`, string(frame))
	require.NoError(t, writeInput.Close())
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("relay did not exit")
	}
}

func TestChromeLongDataDirSetupAndServe(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("data", 25))
	socket, err := chromeSocketPath(dir)
	require.NoError(t, err)
	defer os.RemoveAll(filepath.Dir(socket))
	assert.Less(t, len(socket), 104)
	assert.NotEqual(t, filepath.Join(dir, "chrome", "host.sock"), socket)
	port, err := server.FindAvailablePort(t.Context(), "127.0.0.1", 0)
	require.NoError(t, err)
	cfg := config.Config{Host: "127.0.0.1", Port: port, DataDir: dir}
	srv := server.New(cfg, dbtest.OpenTestDB(t), nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	run, err := startServerWithOptionalCaddy(ctx, cfg, srv, serveRuntimeOptions{Mode: "serve"})
	require.NoError(t, err)
	defer func() {
		require.NoError(t, srv.Shutdown(t.Context()))
		require.ErrorIs(t, <-run.ServeErrCh, http.ErrServerClosed)
	}()
	assets := fstest.MapFS{"chrome-extension/manifest.json": {Data: []byte(`{"key":"dGVzdA=="}`)}}
	_, err = setupChrome(dir, t.TempDir(), chromeTestExecutable(t), assets, func(string) error { return nil })
	require.NoError(t, err)
	launcher := filepath.Join(dir, "chrome", "host")
	if runtime.GOOS == "windows" {
		launcher += ".cmd"
	}
	command := exec.CommandContext(t.Context(), launcher)
	if runtime.GOOS == "windows" {
		command = exec.CommandContext(t.Context(), "cmd", "/c", launcher)
	}
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	args := strings.FieldsFunc(strings.TrimSpace(string(output)), func(r rune) bool { return r == '\r' || r == '\n' })
	require.Len(t, args, 3)
	assert.Equal(t, []string{"chrome-host", "--socket", socket}, args)
	conn, err := net.Dial("unix", args[2])
	require.NoError(t, err)
	defer conn.Close()
	again, err := chromeSocketPath(dir)
	require.NoError(t, err)
	assert.Equal(t, socket, again)
}

func TestChromeSyncPartialFailureSummary(t *testing.T) {
	dataDir := testDataDir(t)
	t.Setenv("AGENTSVIEW_AUTH_TOKEN", "")
	ts := daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/import/claude-ai/sync": func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "chrome", r.URL.Query().Get("browser"))
			w.Header().Set("Content-Type", "text/event-stream")
			_, err := io.WriteString(w, "event: progress\ndata: {\"imported\":2,\"updated\":1,\"skipped\":3,\"errors\":1}\n\nevent: error\ndata: {\"error\":\"Sign in required\"}\n\n")
			assert.NoError(t, err)
		},
	})
	registerTestRuntime(t, dataDir, ts.URL, false)
	output := captureStderr(t, func() {
		cmd := newImportCommand()
		cmd.SetArgs([]string{"--type", "claude-ai", "--sync"})
		require.ErrorContains(t, cmd.ExecuteContext(t.Context()), "Sign in required")
	})
	assert.Contains(t, output, "Done: 6 processed (2 new, 1 updated, 3 skipped)")
	assert.Contains(t, output, "1 errors")
}

func TestChromeSyncResults(t *testing.T) {
	for _, tt := range []struct {
		name, body, wantError string
		status                int
		imported              int
	}{
		{"done", "event: progress\ndata: {}\n\nevent: done\ndata: {\"imported\":2,\"updated\":1,\"skipped\":3,\"errors\":0}\n\n", "", 200, 2},
		{"absent host", `{"code":"claude_ai_chrome_host_required","error":"Run agentsview chrome setup and keep Chrome open, then Sync again"}`, "Run agentsview chrome setup and keep Chrome open, then Sync again", 409, 0},
		{"error", "event: error\ndata: {\"error\":\"Sign in required\"}\n\n", "Sign in required", 200, 0},
		{"fetch", "event: fetch\ndata: {\"id\":\"a\",\"path\":\"/api/organizations\"}\n\n", "page relay", 200, 0},
		{"partial failure", "event:progress\ndata:{\"imported\":2,\"updated\":1,\"skipped\":3,\"errors\":1}\n\nevent:error\ndata:{\"error\":\"Sign in required\"}\n\n", "Sign in required", 200, 2},
		{"partial EOF", "event: progress\ndata: {\"imported\":2}\n\n", "without a result", 200, 2},
		{"multiline", ": keepalive\r\nevent:done\r\ndata:{\"imported\":2,\r\ndata:\"updated\":1}\r\n\r\n", "", 200, 2},
		{"EOF", "event: progress\ndata: {}\n\n", "without a result", 200, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/base/api/v1/import/claude-ai/sync", r.URL.Path)
				assert.Equal(t, "chrome", r.URL.Query().Get("browser"))
				assert.Equal(t, "http://"+r.Host, r.Header.Get("Origin"))
				assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
				if tt.status == 200 {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tt.status)
				_, err := io.WriteString(w, tt.body)
				assert.NoError(t, err)
			}))
			defer ts.Close()
			api, err := apiclient.NewHTTPClient(ts.URL+"/base/", "test-token", ts.Client())
			require.NoError(t, err)
			browser := apiclient.Chrome
			response, err := api.PostAPIV1ImportClaudeAiSyncStreamWithResponse(t.Context(), &apiclient.PostAPIV1ImportClaudeAiSyncRequestOptions{Query: &apiclient.PostAPIV1ImportClaudeAiSyncQuery{Browser: &browser}})
			if tt.status == http.StatusOK {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.NotNil(t, response)
			defer response.HTTPResponse.Body.Close()
			stats, err := readChromeSync(response)
			if tt.wantError != "" {
				if tt.name == "absent host" {
					require.EqualError(t, err, tt.wantError)
				} else {
					require.ErrorContains(t, err, tt.wantError)
				}
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.imported, stats.Imported)
		})
	}
}

func TestChromeSyncCancellation(t *testing.T) {
	dataDir := testDataDir(t)
	t.Setenv("AGENTSVIEW_AUTH_TOKEN", "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ts := daemonRouteTestServer(t, map[string]http.HandlerFunc{
		"/api/v1/import/claude-ai/sync": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			cancel()
			<-r.Context().Done()
		},
	})
	registerTestRuntime(t, dataDir, ts.URL, false)
	cmd := newImportCommand()
	cmd.SetArgs([]string{"--type", "claude-ai", "--sync"})
	require.ErrorIs(t, cmd.ExecuteContext(ctx), context.Canceled)
}

func TestChromeImportArguments(t *testing.T) {
	t.Run("sync request", func(t *testing.T) {
		dataDir := t.TempDir()
		t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
		t.Setenv("AGENTSVIEW_AUTH_TOKEN", "")
		ts := daemonRouteTestServer(t, map[string]http.HandlerFunc{
			"/api/v1/import/claude-ai/sync": func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "chrome", r.URL.Query().Get("browser"))
				if !assert.Equal(t, "http://"+r.Host, r.Header.Get("Origin")) {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, err := io.WriteString(w, "event: done\ndata: {\"imported\":2}\n\n")
				assert.NoError(t, err)
			},
		})
		registerTestRuntime(t, dataDir, ts.URL, false)
		cmd := newImportCommand()
		cmd.SetArgs([]string{"--type", "claude-ai", "--sync"})
		require.NoError(t, cmd.ExecuteContext(t.Context()))
	})

	for _, tt := range []struct {
		name        string
		flags, args []string
		valid       bool
	}{
		{"sync", []string{"--type", "claude-ai", "--sync"}, nil, true},
		{"file", []string{"--type", "claude-ai"}, []string{"conversations.json"}, true},
		{"sync path", []string{"--type", "claude-ai", "--sync"}, []string{"conversations.json"}, false},
		{"sync provider", []string{"--type", "chatgpt", "--sync"}, nil, false},
		{"sync replace", []string{"--type", "claude-ai", "--sync", "--replace", "claude-ai:chat"}, nil, false},
		{"missing path", []string{"--type", "claude-ai"}, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newImportCommand()
			require.NoError(t, cmd.ParseFlags(tt.flags))
			err := cmd.ValidateArgs(tt.args)
			if tt.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
