package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/sync"
)

// testServer creates a Server for internal tests with the given
// write timeout. It registers cleanup of the database via
// t.Cleanup.
func testServer(
	t *testing.T, writeTimeout time.Duration,
	opts ...Option,
) *Server {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database, err := db.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	cfg := config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		DBPath:       dbPath,
		WriteTimeout: writeTimeout,
	}
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {dir},
		},
		Machine: "test",
	})
	return New(cfg, database, engine, opts...)
}

// withHandlerDelay injects a sleep before each timeout-wrapped
// handler, guaranteeing the handler exceeds short timeouts.
// Used only in tests.
func withHandlerDelay(d time.Duration) Option {
	return func(s *Server) { s.handlerDelay = d }
}

// assertTimeoutResponse checks that the response is a 503 with
// a JSON body containing "request timed out" and the correct
// Content-Type header.
func assertTimeoutResponse(
	t *testing.T, resp *http.Response,
) {
	t.Helper()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body = struct {
		io.Reader
		io.Closer
	}{bytes.NewReader(body), resp.Body}
	var je jsonError
	require.NoError(t, json.Unmarshal(body, &je),
		"body is not valid JSON; body=%q", string(body))
	require.Equal(t, "request timed out", je.Error)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))
}

// isTimeoutResponse returns true when the response is a 503
// JSON timeout. Use this for negative assertions where a route
// should NOT produce a timeout.
func isTimeoutResponse(
	t *testing.T, resp *http.Response,
) bool {
	t.Helper()
	if resp.StatusCode != http.StatusServiceUnavailable {
		return false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{bytes.NewReader(body), resp.Body}
	var je jsonError
	if json.Unmarshal(body, &je) != nil {
		return false
	}
	return je.Error == "request timed out"
}

// newTestRequest returns a recorder and request for lightweight
// handler tests. Pass an empty query for no query string.
func newTestRequest(
	t *testing.T, query string,
) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	target := "/test"
	if query != "" {
		target += "?" + query
	}
	return httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, target, nil)
}

// newRoutedTestServerWithStore creates a lightweight Server
// backed by the given Store with routes registered. Use this for
// internal handler tests that drive requests through s.mux
// without a real database or sync engine.
func newRoutedTestServerWithStore(
	t *testing.T, store db.Store,
) *Server {
	t.Helper()
	s := &Server{
		cfg:      config.Config{Host: "127.0.0.1"},
		db:       store,
		sessions: service.NewReadOnlyBackend(store),
		mux:      http.NewServeMux(),
	}
	s.routes()
	return s
}

// serveGet issues a GET request for path through the server's
// mux and returns the recorder.
func serveGet(
	t *testing.T, s *Server, path string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	return w
}

// assertRecorderStatus checks that the recorder has the
// expected HTTP status code.
func assertRecorderStatus(
	t *testing.T, w *httptest.ResponseRecorder, code int,
) {
	t.Helper()
	require.Equal(t, code, w.Code, "body: %s", w.Body.String())
}

// assertContentType checks that the recorder has the expected
// Content-Type header.
func assertContentType(
	t *testing.T, w *httptest.ResponseRecorder, expected string,
) {
	t.Helper()
	assert.Equal(t, expected, w.Header().Get("Content-Type"))
}

// newTestServerMinimal creates a lightweight Server with only the
// config set (no database, engine, or temp dirs). Use this for
// handler-level tests that only need withTimeout or similar
// config-driven wrappers.
func newTestServerMinimal(
	t *testing.T, timeout time.Duration,
) *Server {
	t.Helper()
	return &Server{
		cfg: config.Config{WriteTimeout: timeout},
	}
}

// expiredCtx returns a context with a deadline in the past.
func expiredCtx(
	t *testing.T,
) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithDeadline(
		context.Background(), time.Now().Add(-1*time.Hour),
	)
}

// assertContainsAll checks that got contains every string
// in wants.
func assertContainsAll(
	t *testing.T, got string, wants []string,
) {
	t.Helper()
	for _, want := range wants {
		assert.Contains(t, got, want)
	}
}

// assertContainsNone checks that got does not contain any
// string in bads.
func assertContainsNone(
	t *testing.T, got string, bads []string,
) {
	t.Helper()
	for _, bad := range bads {
		assert.NotContains(t, got, bad)
	}
}
