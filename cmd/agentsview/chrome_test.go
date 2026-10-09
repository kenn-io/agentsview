package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
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
	require.NoError(t, os.MkdirAll(filepath.Dir(executable), 0o700))
	require.NoError(t, os.WriteFile(executable, []byte(program), 0o700))
	return executable
}

func TestChromeSetup(t *testing.T) {
	t.Setenv("CHROME_CONFIG_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
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

func TestChromeConfigRoot(t *testing.T) {
	for _, tt := range []struct{ name, chrome, xdg, want string }{
		{"Chrome overrides XDG", "chrome-config", "xdg-config", "chrome-config/google-chrome"},
		{"XDG overrides home", "", "xdg-config", "xdg-config/google-chrome"},
		{"home fallback", "", "", "home/.config/google-chrome"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CHROME_CONFIG_HOME", tt.chrome)
			t.Setenv("XDG_CONFIG_HOME", tt.xdg)
			assert.Equal(t, filepath.FromSlash(tt.want), chromeConfigRoot("home"))
		})
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

func TestChromeSetupWriteFailurePreservesInstall(t *testing.T) {
	for failAt := range 4 {
		t.Run(fmt.Sprintf("write %d", failAt), func(t *testing.T) {
			t.Setenv("CHROME_CONFIG_HOME", "")
			t.Setenv("XDG_CONFIG_HOME", "")
			dir := t.TempDir()
			assets := fstest.MapFS{
				"chrome-extension/manifest.json": {Data: []byte(`{"key":"dGVzdA=="}`)},
				"chrome-extension/worker.js":     {Data: []byte("old worker")},
			}
			var manifestPath string
			folder, err := setupChrome(dir, dir, chromeTestExecutable(t), assets, func(path string) error { manifestPath = path; return nil })
			require.NoError(t, err)
			manifest, err := os.ReadFile(manifestPath)
			require.NoError(t, err)
			var host struct {
				Path string `json:"path"`
			}
			require.NoError(t, json.Unmarshal(manifest, &host))
			paths := []string{filepath.Join(folder, "manifest.json"), filepath.Join(folder, "worker.js"), host.Path, manifestPath}
			previous := make(map[string][]byte)
			for _, path := range paths {
				previous[path], err = os.ReadFile(path)
				require.NoError(t, err)
			}
			assets["chrome-extension/manifest.json"].Data = []byte(`{"key":"bmV3"}`)
			assets["chrome-extension/worker.js"].Data = []byte("new worker")
			writeErr := errors.New("disk full")
			writes := 0
			registered := false
			_, err = setupChromeWithWriter(dir, dir, "new-executable", assets, func(string) error { registered = true; return nil }, func(file *os.File, data []byte) (int, error) {
				defer func() { writes++ }()
				if writes == failAt {
					n, err := file.Write(data[:1])
					require.NoError(t, err)
					return n, writeErr
				}
				return file.Write(data)
			})
			require.ErrorIs(t, err, writeErr)
			assert.False(t, registered)
			for _, path := range paths {
				body, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, previous[path], body, "%s", path)
			}
			for _, path := range []string{folder, filepath.Dir(host.Path), filepath.Dir(manifestPath)} {
				staged, err := filepath.Glob(filepath.Join(path, ".chrome-*"))
				require.NoError(t, err)
				assert.Empty(t, staged)
			}
		})
	}
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
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
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
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-ctx.Done():
		require.FailNow(t, "refused relay did not reconnect")
	}
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
		require.FailNow(t, "relay did not exit on stdin EOF")
	}
}

func TestChromeHostRelayEOFWhileServerDown(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	require.NoError(t, relayChromeHost(t.Context(), filepath.Join(dir, "host.sock"), strings.NewReader(""), io.Discard))
}

func TestChromeHostRelayRefusesLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory permissions")
	}
	for _, mode := range []os.FileMode{0o750, 0o707} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.Chmod(dir, mode))
			socket := filepath.Join(dir, "host.sock")
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
			require.NoError(t, err)
			defer listener.Close()
			input, writeInput := io.Pipe()
			defer input.Close()
			defer writeInput.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			stderr := captureStderr(t, func() {
				err := relayChromeHost(ctx, socket, input, io.Discard)
				require.ErrorContains(t, err, "not mode 0700")
			})
			assert.Contains(t, stderr, "refusing chrome host socket")
			assert.Contains(t, stderr, "not mode 0700")
			require.NoError(t, listener.SetDeadline(time.Now()))
			conn, err := listener.AcceptUnix()
			if conn != nil {
				defer conn.Close()
			}
			require.Error(t, err)
			var netErr net.Error
			require.ErrorAs(t, err, &netErr)
			assert.True(t, netErr.Timeout())
		})
	}
}

func TestChromeLongDataDirSetupAndServe(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("data", 25))
	socket, err := chromeSocketPath(dir)
	require.NoError(t, err)
	defer os.RemoveAll(filepath.Dir(socket))
	assert.Less(t, len(socket), 104)
	assert.NotEqual(t, filepath.Join(dir, "chrome", "host.sock"), socket)
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, home, filepath.Dir(filepath.Dir(socket)))
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
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", socket)
	require.NoError(t, err)
	defer conn.Close()
	again, err := chromeSocketPath(dir)
	require.NoError(t, err)
	assert.Equal(t, socket, again)
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
		{"partial failure", "event:progress\ndata:{\"imported\":2,\"updated\":1,\"skipped\":3,\"errors\":1}\n\nevent:error\ndata:{\"error\":\"Sign in required\"}\n\n", "Sign in required", 200, 2},
		{"partial EOF", "event: progress\ndata: {\"imported\":2}\n\n", "missing done event", 200, 2},
		{"multiline", ": keepalive\r\nevent:done\r\ndata:{\"imported\":2,\r\ndata:\"updated\":1}\r\n\r\n", "", 200, 2},
		{"EOF", "event: progress\ndata: {}\n\n", "missing done event", 200, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

func TestChromeSyncCommand(t *testing.T) {
	for _, tt := range []struct {
		name, body, wantError string
		cancel                bool
	}{
		{"done", "event: done\ndata: {\"imported\":2}\n\n", "", false},
		{"partial failure", "event: progress\ndata: {\"imported\":2,\"updated\":1,\"skipped\":3,\"errors\":1}\n\nevent: error\ndata: {\"error\":\"Sign in required\"}\n\n", "Sign in required", false},
		{"cancel", "", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := testDataDir(t)
			t.Setenv("AGENTSVIEW_AUTH_TOKEN", "")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ts := daemonRouteTestServer(t, map[string]http.HandlerFunc{
				"/api/v1/import/claude-ai/sync": func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, http.MethodPost, r.Method)
					assert.Equal(t, "chrome", r.URL.Query().Get("browser"))
					if !assert.Equal(t, "http://"+r.Host, r.Header.Get("Origin")) {
						http.Error(w, "Forbidden", http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					if tt.cancel {
						w.WriteHeader(http.StatusOK)
						w.(http.Flusher).Flush()
						cancel()
						<-r.Context().Done()
						return
					}
					_, err := io.WriteString(w, tt.body)
					assert.NoError(t, err)
				},
			})
			registerTestRuntime(t, dataDir, ts.URL, false)
			output := captureStderr(t, func() {
				cmd := newImportCommand()
				cmd.SetArgs([]string{"--type", "claude-ai", "--sync"})
				err := cmd.ExecuteContext(ctx)
				if tt.cancel {
					require.ErrorIs(t, err, context.Canceled)
				} else if tt.wantError != "" {
					require.ErrorContains(t, err, tt.wantError)
				} else {
					require.NoError(t, err)
				}
			})
			if tt.wantError != "" {
				assert.Contains(t, output, "Done: 6 processed (2 new, 1 updated, 3 skipped)")
				assert.Contains(t, output, "1 errors")
			}
		})
	}
}

func TestChromeImportArguments(t *testing.T) {
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
