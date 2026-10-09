package server

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/chromehost"
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
	for _, fixture := range []struct{ path, file string }{
		{"/api/organizations", "organizations.json"},
		{"/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations_v2?limit=50&offset=0", "list_all.json"},
		{"/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations/22222222-2222-4222-8222-222222222222?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", "detail.json"},
		{"/api/organizations/11111111-1111-4111-8111-111111111111/chat_conversations/22222222-2222-4222-8222-222222222223?tree=True&rendering_mode=messages&consistency=strong&render_all_tools=true&include_inline_comparison=true", "detail.json"},
	} {
		id, path := readChromeRequest(t, conn)
		require.Equal(t, fixture.path, path)
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
	messages, err := srv.db.GetAllMessages(t.Context(), "claude-ai:22222222-2222-4222-8222-222222222222")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "Chosen reply", messages[1].Content)
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
	for _, reply := range []struct{ name, body, message string }{
		{"stale extension", `{"status":0,"error":"Unsupported Claude fetch path"}`, "run agentsview chrome setup, reload the extension at chrome://extensions, then Sync again"},
		{"newer extension", `{"version":2,"status":600,"body":"private"}`, "upgrade AgentsView, then Sync again"},
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
			if reply.name == "stale extension" {
				assert.Contains(t, response.Body.String(), `"code":"claude_ai_chrome_host_update_required"`)
			} else {
				assert.NotContains(t, response.Body.String(), `"code":"claude_ai_chrome_host_update_required"`)
			}
			assert.NotContains(t, response.Body.String(), "event: done")
			assert.NotContains(t, response.Body.String(), "private")
			stats, err := srv.db.GetStats(t.Context(), false, false)
			require.NoError(t, err)
			assert.Zero(t, stats.SessionCount)
		})
	}
}

func TestChromeHostIncompatiblePendingRequests(t *testing.T) {
	srv := testServer(t, 5*time.Second)
	_, conn := testChromeConnection(t, srv)
	finished := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := srv.chrome.fetch(t.Context(), "/api/organizations")
			finished <- err
		}()
		readChromeRequest(t, conn)
	}
	require.NoError(t, chromehost.WriteFrame(conn, []byte(`{"version":2}`)))
	for range 2 {
		select {
		case err := <-finished:
			require.ErrorIs(t, err, chromehost.ErrCompatibility)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "incompatible host left a pending fetch")
		}
	}
}

func TestChromeHostDisconnect(t *testing.T) {
	for _, mode := range []string{"oversized frame", "negative status", "status above 599", "EOF"} {
		t.Run(mode, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			_, conn := testChromeConnection(t, srv)
			finished := make(chan error, 1)
			go func() { _, err := srv.chrome.fetch(t.Context(), "/api/organizations"); finished <- err }()
			_, path := readChromeRequest(t, conn)
			require.Equal(t, "/api/organizations", path)
			switch mode {
			case "oversized frame":
				var header [4]byte
				binary.NativeEndian.PutUint32(header[:], chromehost.FrameLimit+1)
				_, err := conn.Write(header[:])
				require.NoError(t, err)
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
