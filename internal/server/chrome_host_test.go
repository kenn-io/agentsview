package server

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/chromehost"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/importer"
)

func testChromeConnection(t *testing.T, srv *Server) (string, net.Conn) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "chrome", "host.sock")
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	require.NoError(t, srv.ServeChromeHost(ctx, socket))
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Eventually(t, func() bool { return srv.chrome.Connected() }, 5*time.Second, 10*time.Millisecond)
	return socket, conn
}

func readChromeRequest(t *testing.T, conn net.Conn) (string, string) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	body, err := chromehost.ReadFrame(conn)
	require.NoError(t, err)
	var request struct {
		Version int    `json:"version"`
		ID      string `json:"id"`
		Path    string `json:"path"`
	}
	require.NoError(t, json.Unmarshal(body, &request))
	require.NotEmpty(t, request.ID)
	require.Equal(t, 1, request.Version)
	return request.ID, request.Path
}

func writeChromeReply(t *testing.T, conn net.Conn, id string, status int, body string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"version": 1, "id": id, "status": status, "body": body})
	require.NoError(t, err)
	require.NoError(t, chromehost.WriteFrame(conn, payload))
}

func TestChromeHostSyncPrivateReplies(t *testing.T) {
	srv := testServer(t, 5*time.Second)
	_, conn := testChromeConnection(t, srv)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/claude-ai/sync?browser=chrome", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		srv.mux.ServeHTTP(response, req)
	}()
	for i, fixture := range []struct{ path, file string }{
		{"/api/organizations", "organizations.json"},
		{"/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=0", "list_all.json"},
		{"/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations/22222222-2222-4222-8222-222222222222?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", "detail.json"},
		{"/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations/22222222-2222-4222-8222-222222222223?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", "detail.json"},
	} {
		id, path := readChromeRequest(t, conn)
		require.Equal(t, fixture.path, path)
		if i == 0 {
			second := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/claude-ai/sync?browser=chrome", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			srv.mux.ServeHTTP(second, req)
			assert.Equal(t, http.StatusConflict, second.Code)
			assert.JSONEq(t, `{"code":"claude_ai_sync_running","error":"Chrome Sync is already running"}`, second.Body.String())
		}
		body, err := os.ReadFile(filepath.Join("../importer/testdata/claude_ai_sync", fixture.file))
		require.NoError(t, err)
		if strings.Contains(path, "22222222-2222-4222-8222-222222222223?") {
			body = []byte(strings.ReplaceAll(string(body), "22222222-2222-4222-8222-222222222222", "22222222-2222-4222-8222-222222222223"))
		}
		writeChromeReply(t, conn, id, 200, string(body))
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "Sync did not finish")
	}
	require.Equal(t, http.StatusOK, response.Code)
	assert.NotContains(t, response.Body.String(), "event: fetch")
	assert.NotContains(t, response.Body.String(), "Chosen reply")
	assert.NotContains(t, response.Body.String(), "Hello")
	var stats importer.ImportStats
	readImportEvents(t, response.Body, func(event, data string) {
		require.NotEqual(t, "error", event, data)
		if event == "done" {
			require.NoError(t, json.Unmarshal([]byte(data), &stats))
		}
	})
	assert.Equal(t, 2, stats.Imported)
}

func TestChromeHostAbsent(t *testing.T) {
	srv := testServer(t, 5*time.Second)
	response := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/claude-ai/sync?browser=chrome", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	srv.mux.ServeHTTP(response, req)
	assert.Equal(t, http.StatusConflict, response.Code)
	assert.JSONEq(t, `{"code":"claude_ai_chrome_host_required","error":"Run agentsview chrome setup and keep Chrome open, then Sync again"}`, response.Body.String())
}

func TestClaudeAIChromeStatus(t *testing.T) {
	for _, tt := range []struct {
		name                               string
		installed, connected, otherProfile bool
		want                               string
	}{
		{"not set up", false, false, false, `{"installed":false,"connected":false,"other_profile":false}`},
		{"registration replaced", true, false, false, `{"installed":false,"connected":false,"other_profile":false}`},
		{"registration missing", true, false, false, `{"installed":false,"connected":false,"other_profile":false}`},
		{"disconnected", true, false, false, `{"installed":true,"connected":false,"other_profile":false}`},
		{"connected", true, true, false, `{"installed":true,"connected":true,"other_profile":false}`},
		{"other profile", true, true, true, `{"installed":true,"connected":true,"other_profile":true}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			if tt.installed {
				dir := filepath.Join(srv.cfg.DataDir, "chrome", "extension")
				require.NoError(t, os.MkdirAll(dir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{}`), 0o600))
				executable := filepath.Join(t.TempDir(), "bin with spaces' and %", "agentsview")
				require.NoError(t, os.MkdirAll(filepath.Dir(executable), 0o700))
				require.NoError(t, os.WriteFile(executable, []byte("program"), 0o700))
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("USERPROFILE", home)
				t.Setenv("CHROME_CONFIG_HOME", "")
				t.Setenv("XDG_CONFIG_HOME", "")
				launcher := chromehost.LauncherPath(srv.cfg.DataDir)
				socket, err := chromehost.SocketPath(srv.cfg.DataDir)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(launcher, []byte(chromehost.BuildLauncher(executable, socket, runtime.GOOS)), 0o700))
				registered := chromehost.ManifestPath(srv.cfg.DataDir, home)
				body, err := json.Marshal(map[string]string{"path": launcher})
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(filepath.Dir(registered), 0o700))
				require.NoError(t, os.WriteFile(registered, body, 0o600))
				if tt.name == "registration replaced" {
					otherLauncher := chromehost.LauncherPath(t.TempDir())
					body, err = json.Marshal(map[string]string{"path": otherLauncher})
					require.NoError(t, err)
					if runtime.GOOS == "windows" {
						registered = filepath.Join(t.TempDir(), chromehost.NativeHost+".json")
					}
					require.NoError(t, os.WriteFile(registered, body, 0o600))
				}
				if tt.name == "registration missing" {
					registered = ""
				}
				srv.chrome.registeredManifest = func(string) (string, error) { return registered, nil }
			}
			if tt.connected {
				socket, _ := testChromeConnection(t, srv)
				if tt.otherProfile {
					second, err := (&net.Dialer{}).DialContext(t.Context(), "unix", socket)
					require.NoError(t, err)
					defer second.Close()
					require.NoError(t, second.SetReadDeadline(time.Now().Add(5*time.Second)))
					_, err = chromehost.ReadFrame(second)
					require.ErrorIs(t, err, io.EOF)
				}
			}
			response := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/import/claude-ai/chrome", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			srv.mux.ServeHTTP(response, req)
			assert.Equal(t, http.StatusOK, response.Code)
			assert.JSONEq(t, tt.want, response.Body.String())
		})
	}
}

func TestClaudeAIChromeStatusIncompleteInstall(t *testing.T) {
	for _, state := range []string{"missing launcher", "malformed launcher", "missing executable", "executable is directory", "missing extension"} {
		t.Run(state, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			dir := filepath.Join(srv.cfg.DataDir, "chrome", "extension")
			require.NoError(t, os.MkdirAll(dir, 0o700))
			if state != "missing extension" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{}`), 0o600))
			}
			executable := filepath.Join(t.TempDir(), "agentsview")
			switch state {
			case "executable is directory":
				require.NoError(t, os.Mkdir(executable, 0o700))
			case "missing extension":
				require.NoError(t, os.WriteFile(executable, []byte("program"), 0o700))
			}
			launcher := chromehost.LauncherPath(srv.cfg.DataDir)
			socket, err := chromehost.SocketPath(srv.cfg.DataDir)
			require.NoError(t, err)
			command := chromehost.BuildLauncher(executable, socket, runtime.GOOS)
			if state == "malformed launcher" {
				command = "invalid"
			}
			if state != "missing launcher" {
				require.NoError(t, os.WriteFile(launcher, []byte(command), 0o700))
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("CHROME_CONFIG_HOME", "")
			t.Setenv("XDG_CONFIG_HOME", "")
			registered := chromehost.ManifestPath(srv.cfg.DataDir, home)
			body, err := json.Marshal(map[string]string{"path": launcher})
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Dir(registered), 0o700))
			require.NoError(t, os.WriteFile(registered, body, 0o600))
			srv.chrome.registeredManifest = func(string) (string, error) { return registered, nil }
			response := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/import/claude-ai/chrome", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			srv.mux.ServeHTTP(response, req)
			assert.Equal(t, http.StatusOK, response.Code)
			assert.JSONEq(t, `{"installed":false,"connected":false,"other_profile":false}`, response.Body.String())
		})
	}
}

type chromeStatusStore struct {
	db.Store
	readOnly bool
}

func (s chromeStatusStore) ReadOnly() bool { return s.readOnly }

func TestClaudeAIChromeStatusArchiveChecks(t *testing.T) {
	for _, tt := range []struct {
		name     string
		readOnly bool
		want     string
	}{
		{"read only", true, "import not available in read-only mode"},
		{"non local archive", false, "sync requires a local archive"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			srv.db = chromeStatusStore{Store: srv.db, readOnly: tt.readOnly}
			response := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/import/claude-ai/chrome", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			srv.mux.ServeHTTP(response, req)
			assert.Equal(t, http.StatusNotImplemented, response.Code)
			assert.Contains(t, response.Body.String(), tt.want)
		})
	}
}

func TestClaudeAIChromeStatusLocalOnly(t *testing.T) {
	for _, tt := range []struct{ name, remote, forwarded string }{
		{"remote", "192.0.2.10:1234", ""},
		{"forwarded", "127.0.0.1:1234", "for=192.0.2.10"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			response := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/import/claude-ai/chrome", nil)
			req.RemoteAddr = tt.remote
			req.Header.Set("Forwarded", tt.forwarded)
			srv.mux.ServeHTTP(response, req)
			assert.Equal(t, http.StatusForbidden, response.Code)
		})
	}
}

func TestChromeHostLocalOnly(t *testing.T) {
	for _, tt := range []struct {
		name, remote, forwarded string
		requireAuth             bool
	}{
		{"remote auth", "192.0.2.10:1234", "", true},
		{"bind all", "192.0.2.10:1234", "", false},
		{"forwarded", "127.0.0.1:1234", "for=192.0.2.10", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			srv.cfg.Host = "0.0.0.0"
			srv.cfg.RequireAuth = tt.requireAuth
			srv.cfg.AuthToken = "test-token"
			_, conn := testChromeConnection(t, srv)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/claude-ai/sync?browser=chrome", nil)
			req.RemoteAddr = tt.remote
			req.Host = "127.0.0.1:0"
			req.Header.Set("Origin", "http://127.0.0.1:0")
			req.Header.Set("Authorization", "Bearer test-token")
			req.Header.Set("Forwarded", tt.forwarded)
			response := httptest.NewRecorder()
			srv.Handler().ServeHTTP(response, req)
			assert.Equal(t, http.StatusForbidden, response.Code)
			assert.Contains(t, response.Body.String(), "Chrome Sync requires a local connection")
			assert.True(t, srv.chrome.Connected())
			require.NoError(t, conn.SetReadDeadline(time.Now()))
			_, err := chromehost.ReadFrame(conn)
			require.Error(t, err)
			networkErr, ok := errors.AsType[net.Error](err)
			require.True(t, ok)
			assert.True(t, networkErr.Timeout(), "refused requests must not use Chrome")
		})
	}
}

func TestChromeHostIncompatibleReply(t *testing.T) {
	for _, reply := range []struct{ name, body, message, code string }{
		{"stale extension", `{"status":0,"error":"Unsupported Claude fetch path"}`, "run agentsview chrome setup, reload the extension at chrome://extensions, then Sync again", "claude_ai_chrome_host_update_required"},
		{"newer extension", `{"version":2,"status":600,"body":"private"}`, "upgrade AgentsView, then Sync again", "claude_ai_agentsview_update_required"},
	} {
		t.Run(reply.name, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			_, conn := testChromeConnection(t, srv)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/claude-ai/sync?browser=chrome", nil)
				req.RemoteAddr = "127.0.0.1:1234"
				srv.mux.ServeHTTP(response, req)
			}()
			id, _ := readChromeRequest(t, conn)
			payload := `{"id":"` + id + `",` + reply.body[1:]
			require.NoError(t, chromehost.WriteFrame(conn, []byte(payload)))
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "incompatible host did not stop Sync")
			}
			assert.Contains(t, response.Body.String(), reply.message)
			assert.Contains(t, response.Body.String(), `"code":"`+reply.code+`"`)
			assert.NotContains(t, response.Body.String(), "event: done")
			assert.NotContains(t, response.Body.String(), "private")
			stats, err := srv.db.GetStats(t.Context(), false, false)
			require.NoError(t, err)
			assert.Zero(t, stats.SessionCount)
		})
	}
}

func TestChromeHostDisconnect(t *testing.T) {
	for _, mode := range []string{"negative status", "status above 599", "EOF"} {
		t.Run(mode, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			_, conn := testChromeConnection(t, srv)
			finished := make(chan error, 1)
			go func() { _, err := srv.chrome.fetch(t.Context(), "/api/organizations"); finished <- err }()
			_, path := readChromeRequest(t, conn)
			require.Equal(t, "/api/organizations", path)
			switch mode {
			case "negative status":
				writeChromeReply(t, conn, "ignored", -1, "[]")
			case "status above 599":
				writeChromeReply(t, conn, "ignored", 600, "[]")
			case "EOF":
				require.NoError(t, conn.Close())
			}
			select {
			case err := <-finished:
				if mode == "negative status" || mode == "status above 599" {
					require.EqualError(t, err, "invalid Chrome reply status")
				} else {
					require.ErrorContains(t, err, "disconnected")
				}
			case <-time.After(5 * time.Second):
				require.FailNow(t, "pending fetch did not fail")
			}
			require.Eventually(t, func() bool { return !srv.chrome.Connected() }, 5*time.Second, 10*time.Millisecond)
		})
	}
}

func TestChromeHostFrameReply(t *testing.T) {
	srv := testServer(t, 5*time.Second)
	_, conn := testChromeConnection(t, srv)
	finished := make(chan importer.ClaudeAIResponse, 1)
	go func() { response, _ := srv.chrome.fetch(t.Context(), "/api/organizations"); finished <- response }()
	id, _ := readChromeRequest(t, conn)
	writeChromeReply(t, conn, "foreign-id", 200, "private")
	writeChromeReply(t, conn, id, 413, "")
	select {
	case reply := <-finished:
		assert.Equal(t, 413, reply.Status)
		assert.Empty(t, strings.TrimSpace(string(reply.Body)))
	case <-time.After(5 * time.Second):
		require.FailNow(t, "fetch did not finish")
	}
}

func TestChromeHostKeepsLiveConnection(t *testing.T) {
	srv := testServer(t, 5*time.Second)
	socket, conn := testChromeConnection(t, srv)
	finished := make(chan error, 1)
	go func() {
		response, err := srv.chrome.fetch(t.Context(), "/api/organizations")
		if err == nil {
			assert.Equal(t, "[]", string(response.Body))
		}
		finished <- err
	}()
	id, _ := readChromeRequest(t, conn)
	second, err := (&net.Dialer{}).DialContext(t.Context(), "unix", socket)
	require.NoError(t, err)
	defer second.Close()
	require.NoError(t, second.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = chromehost.ReadFrame(second)
	require.Error(t, err)
	if networkErr, ok := errors.AsType[net.Error](err); ok {
		assert.False(t, networkErr.Timeout(), "the extra connection must close")
	}
	writeChromeReply(t, conn, id, 200, "[]")
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "first fetch did not finish")
	}
	assert.True(t, srv.chrome.Connected())
}

func TestChromeHostDisconnectClearsOtherProfile(t *testing.T) {
	srv := testServer(t, 5*time.Second)
	socket, conn := testChromeConnection(t, srv)
	second, err := (&net.Dialer{}).DialContext(t.Context(), "unix", socket)
	require.NoError(t, err)
	defer second.Close()
	require.NoError(t, second.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = chromehost.ReadFrame(second)
	require.ErrorIs(t, err, io.EOF)
	status := func() string {
		response := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/import/claude-ai/chrome", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		srv.mux.ServeHTTP(response, req)
		require.Equal(t, http.StatusOK, response.Code)
		return response.Body.String()
	}
	assert.JSONEq(t, `{"installed":false,"connected":true,"other_profile":true}`, status())
	require.NoError(t, conn.Close())
	require.Eventually(t, func() bool { return !srv.chrome.Connected() }, 5*time.Second, 10*time.Millisecond)
	assert.JSONEq(t, `{"installed":false,"connected":false,"other_profile":false}`, status())
	assert.Equal(t, "Sign in to Claude.ai, then Sync again", srv.chrome.signInError())
}

func TestChromeHostSignInProfiles(t *testing.T) {
	for _, tt := range []struct {
		name               string
		refused, reconnect bool
		status             int
		want               string
	}{
		{"single profile", false, false, 401, "Sign in to Claude.ai, then Sync again"},
		{"other profile", true, false, 401, "Another Chrome profile also has the extension. Only the first connected profile is used. Sign in to Claude.ai there or disable the extension in the other profile, then Sync again"},
		{"new connection", true, true, 401, "Sign in to Claude.ai, then Sync again"},
		{"other error", true, false, 403, "403"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			socket, conn := testChromeConnection(t, srv)
			if tt.refused {
				second, err := (&net.Dialer{}).DialContext(t.Context(), "unix", socket)
				require.NoError(t, err)
				defer second.Close()
				require.NoError(t, second.SetReadDeadline(time.Now().Add(5*time.Second)))
				_, err = chromehost.ReadFrame(second)
				require.Error(t, err)
				if networkErr, ok := errors.AsType[net.Error](err); ok {
					require.False(t, networkErr.Timeout(), "the extra connection must close")
				}
			}
			if tt.reconnect {
				require.NoError(t, conn.Close())
				require.Eventually(t, func() bool { return !srv.chrome.Connected() }, 5*time.Second, 10*time.Millisecond)
				var err error
				conn, err = (&net.Dialer{}).DialContext(t.Context(), "unix", socket)
				require.NoError(t, err)
				defer conn.Close()
				require.Eventually(t, func() bool { return srv.chrome.Connected() }, 5*time.Second, 10*time.Millisecond)
			}
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/claude-ai/sync?browser=chrome", nil)
				req.RemoteAddr = "127.0.0.1:1234"
				srv.mux.ServeHTTP(response, req)
			}()
			id, path := readChromeRequest(t, conn)
			require.Equal(t, "/api/organizations", path)
			writeChromeReply(t, conn, id, tt.status, "{}")
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "Sync did not finish")
			}
			assert.Contains(t, response.Body.String(), tt.want)
			if tt.status == 401 {
				assert.Contains(t, response.Body.String(), `"code":"claude_ai_auth_required"`)
			} else {
				assert.NotContains(t, response.Body.String(), "Another Chrome profile")
			}
		})
	}
}

type chromeAcceptListener struct {
	net.Listener
	conn  net.Conn
	calls int
}

func (l *chromeAcceptListener) Accept() (net.Conn, error) {
	l.calls++
	switch l.calls {
	case 1:
		return nil, &net.OpError{Op: "accept", Err: &temporaryChromeAcceptError{}}
	case 2:
		return l.conn, nil
	default:
		return nil, net.ErrClosed
	}
}

type temporaryChromeAcceptError struct{}

func (*temporaryChromeAcceptError) Error() string   { return "temporary accept failure" }
func (*temporaryChromeAcceptError) Timeout() bool   { return false }
func (*temporaryChromeAcceptError) Temporary() bool { return true }

func TestChromeHostAcceptRetries(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	synctest.Test(t, func(t *testing.T) {
		conn, peer := net.Pipe()
		defer peer.Close()
		srv := &Server{}
		listener := &chromeAcceptListener{conn: conn}
		srv.acceptChromeHost(t.Context(), listener)
		assert.True(t, srv.chrome.Connected())
		assert.Equal(t, 3, listener.calls)
		assert.Contains(t, output.String(), "temporary accept failure")
		assert.NotContains(t, output.String(), net.ErrClosed.Error())
	})
}
