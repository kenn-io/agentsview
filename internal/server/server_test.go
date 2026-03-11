package server_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	stdlibsync "sync"
	"testing"
	"time"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/dbtest"
	"github.com/wesm/agentsview/internal/parser"
	"github.com/wesm/agentsview/internal/server"
	"github.com/wesm/agentsview/internal/sync"
	"github.com/wesm/agentsview/internal/testjsonl"
)

// Timestamp constants for test data.
const (
	tsZero    = "2024-01-01T00:00:00Z"
	tsZeroS5  = "2024-01-01T00:00:05Z"
	tsEarly   = "2024-01-01T10:00:00Z"
	tsEarlyS5 = "2024-01-01T10:00:05Z"
	tsSeed    = "2025-01-15T10:00:00Z"
	tsSeedEnd = "2025-01-15T11:00:00Z"
)

// --- Test helpers ---

// testEnv sets up a server with a temporary database.
type testEnv struct {
	srv       *server.Server
	handler   http.Handler
	db        *db.DB
	claudeDir string
	dataDir   string
}

// setupOption customizes the config used by setup.
type setupOption func(*config.Config)

func withWriteTimeout(d time.Duration) setupOption {
	return func(c *config.Config) { c.WriteTimeout = d }
}

func withPublicOrigins(origins ...string) setupOption {
	return func(c *config.Config) {
		c.PublicOrigins = append([]string(nil), origins...)
	}
}

func withPublicURL(url string) setupOption {
	return func(c *config.Config) { c.PublicURL = url }
}

func setup(
	t *testing.T,
	opts ...setupOption,
) *testEnv {
	return setupWithServerOpts(t, nil, opts...)
}

func setupWithServerOpts(
	t *testing.T,
	srvOpts []server.Option,
	opts ...setupOption,
) *testEnv {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("opening db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	claudeDir := filepath.Join(dir, "claude")
	codexDir := filepath.Join(dir, "codex")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("creating claude dir: %v", err)
	}
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatalf("creating codex dir: %v", err)
	}

	cfg := config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		DBPath:       dbPath,
		WriteTimeout: 30 * time.Second,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
			parser.AgentCodex:  {codexDir},
		},
		Machine: "test",
	})
	srv := server.New(cfg, database, engine, srvOpts...)

	// Wrap handler to set default Host header for all test
	// requests, matching the test config (127.0.0.1:0).
	// Individual tests can override by setting req.Host
	// before calling ServeHTTP directly.
	defaultHost := net.JoinHostPort(
		cfg.Host, fmt.Sprintf("%d", cfg.Port),
	)
	defaultOrigin := fmt.Sprintf("http://%s", defaultHost)
	baseHandler := srv.Handler()
	wrappedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "example.com" || r.Host == "" {
			r.Host = defaultHost
		}
		// Auto-set Origin for mutating requests so tests
		// don't need to set it manually on every inline
		// httptest.NewRequest.
		if r.Header.Get("Origin") == "" {
			switch r.Method {
			case http.MethodPost, http.MethodPut,
				http.MethodPatch, http.MethodDelete:
				r.Header.Set("Origin", defaultOrigin)
			}
		}
		baseHandler.ServeHTTP(w, r)
	})

	return &testEnv{
		srv:       srv,
		handler:   wrappedHandler,
		db:        database,
		claudeDir: claudeDir,
		dataDir:   dir,
	}
}

func (te *testEnv) writeProjectFile(
	t *testing.T, project, filename, content string,
) string {
	t.Helper()
	path := filepath.Join(te.claudeDir, project, filename)
	dbtest.WriteTestFile(t, path, []byte(content))
	return path
}

// writeSessionFile builds JSONL from a SessionBuilder and writes it
// as a project file, returning the file path.
func (te *testEnv) writeSessionFile(
	t *testing.T,
	project, filename string,
	b *testjsonl.SessionBuilder,
) string {
	t.Helper()
	return te.writeProjectFile(t, project, filename, b.String())
}

func waitForPort(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var lastDialErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout(
			"tcp", addr, 50*time.Millisecond,
		)
		if err == nil {
			conn.Close()
			return nil
		}
		lastDialErr = err
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("server not ready: last dial error: %v", lastDialErr)
}

// firstNonLoopbackIP returns a host IP assigned to a non-loopback
// interface. The test is skipped when none is available.
func firstNonLoopbackIP(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("listing interfaces: %v", err)
	}
	var firstV6 string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 ||
			iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				return ip4.String()
			}
			if firstV6 == "" {
				firstV6 = ip.String()
			}
		}
	}
	if firstV6 != "" {
		return firstV6
	}
	t.Skip("no non-loopback interface IP available")
	return ""
}

func hostLiteral(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// listenAndServe starts the server on a real port and returns the
// base URL. The server is shut down when the test finishes.
func (te *testEnv) listenAndServe(t *testing.T) string {
	t.Helper()
	port := server.FindAvailablePort("127.0.0.1", 40000)
	te.srv.SetPort(port)

	var serveErr error
	done := make(chan struct{})
	go func() {
		serveErr = te.srv.ListenAndServe()
		close(done)
	}()

	// Wait for the port to accept connections.
	if err := waitForPort(port, 2*time.Second); err != nil {
		select {
		case <-done:
			t.Fatalf("server failed to start: %v", serveErr)
		default:
		}
		t.Fatalf("server not ready after 2s: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()
		if err := te.srv.Shutdown(ctx); err != nil &&
			err != http.ErrServerClosed {
			t.Errorf("server shutdown error: %v", err)
		}
		select {
		case <-done:
			if serveErr != nil &&
				serveErr != http.ErrServerClosed {
				t.Errorf(
					"server exited with error: %v",
					serveErr,
				)
			}
		case <-time.After(5 * time.Second):
			t.Error("timed out waiting for server goroutine")
		}
	})

	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func (te *testEnv) seedSession(
	t *testing.T, id, project string, msgCount int,
	opts ...func(*db.Session),
) {
	t.Helper()
	dbtest.SeedSession(t, te.db, id, project, func(s *db.Session) {
		s.Machine = "test"
		s.MessageCount = msgCount
		s.UserMessageCount = max(msgCount, 2)
		s.StartedAt = dbtest.Ptr(tsSeed)
		s.EndedAt = dbtest.Ptr(tsSeedEnd)
		s.FirstMessage = dbtest.Ptr("Hello world")
		for _, opt := range opts {
			opt(s)
		}
	})
}

func (te *testEnv) seedMessages(
	t *testing.T, sessionID string, count int, mods ...func(i int, m *db.Message),
) {
	t.Helper()
	msgs := make([]db.Message, count)
	for i := range count {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = db.Message{
			SessionID:     sessionID,
			Ordinal:       i,
			Role:          role,
			Content:       "Message " + string(rune('A'+i%26)),
			Timestamp:     tsSeed,
			ContentLength: 10,
		}
		for _, mod := range mods {
			mod(i, &msgs[i])
		}
	}
	if err := te.db.ReplaceSessionMessages(
		sessionID, msgs,
	); err != nil {
		t.Fatalf("seeding messages: %v", err)
	}
}

func (te *testEnv) getWithContext(
	t *testing.T, ctx context.Context, path string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	return w
}

func (te *testEnv) get(
	t *testing.T, path string,
) *httptest.ResponseRecorder {
	t.Helper()
	return te.getWithContext(t, context.Background(), path)
}

func (te *testEnv) post(
	t *testing.T, path string, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path,
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	return w
}

func (te *testEnv) del(
	t *testing.T, path string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	return w
}

// uploadFile creates a multipart upload request.
func (te *testEnv) upload(
	t *testing.T, filename, content, query string,
) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("creating form file: %v", err)
	}
	if _, err := fw.Write([]byte(content)); err != nil {
		t.Fatalf("writing form file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("closing multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/sessions/upload?"+query, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	return w
}

// decode unmarshals the response body into a typed struct.
func decode[T any](
	t *testing.T, w *httptest.ResponseRecorder,
) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(
		w.Body.Bytes(), &result,
	); err != nil {
		t.Fatalf("decoding JSON: %v\nbody: %s",
			err, w.Body.String())
	}
	return result
}

func assertStatus(
	t *testing.T, w *httptest.ResponseRecorder, code int,
) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("expected status %d, got %d: %s",
			code, w.Code, w.Body.String())
	}
}

func assertBodyContains(
	t *testing.T, w *httptest.ResponseRecorder, substr string,
) {
	t.Helper()
	if !strings.Contains(w.Body.String(), substr) {
		t.Errorf("body %q does not contain %q",
			w.Body.String(), substr)
	}
}

// assertErrorResponse checks that the response body is a JSON
// object with an "error" field matching wantMsg.
func assertErrorResponse(
	t *testing.T, w *httptest.ResponseRecorder,
	wantMsg string,
) {
	t.Helper()
	resp := decode[map[string]string](t, w)
	if got := resp["error"]; got != wantMsg {
		t.Errorf("error = %q, want %q", got, wantMsg)
	}
}

// assertTimeoutRace validates a timeout response where either
// the middleware (503 "request timed out") or the handler
// (504 "gateway timeout") may win the race. Checks status,
// Content-Type, and error body.
func assertTimeoutRace(
	t *testing.T, w *httptest.ResponseRecorder,
) {
	t.Helper()
	code := w.Code
	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf(
			"Content-Type = %q, want application/json", ct,
		)
	}
	switch code {
	case http.StatusServiceUnavailable:
		assertBodyContains(t, w, "request timed out")
	case http.StatusGatewayTimeout:
		assertBodyContains(t, w, "gateway timeout")
	default:
		t.Fatalf(
			"expected 503 or 504, got %d: %s",
			code, w.Body.String(),
		)
	}
}

// expiredContext returns a context with a deadline in the past.
func expiredContext(
	t *testing.T,
) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithDeadline(
		context.Background(), time.Now().Add(-1*time.Hour),
	)
}

type SSEEvent struct {
	Event string
	Data  string
}

func parseSSE(body string) []SSEEvent {
	var events []SSEEvent
	scanner := bufio.NewScanner(strings.NewReader(body))
	var currentEvent SSEEvent
	hasData := false
	for scanner.Scan() {
		line := scanner.Text()
		if ev, ok := strings.CutPrefix(line, "event: "); ok {
			currentEvent.Event = ev
		} else if data, ok := strings.CutPrefix(line, "data: "); ok {
			if hasData {
				currentEvent.Data += "\n" + data
			} else {
				currentEvent.Data = data
				hasData = true
			}
		} else if line == "" {
			if currentEvent.Event != "" || hasData {
				events = append(events, currentEvent)
				currentEvent = SSEEvent{}
				hasData = false
			}
		} else if hasData {
			currentEvent.Data += "\n" + line
		}
	}
	if currentEvent.Event != "" || hasData {
		events = append(events, currentEvent)
	}
	return events
}

func (te *testEnv) waitForSSEEvent(t *testing.T, w *flushRecorder, expectedEvent string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		<-ticker.C
		events := parseSSE(w.BodyString())
		for _, e := range events {
			if e.Event == expectedEvent {
				return
			}
		}
	}
	t.Fatalf("timed out waiting for event: %s, got: %s", expectedEvent, w.BodyString())
}

// --- Typed response structs for JSON decoding ---

type sessionListResponse struct {
	Sessions []db.Session `json:"sessions"`
	Total    int          `json:"total"`
}

type messageListResponse struct {
	Messages []db.Message `json:"messages"`
	Count    int          `json:"count"`
}

type minimapResponse struct {
	Entries []db.MinimapEntry `json:"entries"`
	Count   int               `json:"count"`
}

type searchResponse struct {
	Query   string            `json:"query"`
	Results []db.SearchResult `json:"results"`
	Count   int               `json:"count"`
}

type projectListResponse struct {
	Projects []db.ProjectInfo `json:"projects"`
}

type syncStatusResponse struct {
	LastSync string `json:"last_sync"`
}

type githubConfigResponse struct {
	Configured bool `json:"configured"`
}

type uploadResponse struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project"`
	Machine   string `json:"machine"`
	Messages  int    `json:"messages"`
}

type machineListResponse struct {
	Machines []string `json:"machines"`
}

type syncResultResponse struct {
	TotalSessions int `json:"total_sessions"`
	Synced        int `json:"synced"`
	Skipped       int `json:"skipped"`
}

// --- Tests ---

func TestListSessions_Empty(t *testing.T) {
	te := setup(t)
	w := te.get(t, "/api/v1/sessions")
	assertStatus(t, w, http.StatusOK)

	// Verify raw JSON has "sessions":[] not "sessions":null.
	var raw struct {
		Sessions json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(
		w.Body.Bytes(), &raw,
	); err != nil {
		t.Fatalf("unmarshaling raw response: %v", err)
	}
	if got := strings.TrimSpace(string(raw.Sessions)); got != "[]" {
		t.Fatalf(
			"expected sessions to be [], got: %s", got,
		)
	}

	resp := decode[sessionListResponse](t, w)
	if len(resp.Sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d",
			len(resp.Sessions))
	}
}

func TestListSessions_WithData(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 5)
	te.seedSession(t, "s2", "my-app", 3)
	te.seedSession(t, "s3", "other-app", 1)

	w := te.get(t, "/api/v1/sessions")
	assertStatus(t, w, http.StatusOK)

	resp := decode[sessionListResponse](t, w)
	if len(resp.Sessions) != 3 {
		t.Fatalf("expected 3 sessions, got %d",
			len(resp.Sessions))
	}
}

func TestListSessions_ProjectFilter(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 5)
	te.seedSession(t, "s2", "other-app", 3)

	w := te.get(t, "/api/v1/sessions?project=my-app")
	assertStatus(t, w, http.StatusOK)

	resp := decode[sessionListResponse](t, w)
	if len(resp.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d",
			len(resp.Sessions))
	}
}

func TestListSessions_ExcludeProjectFilter(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 5)
	te.seedSession(t, "s2", "unknown", 3)
	te.seedSession(t, "s3", "unknown", 7)

	w := te.get(t,
		"/api/v1/sessions?exclude_project=unknown",
	)
	assertStatus(t, w, http.StatusOK)

	resp := decode[sessionListResponse](t, w)
	if len(resp.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d",
			len(resp.Sessions))
	}
	if resp.Sessions[0].ID != "s1" {
		t.Errorf("expected session s1, got %s",
			resp.Sessions[0].ID)
	}
}

func TestListSessions_ExcludeOneShotDefault(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 5, func(s *db.Session) {
		s.UserMessageCount = 1
	})
	te.seedSession(t, "s2", "my-app", 10, func(s *db.Session) {
		s.UserMessageCount = 5
	})
	te.seedSession(t, "s3", "my-app", 3, func(s *db.Session) {
		s.UserMessageCount = 0
	})

	// Default: exclude one-shot sessions.
	w := te.get(t, "/api/v1/sessions")
	assertStatus(t, w, http.StatusOK)
	resp := decode[sessionListResponse](t, w)
	if len(resp.Sessions) != 1 {
		t.Fatalf("default: expected 1 session, got %d",
			len(resp.Sessions))
	}
	if resp.Sessions[0].ID != "s2" {
		t.Errorf("default: expected s2, got %s",
			resp.Sessions[0].ID)
	}

	// Explicit include_one_shot=true: include all.
	w = te.get(t,
		"/api/v1/sessions?include_one_shot=true",
	)
	assertStatus(t, w, http.StatusOK)
	resp = decode[sessionListResponse](t, w)
	if len(resp.Sessions) != 3 {
		t.Fatalf("include: expected 3 sessions, got %d",
			len(resp.Sessions))
	}
}

func TestGetSession_Found(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 5)

	w := te.get(t, "/api/v1/sessions/s1")
	assertStatus(t, w, http.StatusOK)

	resp := decode[db.Session](t, w)
	if resp.ID != "s1" {
		t.Fatalf("expected id=s1, got %v", resp.ID)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/sessions/nonexistent")
	assertStatus(t, w, http.StatusNotFound)
}

func TestGetChildSessions_Found(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "parent-1", "my-app", 10)
	te.seedSession(t, "child-a", "my-app", 3, func(s *db.Session) {
		s.ParentSessionID = dbtest.Ptr("parent-1")
		s.RelationshipType = "subagent"
		s.StartedAt = dbtest.Ptr("2025-01-15T10:05:00Z")
		s.EndedAt = dbtest.Ptr("2025-01-15T10:10:00Z")
	})
	te.seedSession(t, "child-b", "my-app", 2, func(s *db.Session) {
		s.ParentSessionID = dbtest.Ptr("parent-1")
		s.RelationshipType = "fork"
		s.StartedAt = dbtest.Ptr("2025-01-15T10:15:00Z")
		s.EndedAt = dbtest.Ptr("2025-01-15T10:20:00Z")
	})

	w := te.get(t, "/api/v1/sessions/parent-1/children")
	assertStatus(t, w, http.StatusOK)

	var children []db.Session
	if err := json.Unmarshal(w.Body.Bytes(), &children); err != nil {
		t.Fatalf("decoding JSON: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(children))
	}
	if children[0].ID != "child-a" {
		t.Errorf("children[0].ID = %q, want %q",
			children[0].ID, "child-a")
	}
	if children[1].ID != "child-b" {
		t.Errorf("children[1].ID = %q, want %q",
			children[1].ID, "child-b")
	}
}

func TestGetChildSessions_Empty(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "no-kids", "my-app", 5)

	w := te.get(t, "/api/v1/sessions/no-kids/children")
	assertStatus(t, w, http.StatusOK)

	var children []db.Session
	if err := json.Unmarshal(w.Body.Bytes(), &children); err != nil {
		t.Fatalf("decoding JSON: %v", err)
	}
	if len(children) != 0 {
		t.Fatalf("expected 0 children, got %d", len(children))
	}
}

func TestGetMessages_AscDefault(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 10)
	te.seedMessages(t, "s1", 10)

	w := te.get(t, "/api/v1/sessions/s1/messages")
	assertStatus(t, w, http.StatusOK)

	resp := decode[messageListResponse](t, w)
	if len(resp.Messages) != 10 {
		t.Fatalf("expected 10 messages, got %d",
			len(resp.Messages))
	}
	first := resp.Messages[0]
	last := resp.Messages[9]
	if first.Ordinal > last.Ordinal {
		t.Fatal("expected ascending ordinal order")
	}
}

func TestGetMessages_DescDefault(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 10)
	te.seedMessages(t, "s1", 10)

	w := te.get(t,
		"/api/v1/sessions/s1/messages?direction=desc",
	)
	assertStatus(t, w, http.StatusOK)

	resp := decode[messageListResponse](t, w)
	if len(resp.Messages) != 10 {
		t.Fatalf("expected 10 messages, got %d",
			len(resp.Messages))
	}
	first := resp.Messages[0]
	last := resp.Messages[len(resp.Messages)-1]
	if first.Ordinal < last.Ordinal {
		t.Fatal("expected descending ordinal order")
	}
}

func TestGetMessages_DescWithFrom(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 20)
	te.seedMessages(t, "s1", 20)

	w := te.get(t,
		"/api/v1/sessions/s1/messages?direction=desc&from=10&limit=5",
	)
	assertStatus(t, w, http.StatusOK)

	resp := decode[messageListResponse](t, w)
	if len(resp.Messages) != 5 {
		t.Fatalf("expected 5 messages, got %d",
			len(resp.Messages))
	}
	if resp.Messages[0].Ordinal != 10 {
		t.Fatalf("expected first ordinal=10, got %d",
			resp.Messages[0].Ordinal)
	}
}

func TestGetMessages_Pagination(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 20)
	te.seedMessages(t, "s1", 20)

	// First page
	w := te.get(t,
		"/api/v1/sessions/s1/messages?from=0&limit=5",
	)
	assertStatus(t, w, http.StatusOK)
	resp := decode[messageListResponse](t, w)
	if len(resp.Messages) != 5 {
		t.Fatalf("expected 5 messages, got %d",
			len(resp.Messages))
	}
	if resp.Messages[4].Ordinal != 4 {
		t.Fatalf("expected last ordinal=4, got %d",
			resp.Messages[4].Ordinal)
	}

	// Second page
	w = te.get(t,
		"/api/v1/sessions/s1/messages?from=5&limit=5",
	)
	assertStatus(t, w, http.StatusOK)
	resp = decode[messageListResponse](t, w)
	if len(resp.Messages) != 5 {
		t.Fatalf("expected 5 messages, got %d",
			len(resp.Messages))
	}
	if resp.Messages[0].Ordinal != 5 {
		t.Fatalf("expected first ordinal=5, got %d",
			resp.Messages[0].Ordinal)
	}
}

func TestGetMessages_InvalidParams(t *testing.T) {
	te := setup(t)

	tests := []struct {
		name string
		path string
	}{
		{"InvalidLimit", "/api/v1/sessions/s1/messages?limit=abc"},
		{"InvalidFrom", "/api/v1/sessions/s1/messages?from=xyz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := te.get(t, tt.path)
			assertStatus(t, w, http.StatusBadRequest)
		})
	}
}

func TestListSessions_InvalidLimit(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/sessions?limit=bad")
	assertStatus(t, w, http.StatusBadRequest)
}

func TestListSessions_InvalidCursor(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/sessions?cursor=invalid-cursor")
	assertStatus(t, w, http.StatusBadRequest)
}

func TestSearch_InvalidParams(t *testing.T) {
	te := setup(t)

	tests := []struct {
		name string
		path string
	}{
		{"InvalidLimit", "/api/v1/search?q=test&limit=nope"},
		{"InvalidCursor", "/api/v1/search?q=test&cursor=bad"},
		{"EmptyQuery", "/api/v1/search"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := te.get(t, tt.path)
			assertStatus(t, w, http.StatusBadRequest)
		})
	}
}

func TestGetMinimap(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 5)
	te.seedMessages(t, "s1", 5)

	w := te.get(t, "/api/v1/sessions/s1/minimap")
	assertStatus(t, w, http.StatusOK)

	resp := decode[minimapResponse](t, w)
	if len(resp.Entries) != 5 {
		t.Fatalf("expected 5 entries, got %d",
			len(resp.Entries))
	}
	if resp.Entries[0].Role == "" {
		t.Fatal("minimap should include role")
	}

	// Verify "content" is not present in raw JSON
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding raw JSON: %v", err)
	}
	entriesRaw, ok := raw["entries"].([]any)
	if !ok {
		t.Fatal("expected entries to be a list")
	}
	if len(entriesRaw) > 0 {
		first, ok := entriesRaw[0].(map[string]any)
		if !ok {
			t.Fatal("expected entry to be a map")
		}
		if _, ok := first["content"]; ok {
			t.Fatal("minimap should not include content")
		}
	}
}

func TestGetMinimap_FromOrdinal(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 5)
	te.seedMessages(t, "s1", 5)

	w := te.get(t, "/api/v1/sessions/s1/minimap?from=3")
	assertStatus(t, w, http.StatusOK)

	resp := decode[minimapResponse](t, w)
	if len(resp.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d",
			len(resp.Entries))
	}
	if got := resp.Entries[0].Ordinal; got != 3 {
		t.Fatalf("first ordinal = %d, want 3", got)
	}
}

func TestGetMinimap_InvalidFrom(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 1)
	te.seedMessages(t, "s1", 1)

	w := te.get(t, "/api/v1/sessions/s1/minimap?from=bad")
	assertStatus(t, w, http.StatusBadRequest)
}

func TestGetMinimap_MaxSampled(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 10)
	te.seedMessages(t, "s1", 10)

	w := te.get(t, "/api/v1/sessions/s1/minimap?max=3")
	assertStatus(t, w, http.StatusOK)

	resp := decode[minimapResponse](t, w)
	if len(resp.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d",
			len(resp.Entries))
	}
	if got := resp.Entries[0].Ordinal; got != 0 {
		t.Fatalf("first ordinal = %d, want 0", got)
	}
	if got := resp.Entries[1].Ordinal; got != 4 {
		t.Fatalf("second ordinal = %d, want 4", got)
	}
	if got := resp.Entries[2].Ordinal; got != 9 {
		t.Fatalf("third ordinal = %d, want 9", got)
	}
}

func TestGetMinimap_InvalidMax(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 1)
	te.seedMessages(t, "s1", 1)

	w := te.get(t, "/api/v1/sessions/s1/minimap?max=0")
	assertStatus(t, w, http.StatusBadRequest)
}

func TestSearch_WithResults(t *testing.T) {
	te := setup(t)
	if !te.db.HasFTS() {
		t.Skip("skipping search test: no FTS support")
	}
	te.seedSession(t, "s1", "my-app", 3)
	te.seedMessages(t, "s1", 3, func(i int, m *db.Message) {
		switch i {
		case 0:
			m.Role = "user"
			m.Content = "fix the login bug"
			m.ContentLength = 17
		case 1:
			m.Role = "assistant"
			m.Content = "looking at auth module"
			m.ContentLength = 22
		case 2:
			m.Role = "user"
			m.Content = "ship it"
			m.ContentLength = 7
		}
	})

	w := te.get(t, "/api/v1/search?q=login")
	assertStatus(t, w, http.StatusOK)

	resp := decode[searchResponse](t, w)
	if resp.Query != "login" {
		t.Fatalf("expected query=login, got %v", resp.Query)
	}
	if resp.Count < 1 {
		t.Fatal("expected at least 1 search result")
	}
}

func TestSearch_Limits(t *testing.T) {
	te := setup(t)
	if !te.db.HasFTS() {
		t.Skip("skipping search test: no FTS support")
	}
	// Seed enough messages to test limits
	te.seedSession(t, "s1", "my-app", 600)
	te.seedMessages(t, "s1", 600, func(i int, m *db.Message) {
		m.Content = "common search term"
		m.ContentLength = 18
	})

	tests := []struct {
		name      string
		queryVal  string
		wantCount int
	}{
		{"DefaultLimit", "", 50},          // default
		{"ExplicitLimit", "limit=10", 10}, // explicit
		{"ZeroLimit", "limit=0", 50},      // treat as default
		{"LargeLimit", "limit=1000", 500}, // clamped to 500
		{"ExactMax", "limit=500", 500},    // max allowed
		{"JustOver", "limit=501", 500},    // clamped to 500
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := "/api/v1/search?q=common"
			if tt.queryVal != "" {
				path += "&" + tt.queryVal
			}
			w := te.get(t, path)
			assertStatus(t, w, http.StatusOK)

			resp := decode[searchResponse](t, w)
			if resp.Count != tt.wantCount {
				t.Errorf("limit=%q: got %d results, want %d",
					tt.queryVal, resp.Count, tt.wantCount)
			}
		})
	}
}

func TestSearch_CanceledContext(t *testing.T) {
	te := setup(t)
	if !te.db.HasFTS() {
		t.Skip("skipping search test: no FTS support")
	}
	te.seedSession(t, "s1", "my-app", 1)
	te.seedMessages(t, "s1", 1, func(i int, m *db.Message) {
		m.Content = "searchable content"
		m.ContentLength = 18
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w := te.getWithContext(t, ctx, "/api/v1/search?q=searchable")

	// A canceled request should just return without writing a response
	// (implicit 200 with empty body in httptest, but importantly NO content).
	if w.Body.Len() > 0 {
		t.Errorf("expected empty body for canceled context, got: %s",
			w.Body.String())
	}
}

func TestSearch_DeadlineExceeded(t *testing.T) {
	te := setup(t)
	if !te.db.HasFTS() {
		t.Skip("skipping search test: no FTS support")
	}
	te.seedSession(t, "s1", "my-app", 1)
	te.seedMessages(t, "s1", 1, func(i int, m *db.Message) {
		m.Content = "searchable content"
		m.ContentLength = 18
	})

	ctx, cancel := expiredContext(t)
	defer cancel()

	w := te.getWithContext(t, ctx, "/api/v1/search?q=searchable")

	assertTimeoutRace(t, w)
}

func TestSearch_ZeroResults(t *testing.T) {
	te := setup(t)
	if !te.db.HasFTS() {
		t.Skip("skipping search test: no FTS support")
	}
	te.seedSession(t, "s1", "my-app", 1)
	te.seedMessages(t, "s1", 1)

	w := te.get(t, "/api/v1/search?q=spamalot")
	assertStatus(t, w, http.StatusOK)

	resp := decode[searchResponse](t, w)
	if resp.Results == nil {
		t.Fatal("results must be [] not null")
	}
	if resp.Count != 0 {
		t.Fatalf("expected count=0, got %d", resp.Count)
	}
}

func TestSearch_NotAvailable(t *testing.T) {
	te := setup(t)
	// Simulate missing FTS by dropping the virtual table.
	// HasFTS() will return false because the query against messages_fts will fail.
	err := te.db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec("DROP TABLE IF EXISTS messages_fts")
		return err
	})
	if err != nil {
		t.Fatalf("dropping messages_fts: %v", err)
	}

	w := te.get(t, "/api/v1/search?q=foo")
	assertStatus(t, w, http.StatusNotImplemented)
	assertErrorResponse(t, w, "search not available")
}

func TestGetStats(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 5)
	te.seedMessages(t, "s1", 5)

	w := te.get(t, "/api/v1/stats")
	assertStatus(t, w, http.StatusOK)

	resp := decode[db.Stats](t, w)
	if resp.SessionCount != 1 {
		t.Fatalf("expected 1 session, got %d",
			resp.SessionCount)
	}
	if resp.MessageCount != 5 {
		t.Fatalf("expected 5 messages, got %d",
			resp.MessageCount)
	}
}

func TestGetStats_ExcludeOneShotDefault(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 5, func(s *db.Session) {
		s.UserMessageCount = 1
	})
	te.seedSession(t, "s2", "my-app", 10, func(s *db.Session) {
		s.UserMessageCount = 5
	})
	te.seedMessages(t, "s1", 5)
	te.seedMessages(t, "s2", 10)

	// Default: exclude one-shot sessions.
	w := te.get(t, "/api/v1/stats")
	assertStatus(t, w, http.StatusOK)
	resp := decode[db.Stats](t, w)
	if resp.SessionCount != 1 {
		t.Errorf("default: session_count = %d, want 1",
			resp.SessionCount)
	}
	if resp.MessageCount != 10 {
		t.Errorf("default: message_count = %d, want 10",
			resp.MessageCount)
	}

	// Explicit include: all sessions.
	w = te.get(t, "/api/v1/stats?include_one_shot=true")
	assertStatus(t, w, http.StatusOK)
	resp = decode[db.Stats](t, w)
	if resp.SessionCount != 2 {
		t.Errorf("include: session_count = %d, want 2",
			resp.SessionCount)
	}
	if resp.MessageCount != 15 {
		t.Errorf("include: message_count = %d, want 15",
			resp.MessageCount)
	}
}

func TestListMachines_ExcludeOneShotDefault(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 5, func(s *db.Session) {
		s.Machine = "laptop"
		s.UserMessageCount = 1
	})
	te.seedSession(t, "s2", "my-app", 10, func(s *db.Session) {
		s.Machine = "desktop"
		s.UserMessageCount = 5
	})

	// Default: exclude one-shot sessions.
	w := te.get(t, "/api/v1/machines")
	assertStatus(t, w, http.StatusOK)
	resp := decode[machineListResponse](t, w)
	if len(resp.Machines) != 1 {
		t.Fatalf("default: expected 1 machine, got %d",
			len(resp.Machines))
	}
	if resp.Machines[0] != "desktop" {
		t.Errorf("default: expected desktop, got %s",
			resp.Machines[0])
	}

	// Explicit include: all machines.
	w = te.get(t, "/api/v1/machines?include_one_shot=true")
	assertStatus(t, w, http.StatusOK)
	resp = decode[machineListResponse](t, w)
	if len(resp.Machines) != 2 {
		t.Fatalf("include: expected 2 machines, got %d",
			len(resp.Machines))
	}
}

func TestListProjects(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 5)
	te.seedSession(t, "s2", "my-app", 3)
	te.seedSession(t, "s3", "other-app", 1)

	w := te.get(t, "/api/v1/projects")
	assertStatus(t, w, http.StatusOK)

	resp := decode[projectListResponse](t, w)
	if len(resp.Projects) != 2 {
		t.Fatalf("expected 2 projects, got %d",
			len(resp.Projects))
	}
}

func TestSyncStatus(t *testing.T) {
	te := setup(t)

	// Trigger a sync so LastSync is set
	w := te.post(t, "/api/v1/sync", "{}")
	assertStatus(t, w, http.StatusOK)

	w = te.get(t, "/api/v1/sync/status")
	assertStatus(t, w, http.StatusOK)

	resp := decode[syncStatusResponse](t, w)
	if resp.LastSync == "" {
		t.Fatal("expected last_sync field")
	}
}

func TestCORSHeaders(t *testing.T) {
	te := setup(t)

	// Request with matching origin should get CORS header.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assertStatus(t, w, http.StatusOK)

	cors := w.Header().Get("Access-Control-Allow-Origin")
	if cors != "http://127.0.0.1:0" {
		t.Fatalf("expected CORS origin http://127.0.0.1:0, got %q", cors)
	}
}

func TestCORSRejectsUnknownOrigin(t *testing.T) {
	te := setup(t)

	// GET from a foreign origin: allowed (read-only) but no CORS header.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	req.Header.Set("Origin", "http://evil-site.com")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assertStatus(t, w, http.StatusOK)

	cors := w.Header().Get("Access-Control-Allow-Origin")
	if cors != "" {
		t.Fatalf("expected no CORS header for foreign origin, got %q", cors)
	}
}

func TestCORSBlocksMutatingFromUnknownOrigin(t *testing.T) {
	te := setup(t)

	// POST from a foreign origin should be blocked (CSRF protection).
	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/sync", nil,
	)
	req.Header.Set("Origin", "http://evil-site.com")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assertStatus(t, w, http.StatusForbidden)
}

func TestCORSAllowsMutatingFromKnownOrigin(t *testing.T) {
	te := setup(t)

	// POST from the legitimate origin should succeed.
	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/sync", nil,
	)
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	// Sync returns 200 or 202, not 403.
	if w.Code == http.StatusForbidden {
		t.Fatal("legitimate origin should not be blocked")
	}
}

func TestCORSPreflightRejectsBadOrigin(t *testing.T) {
	te := setup(t)

	// OPTIONS preflight from foreign origin should return 403.
	req := httptest.NewRequest(
		http.MethodOptions, "/api/v1/sessions", nil,
	)
	req.Header.Set("Origin", "http://evil-site.com")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assertStatus(t, w, http.StatusForbidden)
}

func TestCORSBlocksMutatingWithNoOrigin(t *testing.T) {
	te := setup(t)

	// POST with no Origin header should be blocked (prevents
	// CSRF where browser omits Origin). Use srv.Handler()
	// directly to bypass the test wrapper that auto-sets Origin.
	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/sync", nil,
	)
	req.Host = "127.0.0.1:0"
	w := httptest.NewRecorder()
	te.srv.Handler().ServeHTTP(w, req)
	assertStatus(t, w, http.StatusForbidden)
}

func TestHostHeaderRejectsDNSRebinding(t *testing.T) {
	te := setup(t)

	// A DNS rebinding attack uses a custom domain that resolves
	// to 127.0.0.1. The Host header carries the attacker's domain.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	req.Host = "evil.attacker.com:8080"
	w := httptest.NewRecorder()
	te.srv.Handler().ServeHTTP(w, req)
	assertStatus(t, w, http.StatusForbidden)
}

func TestHostHeaderAllowsLegitimate(t *testing.T) {
	te := setup(t)

	// Requests with legitimate Host should pass.
	for _, host := range []string{
		"127.0.0.1:0",
		"localhost:0",
	} {
		req := httptest.NewRequest(
			http.MethodGet, "/api/v1/stats", nil,
		)
		req.Host = host
		w := httptest.NewRecorder()
		te.srv.Handler().ServeHTTP(w, req)
		if w.Code == http.StatusForbidden {
			t.Errorf("host %s should be allowed, got 403", host)
		}
	}
}

func TestHostHeaderAllowsConfiguredPublicOriginHost(t *testing.T) {
	te := setup(t, withPublicURL("http://viewer.example.test:8004"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	req.Host = "viewer.example.test:8004"
	w := httptest.NewRecorder()
	te.srv.Handler().ServeHTTP(w, req)
	assertStatus(t, w, http.StatusOK)
}

func TestHostHeaderPublicOriginsDoNotExpandTrustedHosts(t *testing.T) {
	te := setup(t, withPublicOrigins("http://viewer.example.test:8004"))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	req.Host = "viewer.example.test:8004"
	w := httptest.NewRecorder()
	te.srv.Handler().ServeHTTP(w, req)
	assertStatus(t, w, http.StatusForbidden)
}

func TestCORSAllowsConfiguredHTTPSPublicOrigin(t *testing.T) {
	te := setup(t, withPublicOrigins("https://viewer.example.test"))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
	req.Header.Set("Origin", "https://viewer.example.test")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	if w.Code == http.StatusForbidden {
		t.Fatal("configured public origin should not be blocked")
	}
}

func TestCORSAllowsLocalhost(t *testing.T) {
	te := setup(t)

	// localhost variant should also be allowed when bound to 127.0.0.1.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	req.Header.Set("Origin", "http://localhost:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assertStatus(t, w, http.StatusOK)

	cors := w.Header().Get("Access-Control-Allow-Origin")
	if cors != "http://localhost:0" {
		t.Fatalf("expected CORS origin http://localhost:0, got %q", cors)
	}
}

func TestHostHeaderBindAllPort80AllowsPortlessLoopback(t *testing.T) {
	for _, bindHost := range []string{"0.0.0.0", "::"} {
		t.Run(bindHost, func(t *testing.T) {
			te := setup(t, func(c *config.Config) {
				c.Host = bindHost
				c.Port = 80
			})

			for _, host := range []string{
				"127.0.0.1:80",
				"127.0.0.1",
				"localhost:80",
				"localhost",
				"[::1]:80",
				"[::1]",
			} {
				req := httptest.NewRequest(
					http.MethodGet, "/api/v1/stats", nil,
				)
				req.Host = host
				w := httptest.NewRecorder()
				te.srv.Handler().ServeHTTP(w, req)
				assertStatus(t, w, http.StatusOK)
			}
		})
	}
}

func TestCORSBindAllPort80AllowsPortlessLoopbackOrigins(t *testing.T) {
	for _, bindHost := range []string{"0.0.0.0", "::"} {
		t.Run(bindHost, func(t *testing.T) {
			te := setup(t, func(c *config.Config) {
				c.Host = bindHost
				c.Port = 80
			})

			for _, origin := range []string{
				"http://127.0.0.1:80",
				"http://127.0.0.1",
				"http://localhost:80",
				"http://localhost",
				"http://[::1]:80",
				"http://[::1]",
			} {
				req := httptest.NewRequest(
					http.MethodGet, "/api/v1/stats", nil,
				)
				req.Header.Set("Origin", origin)
				w := httptest.NewRecorder()
				te.handler.ServeHTTP(w, req)
				assertStatus(t, w, http.StatusOK)

				cors := w.Header().Get("Access-Control-Allow-Origin")
				if cors != origin {
					t.Fatalf(
						"origin %s: expected CORS %s, got %q",
						origin, origin, cors,
					)
				}
			}
		})
	}
}

func TestCORSBindAllPort80AllowsPortlessLANOrigin(t *testing.T) {
	lanIP := firstNonLoopbackIP(t)
	origin := "http://" + hostLiteral(lanIP)

	for _, bindHost := range []string{"0.0.0.0", "::"} {
		t.Run(bindHost, func(t *testing.T) {
			te := setup(t, func(c *config.Config) {
				c.Host = bindHost
				c.Port = 80
			})

			req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
			req.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			te.handler.ServeHTTP(w, req)
			assertStatus(t, w, http.StatusOK)

			cors := w.Header().Get("Access-Control-Allow-Origin")
			if cors != origin {
				t.Fatalf("expected CORS origin %s, got %q", origin, cors)
			}
		})
	}
}

func TestHostHeaderBindAllPort80AllowsPortlessLANIP(t *testing.T) {
	lanIP := firstNonLoopbackIP(t)
	host := hostLiteral(lanIP)

	for _, bindHost := range []string{"0.0.0.0", "::"} {
		t.Run(bindHost, func(t *testing.T) {
			te := setup(t, func(c *config.Config) {
				c.Host = bindHost
				c.Port = 80
			})

			req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
			req.Host = host
			w := httptest.NewRecorder()
			te.srv.Handler().ServeHTTP(w, req)
			assertStatus(t, w, http.StatusOK)
		})
	}
}

func TestCORSBindAllPort80RejectsNonLocalIPOrigin(t *testing.T) {
	const origin = "http://198.51.100.10"

	for _, bindHost := range []string{"0.0.0.0", "::"} {
		t.Run(bindHost, func(t *testing.T) {
			te := setup(t, func(c *config.Config) {
				c.Host = bindHost
				c.Port = 80
			})

			req := httptest.NewRequest(
				http.MethodPost, "/api/v1/sync", nil,
			)
			req.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			te.handler.ServeHTTP(w, req)
			assertStatus(t, w, http.StatusForbidden)
		})
	}
}

func TestHostHeaderBindAllPort80RejectsNonLocalIP(t *testing.T) {
	const host = "198.51.100.10"

	for _, bindHost := range []string{"0.0.0.0", "::"} {
		t.Run(bindHost, func(t *testing.T) {
			te := setup(t, func(c *config.Config) {
				c.Host = bindHost
				c.Port = 80
			})

			req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
			req.Host = host
			w := httptest.NewRecorder()
			te.srv.Handler().ServeHTTP(w, req)
			assertStatus(t, w, http.StatusForbidden)
		})
	}
}

func TestCORSBindAllInterfaces(t *testing.T) {
	for _, bindHost := range []string{"0.0.0.0", "::"} {
		t.Run(bindHost, func(t *testing.T) {
			te := setup(t, func(c *config.Config) {
				c.Host = bindHost
			})

			// In bind-all mode, all loopback origins must be allowed
			// (including IPv6 [::1]).
			for _, origin := range []string{
				"http://127.0.0.1:0",
				"http://localhost:0",
				"http://[::1]:0",
			} {
				req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
				req.Header.Set("Origin", origin)
				w := httptest.NewRecorder()
				te.handler.ServeHTTP(w, req)
				assertStatus(t, w, http.StatusOK)

				cors := w.Header().Get("Access-Control-Allow-Origin")
				if cors != origin {
					t.Errorf("origin %s: expected CORS %s, got %q", origin, origin, cors)
				}
			}
		})
	}
}

func TestCORSBindAllAllowsLANIPOrigin(t *testing.T) {
	lanIP := firstNonLoopbackIP(t)
	origin := "http://" + net.JoinHostPort(lanIP, "0")

	for _, bindHost := range []string{"0.0.0.0", "::"} {
		t.Run(bindHost, func(t *testing.T) {
			te := setup(t, func(c *config.Config) {
				c.Host = bindHost
			})

			req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
			req.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			te.handler.ServeHTTP(w, req)
			assertStatus(t, w, http.StatusOK)

			cors := w.Header().Get("Access-Control-Allow-Origin")
			if cors != origin {
				t.Fatalf("expected CORS origin %s, got %q", origin, cors)
			}
		})
	}
}

func TestHostHeaderBindAllAllowsLANIP(t *testing.T) {
	lanIP := firstNonLoopbackIP(t)
	host := net.JoinHostPort(lanIP, "0")

	for _, bindHost := range []string{"0.0.0.0", "::"} {
		t.Run(bindHost, func(t *testing.T) {
			te := setup(t, func(c *config.Config) {
				c.Host = bindHost
			})

			req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
			req.Host = host
			w := httptest.NewRecorder()
			te.srv.Handler().ServeHTTP(w, req)
			assertStatus(t, w, http.StatusOK)
		})
	}
}

func TestCORSBindAllRejectsNonLocalIPOrigin(t *testing.T) {
	const origin = "http://198.51.100.10:0"

	for _, bindHost := range []string{"0.0.0.0", "::"} {
		t.Run(bindHost, func(t *testing.T) {
			te := setup(t, func(c *config.Config) {
				c.Host = bindHost
			})

			req := httptest.NewRequest(
				http.MethodPost, "/api/v1/sync", nil,
			)
			req.Header.Set("Origin", origin)
			w := httptest.NewRecorder()
			te.handler.ServeHTTP(w, req)
			assertStatus(t, w, http.StatusForbidden)
		})
	}
}

func TestHostHeaderBindAllRejectsNonLocalIP(t *testing.T) {
	const host = "198.51.100.10:0"

	for _, bindHost := range []string{"0.0.0.0", "::"} {
		t.Run(bindHost, func(t *testing.T) {
			te := setup(t, func(c *config.Config) {
				c.Host = bindHost
			})

			req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
			req.Host = host
			w := httptest.NewRecorder()
			te.srv.Handler().ServeHTTP(w, req)
			assertStatus(t, w, http.StatusForbidden)
		})
	}
}

func TestCORSBindAllRejectsForeignOrigin(t *testing.T) {
	for _, bindHost := range []string{"0.0.0.0", "::"} {
		t.Run(bindHost, func(t *testing.T) {
			te := setup(t, func(c *config.Config) {
				c.Host = bindHost
			})

			req := httptest.NewRequest(
				http.MethodPost, "/api/v1/sync", nil,
			)
			req.Header.Set("Origin", "http://evil-site.com")
			w := httptest.NewRecorder()
			te.handler.ServeHTTP(w, req)
			assertStatus(t, w, http.StatusForbidden)
		})
	}
}

func TestHostHeaderBindAllRejectsDNSRebinding(t *testing.T) {
	for _, bindHost := range []string{"0.0.0.0", "::"} {
		t.Run(bindHost, func(t *testing.T) {
			te := setup(t, func(c *config.Config) {
				c.Host = bindHost
			})

			req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
			req.Host = "evil.attacker.com:8080"
			w := httptest.NewRecorder()
			te.srv.Handler().ServeHTTP(w, req)
			assertStatus(t, w, http.StatusForbidden)
		})
	}
}

func TestCORSVaryAlwaysSet(t *testing.T) {
	te := setup(t)

	// Vary: Origin should be set even for disallowed origins.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	req.Header.Set("Origin", "http://evil-site.com")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assertStatus(t, w, http.StatusOK)

	vary := w.Header().Get("Vary")
	if vary != "Origin" {
		t.Fatalf("expected Vary: Origin, got %q", vary)
	}
}

func TestCORSPreflight(t *testing.T) {
	te := setup(t)

	req := httptest.NewRequest(
		http.MethodOptions, "/api/v1/sessions", nil,
	)
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assertStatus(t, w, http.StatusNoContent)
}

func TestCORSAllowMethods(t *testing.T) {
	te := setup(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assertStatus(t, w, http.StatusOK)

	methods := w.Header().Get(
		"Access-Control-Allow-Methods",
	)
	for _, want := range []string{
		http.MethodGet, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
	} {
		if !strings.Contains(methods, want) {
			t.Errorf(
				"Allow-Methods %q missing %s",
				methods, want,
			)
		}
	}
}

func TestGetGithubConfig(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/config/github")
	assertStatus(t, w, http.StatusOK)

	resp := decode[githubConfigResponse](t, w)
	if resp.Configured {
		t.Fatal("expected configured=false")
	}
}

func TestExportSession(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 3)
	te.seedMessages(t, "s1", 3)

	w := te.get(t, "/api/v1/sessions/s1/export")
	assertStatus(t, w, http.StatusOK)

	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Fatalf("expected text/html content type, got %q", ct)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") {
		t.Fatalf("expected attachment disposition, got %q", cd)
	}
	assertBodyContains(t, w, "my-app")
}

func TestExportSession_NotFound(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/sessions/nonexistent/export")
	assertStatus(t, w, http.StatusNotFound)
}

func TestPublishSession_NoToken(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 3)

	w := te.post(t, "/api/v1/sessions/s1/publish", "{}")
	assertStatus(t, w, http.StatusUnauthorized)
}

func TestSetGithubConfig_InvalidInput(t *testing.T) {
	te := setup(t)

	tests := []struct {
		name string
		body string
	}{
		{"EmptyToken", `{"token": ""}`},
		{"InvalidJSON", `{bad json`},
		{"WhitespaceToken", `{"token": "   "}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := te.post(t, "/api/v1/config/github", tt.body)
			assertStatus(t, w, http.StatusBadRequest)
		})
	}
}

func TestPublishSession_NotFound(t *testing.T) {
	te := setup(t)
	te.srv.SetGithubToken("fake-token")

	w := te.post(t,
		"/api/v1/sessions/nonexistent/publish", "{}")
	assertStatus(t, w, http.StatusNotFound)
}

func TestExportSession_HTMLContent(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", 3)
	te.seedMessages(t, "s1", 3)

	w := te.get(t, "/api/v1/sessions/s1/export")
	assertStatus(t, w, http.StatusOK)

	body := w.Body.String()
	for _, want := range []string{
		"<!DOCTYPE html>",
		"<header>",
		"<main>",
		"message-content",
		"message-role",
		"Agent Session",
	} {
		if !strings.Contains(body, want) {
			t.Errorf(
				"expected to contain %q, got:\n%s",
				want, body,
			)
		}
	}
}

func TestUploadSession(t *testing.T) {
	te := setup(t)

	content := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsEarly, "Hello upload").
		AddClaudeAssistant(tsEarlyS5, "Hi!").
		String()

	w := te.upload(t, "upload-test.jsonl", content,
		"project=myproj&machine=remote")
	assertStatus(t, w, http.StatusOK)

	resp := decode[uploadResponse](t, w)
	if resp.SessionID != "upload-test" {
		t.Errorf("session_id = %v", resp.SessionID)
	}
	if resp.Project != "myproj" {
		t.Errorf("project = %v", resp.Project)
	}
	if resp.Machine != "remote" {
		t.Errorf("machine = %v", resp.Machine)
	}
	if resp.Messages != 2 {
		t.Errorf("messages = %v", resp.Messages)
	}

	sess, err := te.db.GetSession(context.Background(), "upload-test")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess == nil {
		t.Fatal("session not found in DB")
		return
	}
	if sess.Project != "myproj" {
		t.Errorf("stored project = %q", sess.Project)
	}
}

func TestUploadSession_InfersRelationshipType(t *testing.T) {
	te := setup(t)

	// Build a session whose first entry has a different sessionId,
	// making it a child session. The filename starts with "agent-"
	// so it should be inferred as a subagent.
	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID(
			tsEarly, "Run task", "parent-session",
		).
		AddClaudeAssistant(tsEarlyS5, "Done.").
		String()

	w := te.upload(t, "agent-task42.jsonl", content,
		"project=myproj&machine=remote")
	assertStatus(t, w, http.StatusOK)

	sess, err := te.db.GetSession(
		context.Background(), "agent-task42",
	)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess == nil {
		t.Fatal("session not found in DB")
		return
	}
	if sess.RelationshipType != "subagent" {
		t.Errorf(
			"RelationshipType = %q, want %q",
			sess.RelationshipType, "subagent",
		)
	}
}

func TestUploadSession_Errors(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		content  string
		query    string
	}{
		{
			"InvalidExtension",
			"bad.txt", "content", "project=myproj",
		},
		{
			"MissingProject",
			"test.jsonl", "{}", "",
		},
		{
			"TraversalProject",
			"test.jsonl", "{}", "project=../../../etc",
		},
		{
			"TraversalFilename",
			"..secret.jsonl", "{}", "project=safe",
		},
		{
			"DotPrefixProject",
			"test.jsonl", "{}", "project=.hidden",
		},
		{
			"DotPrefixFilename",
			".hidden.jsonl", "{}", "project=safe",
		},
		{
			"SlashInProject",
			"test.jsonl", "{}", "project=foo/bar",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te := setup(t)
			w := te.upload(t,
				tt.filename, tt.content, tt.query)
			assertStatus(t, w, http.StatusBadRequest)
		})
	}
}

func TestUploadSession_EmptyFile(t *testing.T) {
	te := setup(t)

	w := te.upload(t, "empty.jsonl", "",
		"project=myproj")
	assertStatus(t, w, http.StatusOK)

	resp := decode[uploadResponse](t, w)
	if resp.Messages != 0 {
		t.Errorf("messages = %v, want 0", resp.Messages)
	}
}

// noFlushWriter wraps an http.ResponseWriter without Flusher.
type noFlushWriter struct {
	http.ResponseWriter
}

func TestTriggerSync_NonStreaming(t *testing.T) {
	te := setup(t)

	// Seed a session file so we expect at least one session in the sync result.
	te.writeSessionFile(t, "test-proj", "sync-test.jsonl",
		testjsonl.NewSessionBuilder().
			AddClaudeUser(tsZero, "msg"),
	)

	rec := httptest.NewRecorder()
	nf := &noFlushWriter{rec}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
	req.Header.Set("Content-Type", "application/json")
	te.handler.ServeHTTP(nf, req)
	assertStatus(t, rec, http.StatusOK)

	resp := decode[syncResultResponse](t, rec)
	if resp.TotalSessions != 1 {
		t.Fatalf("expected 1 total_session, got %d", resp.TotalSessions)
	}
}

// flushRecorder wraps httptest.ResponseRecorder to implement
// http.Flusher, enabling SSE streaming tests.
type flushRecorder struct {
	*httptest.ResponseRecorder
	mu stdlibsync.Mutex
}

func (f *flushRecorder) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ResponseRecorder.Write(b)
}

func (f *flushRecorder) Flush() {
	f.ResponseRecorder.Flush()
}

func (f *flushRecorder) BodyString() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Body.String()
}

func TestTriggerSync_SSE(t *testing.T) {
	te := setup(t)

	te.writeSessionFile(t, "test-proj", "sse-test.jsonl",
		testjsonl.NewSessionBuilder().
			AddClaudeUser(tsZero, "msg"),
	)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	te.handler.ServeHTTP(w, req)

	te.waitForSSEEvent(t, w, "done", 5*time.Second)
	te.waitForSSEEvent(t, w, "progress", 5*time.Second)
}

func TestWatchSession_Events(t *testing.T) {
	te := setup(t)

	b := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "initial")
	content := b.String()
	sessionPath := te.writeSessionFile(t, "watch-proj", "watch-sess.jsonl", b)

	engine := sync.NewEngine(te.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {te.claudeDir},
			parser.AgentCodex:  {filepath.Join(te.dataDir, "codex")},
		},
		Machine: "test",
	})
	engine.SyncAll(nil)

	ctx, cancel := context.WithTimeout(
		context.Background(), 5*time.Second,
	)
	defer cancel()

	req := httptest.NewRequest(
		http.MethodGet, "/api/v1/sessions/watch-sess/watch", nil,
	).WithContext(ctx)
	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		te.handler.ServeHTTP(w, req)
		close(done)
	}()

	time.Sleep(200 * time.Millisecond)

	updated := content + testjsonl.NewSessionBuilder().
		AddClaudeAssistant(tsZeroS5, "response").
		String()
	if err := os.WriteFile(
		sessionPath, []byte(updated), 0o644,
	); err != nil {
		t.Fatalf("writing updated session file: %v", err)
	}

	te.waitForSSEEvent(t, w, "session_updated", 5*time.Second)
	cancel()
	<-done
}

func TestWatchSession_FileDisappearAndResolve(t *testing.T) {
	te := setup(t)

	b := testjsonl.NewSessionBuilder().
		AddClaudeUser(tsZero, "initial")
	content := b.String()
	sessionPath := te.writeSessionFile(t, "vanish-proj", "vanish-sess.jsonl", b)

	engine := sync.NewEngine(te.db, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {te.claudeDir},
			parser.AgentCodex:  {filepath.Join(te.dataDir, "codex")},
		},
		Machine: "test",
	})
	engine.SyncAll(nil)

	ctx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer cancel()

	req := httptest.NewRequest(
		http.MethodGet, "/api/v1/sessions/vanish-sess/watch", nil,
	).WithContext(ctx)
	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		te.handler.ServeHTTP(w, req)
		close(done)
	}()

	// Let the monitor start and record the initial mtime.
	time.Sleep(200 * time.Millisecond)

	// Delete the source file to simulate disappearance.
	if err := os.Remove(sessionPath); err != nil {
		t.Fatalf("removing session file: %v", err)
	}

	// Wait long enough for at least one poll tick to notice
	// the missing file and clear the cached path.
	time.Sleep(2 * time.Second)

	// Recreate the file with updated content at a NEW location
	// so we verify that FindSourceFile actually re-scans.
	updated := content + testjsonl.NewSessionBuilder().
		AddClaudeAssistant(tsZeroS5, "recovered").
		String()
	te.writeProjectFile(t, "moved-proj", "vanish-sess.jsonl", updated)

	te.waitForSSEEvent(t, w, "session_updated", 8*time.Second)
	cancel()
	<-done
}

func TestTriggerSync_SSEEvents(t *testing.T) {
	te := setup(t)

	for _, name := range []string{"a", "b"} {
		te.writeSessionFile(t, "sse-proj", name+".jsonl",
			testjsonl.NewSessionBuilder().
				AddClaudeUser(tsZero, fmt.Sprintf("msg %s", name)),
		)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	te.handler.ServeHTTP(w, req)

	events := parseSSE(w.BodyString())
	hasDone := false
	hasProgress := false
	for _, e := range events {
		if e.Event == "done" {
			hasDone = true
		}
		if e.Event == "progress" {
			hasProgress = true
		}
	}
	if !hasDone {
		t.Error("expected done event")
	}
	if !hasProgress {
		t.Error("expected progress event")
	}
}

func TestResyncEndpoint(t *testing.T) {
	te := setup(t)

	te.writeSessionFile(t, "resync-proj", "resync.jsonl",
		testjsonl.NewSessionBuilder().
			AddClaudeUser(tsZero, "msg resync"),
	)

	// Initial sync — session gets processed normally.
	syncReq := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
	syncW := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	te.handler.ServeHTTP(syncW, syncReq)

	syncStats := parseSSEDoneStats(t, syncW.BodyString())
	if syncStats.Synced != 1 {
		t.Fatalf("initial sync: synced = %d, want 1",
			syncStats.Synced)
	}

	// Second normal sync — file is unchanged so it's skipped.
	sync2Req := httptest.NewRequest(http.MethodPost, "/api/v1/sync", nil)
	sync2W := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	te.handler.ServeHTTP(sync2W, sync2Req)

	sync2Stats := parseSSEDoneStats(t, sync2W.BodyString())
	if sync2Stats.Synced != 0 {
		t.Fatalf("second sync: synced = %d, want 0 (skipped)",
			sync2Stats.Synced)
	}

	// Resync — should re-process the same unchanged file.
	resyncReq := httptest.NewRequest(
		http.MethodPost, "/api/v1/resync", nil,
	)
	resyncW := &flushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
	}
	te.handler.ServeHTTP(resyncW, resyncReq)

	resyncStats := parseSSEDoneStats(t, resyncW.BodyString())
	if resyncStats.Synced != 1 {
		t.Fatalf("resync: synced = %d, want 1 (reprocessed)",
			resyncStats.Synced)
	}
}

// TestResyncPreservesDataThroughSwap verifies the full resync
// flow end-to-end: initial sync, resync (which rebuilds the DB
// from scratch and swaps files), then verifies sessions and
// messages are accessible via the API. This exercises the
// close-rename-reopen sequence that is critical on Windows.
func TestResyncPreservesDataThroughSwap(t *testing.T) {
	te := setup(t)

	// Write two session files in different projects.
	te.writeSessionFile(t, "proj-a", "a.jsonl",
		testjsonl.NewSessionBuilder().
			AddClaudeUser(tsZero, "hello from proj-a").
			AddClaudeAssistant(tsZeroS5, "response a"),
	)
	te.writeSessionFile(t, "proj-b", "b.jsonl",
		testjsonl.NewSessionBuilder().
			AddClaudeUser(tsEarly, "hello from proj-b").
			AddClaudeAssistant(tsEarlyS5, "response b"),
	)

	// Initial sync.
	syncReq := httptest.NewRequest(
		http.MethodPost, "/api/v1/sync", nil,
	)
	syncW := &flushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
	}
	te.handler.ServeHTTP(syncW, syncReq)
	syncStats := parseSSEDoneStats(t, syncW.BodyString())
	if syncStats.Synced != 2 {
		t.Fatalf(
			"initial sync: synced = %d, want 2",
			syncStats.Synced,
		)
	}

	// Verify sessions are accessible before resync.
	w := te.get(t,
		"/api/v1/sessions?include_one_shot=true",
	)
	assertStatus(t, w, http.StatusOK)
	before := decode[sessionListResponse](t, w)
	if before.Total != 2 {
		t.Fatalf(
			"before resync: total = %d, want 2",
			before.Total,
		)
	}

	// Resync — rebuilds the database from scratch and swaps.
	resyncReq := httptest.NewRequest(
		http.MethodPost, "/api/v1/resync", nil,
	)
	resyncW := &flushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
	}
	te.handler.ServeHTTP(resyncW, resyncReq)
	resyncStats := parseSSEDoneStats(t, resyncW.BodyString())
	if resyncStats.Synced != 2 {
		t.Fatalf(
			"resync: synced = %d, want 2",
			resyncStats.Synced,
		)
	}

	// Verify sessions survived the DB swap.
	w = te.get(t,
		"/api/v1/sessions?include_one_shot=true",
	)
	assertStatus(t, w, http.StatusOK)
	after := decode[sessionListResponse](t, w)
	if after.Total != 2 {
		t.Fatalf(
			"after resync: total = %d, want 2",
			after.Total,
		)
	}

	// Verify messages are accessible for each session.
	for _, s := range after.Sessions {
		msgW := te.get(t, fmt.Sprintf(
			"/api/v1/sessions/%s/messages", s.ID,
		))
		assertStatus(t, msgW, http.StatusOK)
		msgs := decode[messageListResponse](t, msgW)
		if msgs.Count < 2 {
			t.Errorf(
				"session %s: messages = %d, want >= 2",
				s.ID, msgs.Count,
			)
		}
	}

	// Verify projects endpoint works (exercises reader pool).
	projW := te.get(t,
		"/api/v1/projects?include_one_shot=true",
	)
	assertStatus(t, projW, http.StatusOK)
	projects := decode[projectListResponse](t, projW)
	if len(projects.Projects) != 2 {
		t.Errorf(
			"projects = %d, want 2",
			len(projects.Projects),
		)
	}
}

// TestResyncConcurrentReads verifies that concurrent API reads
// don't panic or deadlock during resync, and that reads succeed
// after resync completes. During the close->rename->reopen
// window, SQLite may return various transient errors (database
// is closed, no such file, no such table). These are expected
// since resync is a rare manual operation.
func TestResyncConcurrentReads(t *testing.T) {
	te := setup(t)

	te.writeSessionFile(t, "conc-proj", "c.jsonl",
		testjsonl.NewSessionBuilder().
			AddClaudeUser(tsZero, "concurrent test"),
	)

	// Initial sync.
	syncReq := httptest.NewRequest(
		http.MethodPost, "/api/v1/sync", nil,
	)
	syncW := &flushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
	}
	te.handler.ServeHTTP(syncW, syncReq)

	// Spin up concurrent readers with a barrier to ensure
	// they are actively querying before resync starts.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg stdlibsync.WaitGroup
	var readersReady stdlibsync.WaitGroup
	readersReady.Add(4)

	for range 4 {
		wg.Go(func() {
			readySignaled := false
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				req := httptest.NewRequest(
					http.MethodGet, "/api/v1/sessions",
					nil,
				)
				w := httptest.NewRecorder()
				te.handler.ServeHTTP(w, req)

				if !readySignaled && w.Code == http.StatusOK {
					readersReady.Done()
					readySignaled = true
				}
				// Transient 500s are expected during the
				// close->reopen window. We only care that
				// no panics/deadlocks occur and reads
				// succeed after resync (verified below).
			}
		})
	}

	// Wait for all readers to complete at least one
	// successful request before triggering resync.
	readersReady.Wait()

	// Trigger resync while readers are active.
	resyncReq := httptest.NewRequest(
		http.MethodPost, "/api/v1/resync", nil,
	)
	resyncW := &flushRecorder{
		ResponseRecorder: httptest.NewRecorder(),
	}
	te.handler.ServeHTTP(resyncW, resyncReq)

	// Verify resync actually succeeded.
	resyncStats := parseSSEDoneStats(t, resyncW.BodyString())
	if resyncStats.Synced != 1 {
		t.Errorf(
			"resync: synced = %d, want 1",
			resyncStats.Synced,
		)
	}

	cancel()
	wg.Wait()

	// The real assertion: reads must succeed after resync
	// completes. If the close->reopen cycle left the DB
	// in a bad state, this will fail.
	w := te.get(t,
		"/api/v1/sessions?include_one_shot=true",
	)
	assertStatus(t, w, http.StatusOK)
	resp := decode[sessionListResponse](t, w)
	if resp.Total != 1 {
		t.Errorf("post-resync sessions = %d, want 1", resp.Total)
	}
}

// parseSSEDoneStats extracts the SyncStats from the "done" SSE
// event in a response body. Fails the test if no done event.
func parseSSEDoneStats(
	t *testing.T, body string,
) syncResultResponse {
	t.Helper()
	events := parseSSE(body)
	for _, e := range events {
		if e.Event == "done" {
			var stats syncResultResponse
			if err := json.Unmarshal([]byte(e.Data), &stats); err != nil {
				t.Fatalf("parsing done data: %v", err)
			}
			return stats
		}
	}
	t.Fatal("no done event in SSE stream")
	return syncResultResponse{}
}

func TestListSessions_Limits(t *testing.T) {
	te := setup(t)
	for i := range db.MaxSessionLimit + 5 {
		te.seedSession(t, fmt.Sprintf("s%d", i), "my-app", 1)
	}

	tests := []struct {
		name      string
		limitVal  string
		wantCount int
	}{
		{"DefaultLimit", "", db.DefaultSessionLimit},
		{"ExplicitLimit", "limit=10", 10},
		{"LargeLimit", "limit=1000", db.MaxSessionLimit},
		{"ExactMax", fmt.Sprintf("limit=%d", db.MaxSessionLimit), db.MaxSessionLimit},
		{"JustOver", fmt.Sprintf("limit=%d", db.MaxSessionLimit+1), db.MaxSessionLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := "/api/v1/sessions"
			if tt.limitVal != "" {
				path += "?" + tt.limitVal
			}
			w := te.get(t, path)
			assertStatus(t, w, http.StatusOK)

			resp := decode[sessionListResponse](t, w)
			if len(resp.Sessions) != tt.wantCount {
				t.Errorf("limit=%q: got %d sessions, want %d",
					tt.limitVal, len(resp.Sessions), tt.wantCount)
			}
		})
	}
}

func TestGetMessages_Limits(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "my-app", db.MaxMessageLimit+5)
	te.seedMessages(t, "s1", db.MaxMessageLimit+5)

	tests := []struct {
		name      string
		limitVal  string
		wantCount int
	}{
		{"DefaultLimit", "", db.DefaultMessageLimit},
		{"ExplicitLimit", "limit=10", 10},
		{"LargeLimit", "limit=2000", db.MaxMessageLimit},
		{"ExactMax", fmt.Sprintf("limit=%d", db.MaxMessageLimit), db.MaxMessageLimit},
		{"JustOver", fmt.Sprintf("limit=%d", db.MaxMessageLimit+1), db.MaxMessageLimit},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := "/api/v1/sessions/s1/messages"
			if tt.limitVal != "" {
				path += "?" + tt.limitVal
			}
			w := te.get(t, path)
			assertStatus(t, w, http.StatusOK)

			resp := decode[messageListResponse](t, w)
			if len(resp.Messages) != tt.wantCount {
				t.Errorf("limit=%q: got %d messages, want %d",
					tt.limitVal, len(resp.Messages), tt.wantCount)
			}
		})
	}
}

func TestGetVersion(t *testing.T) {
	v := server.VersionInfo{
		Version:   "v1.2.3",
		Commit:    "abc1234",
		BuildDate: "2025-01-15T00:00:00Z",
	}
	te := setupWithServerOpts(t, []server.Option{
		server.WithVersion(v),
	})

	w := te.get(t, "/api/v1/version")
	assertStatus(t, w, http.StatusOK)

	resp := decode[server.VersionInfo](t, w)
	if resp.Version != "v1.2.3" {
		t.Errorf("version = %q, want v1.2.3", resp.Version)
	}
	if resp.Commit != "abc1234" {
		t.Errorf("commit = %q, want abc1234", resp.Commit)
	}
	if resp.BuildDate != "2025-01-15T00:00:00Z" {
		t.Errorf(
			"build_date = %q, want 2025-01-15T00:00:00Z",
			resp.BuildDate,
		)
	}
}

func TestGetVersion_Default(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/version")
	assertStatus(t, w, http.StatusOK)

	resp := decode[server.VersionInfo](t, w)
	if resp.Version != "" {
		t.Errorf("version = %q, want empty", resp.Version)
	}
}

func TestFindAvailablePortSkipsOccupied(t *testing.T) {
	// Bind a port on 127.0.0.1 so FindAvailablePort must skip it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	occupied := ln.Addr().(*net.TCPAddr).Port

	got := server.FindAvailablePort("127.0.0.1", occupied)
	if got == occupied {
		t.Errorf(
			"FindAvailablePort returned occupied port %d", occupied,
		)
	}

	// The returned port should be bindable on the same host.
	ln2, err := net.Listen(
		"tcp",
		fmt.Sprintf("127.0.0.1:%d", got),
	)
	if err != nil {
		t.Fatalf(
			"returned port %d not bindable: %v", got, err,
		)
	}
	ln2.Close()
}
