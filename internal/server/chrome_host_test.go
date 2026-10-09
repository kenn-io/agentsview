package server

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"io"
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
	"go.kenn.io/agentsview/internal/importer"
)

func chromeHostVersion(t *testing.T, srv *Server) bool {
	t.Helper()
	response := httptest.NewRecorder()
	srv.mux.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/version", nil))
	require.Equal(t, http.StatusOK, response.Code)
	var version VersionInfo
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &version))
	return version.ClaudeAIChromeHost
}

func testChromeConnection(t *testing.T, srv *Server) (string, net.Conn) {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "chrome", "host.sock")
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	require.NoError(t, srv.ServeChromeHost(ctx, socket))
	conn, err := net.Dial("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Eventually(t, func() bool { return chromeHostVersion(t, srv) }, 5*time.Second, 10*time.Millisecond)
	return socket, conn
}

func readChromeRequest(t *testing.T, conn net.Conn) (string, string) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	var header [4]byte
	_, err := io.ReadFull(conn, header[:])
	require.NoError(t, err)
	body := make([]byte, binary.NativeEndian.Uint32(header[:]))
	_, err = io.ReadFull(conn, body)
	require.NoError(t, err)
	var request struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	require.NoError(t, json.Unmarshal(body, &request))
	require.NotEmpty(t, request.ID)
	return request.ID, request.Path
}

func writeChromeReply(t *testing.T, conn net.Conn, id string, status int, body string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"id": id, "status": status, "body": body})
	require.NoError(t, err)
	var header [4]byte
	binary.NativeEndian.PutUint32(header[:], uint32(len(payload)))
	_, err = conn.Write(append(header[:], payload...))
	require.NoError(t, err)
}

func TestChromeHostSyncPrivateReplies(t *testing.T) {
	srv := testServer(t, 5*time.Second)
	_, conn := testChromeConnection(t, srv)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.mux.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/claude-ai/sync?browser=chrome", nil))
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
		t.Fatal("Sync did not finish")
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
	assert.False(t, chromeHostVersion(t, srv))
	response := httptest.NewRecorder()
	srv.mux.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/import/claude-ai/sync?browser=chrome", nil))
	assert.Equal(t, http.StatusConflict, response.Code)
	assert.Contains(t, response.Body.String(), "Chrome host not connected")
}

func TestChromeHostDisconnect(t *testing.T) {
	for _, mode := range []string{"oversized frame", "new connection", "EOF"} {
		t.Run(mode, func(t *testing.T) {
			srv := testServer(t, 5*time.Second)
			socket, conn := testChromeConnection(t, srv)
			finished := make(chan error, 1)
			go func() { _, err := srv.chrome.fetch(t.Context(), "/api/organizations"); finished <- err }()
			_, path := readChromeRequest(t, conn)
			require.Equal(t, "/api/organizations", path)
			switch mode {
			case "oversized frame":
				var header [4]byte
				binary.NativeEndian.PutUint32(header[:], chromeFrameLimit+1)
				_, err := conn.Write(header[:])
				require.NoError(t, err)
			case "new connection":
				newConn, err := net.Dial("unix", socket)
				require.NoError(t, err)
				defer newConn.Close()
				// Wait for the replaced connection to close before issuing a new fetch.
				select {
				case err := <-finished:
					require.ErrorContains(t, err, "disconnected")
				case <-time.After(5 * time.Second):
					t.Fatal("old fetch did not fail")
				}
				go func() { _, err := srv.chrome.fetch(t.Context(), "/api/organizations"); finished <- err }()
				id, _ := readChromeRequest(t, newConn)
				writeChromeReply(t, newConn, id, 200, "[]")
				select {
				case err := <-finished:
					require.NoError(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("new fetch did not finish")
				}
				assert.True(t, chromeHostVersion(t, srv))
				return
			case "EOF":
				require.NoError(t, conn.Close())
			}
			select {
			case err := <-finished:
				require.ErrorContains(t, err, "disconnected")
			case <-time.After(5 * time.Second):
				t.Fatal("pending fetch did not fail")
			}
			require.Eventually(t, func() bool { return !chromeHostVersion(t, srv) }, 5*time.Second, 10*time.Millisecond)
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
		t.Fatal("fetch did not finish")
	}
}
