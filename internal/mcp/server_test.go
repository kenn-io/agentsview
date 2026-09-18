package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

// newInMemoryPair connects a real MCP client to srv over an in-memory
// transport, returning both sessions. The caller closes the client and
// waits on the server session.
func newInMemoryPair(
	t *testing.T, srv *mcp.Server,
) (*mcp.ServerSession, *mcp.ClientSession) {
	t.Helper()
	ctx := t.Context()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	require.NoError(t, err)
	client := mcp.NewClient(
		&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	require.NoError(t, err)
	return ss, cs
}

func callParams(name string, args map[string]any) *mcp.CallToolParams {
	return &mcp.CallToolParams{Name: name, Arguments: args}
}

func TestNewServer_RegistersSevenReadOnlyTools(t *testing.T) {
	require := require.New(t)

	d := dbtest.OpenTestDB(t)
	srv := newServer(ServeOptions{
		Service: service.NewDirectBackend(d, nil),
		Now:     func() time.Time { return fixedNow },
	})
	require.NotNil(srv)

	st, ct := newInMemoryPair(t, srv)
	tools, err := ct.ListTools(t.Context(), nil)
	require.NoError(err)
	require.Len(tools.Tools, 7)
	for _, tl := range tools.Tools {
		require.NotNil(tl.Annotations, "tool %s missing annotations", tl.Name)
		require.True(tl.Annotations.ReadOnlyHint,
			"tool %s should be annotated read-only", tl.Name)
	}
	require.NoError(ct.Close())
	require.NoError(st.Wait())
}

func TestNewServer_OmitsRecallToolForUnsupportedBackend(t *testing.T) {
	require := require.New(t)

	d := dbtest.OpenTestDB(t)
	srv := newServer(ServeOptions{
		Service: service.NewReadOnlyBackend(d),
		Now:     func() time.Time { return fixedNow },
	})

	st, ct := newInMemoryPair(t, srv)
	tools, err := ct.ListTools(t.Context(), nil)
	require.NoError(err)
	require.Len(tools.Tools, 6)
	for _, tool := range tools.Tools {
		assert.NotEqual(t, ToolQueryRecall, tool.Name)
	}
	require.NoError(ct.Close())
	require.NoError(st.Wait())
}

func TestServer_SearchSessionsBySessionID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := dbtest.OpenTestDB(t)
	rootID := "remote~U"
	dbtest.SeedSession(t, d, rootID, "root-project", func(s *db.Session) {
		s.SessionName = new("Root session")
		s.EndedAt = new("2024-01-01T00:00:00Z")
	})
	for i := range 1000 {
		dbtest.SeedSession(t, d, fmt.Sprintf("remote~U-E%04d", i), "fork-project", func(s *db.Session) {
			s.EndedAt = new("2025-01-01T00:00:00Z")
		})
	}

	srv := newServer(ServeOptions{
		Service: service.NewDirectBackend(d, nil),
		Now:     func() time.Time { return fixedNow },
	})
	st, ct := newInMemoryPair(t, srv)
	defer func() {
		require.NoError(ct.Close())
		require.NoError(st.Wait())
	}()

	res, err := ct.CallTool(t.Context(), callParams(ToolSearchSessions, map[string]any{
		"session_id": "U",
		"query":      "does-not-exist",
		"project":    "does-not-exist",
		"date_from":  "not-a-date",
		"cursor":     99,
		"limit":      1,
	}))
	require.NoError(err)
	require.False(res.IsError, "%+v", res.Content)

	var out searchSessionsOut
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(err)
	t.Logf("head: fixture_sessions=%d response=%s", 1001, raw)
	require.NoError(json.Unmarshal(raw, &out))
	require.Len(out.Results, 1)
	assert.Equal(rootID, out.Results[0].SessionID)
	assert.Equal("root-project", out.Results[0].Project)
	assert.Equal("Root session", out.Results[0].Name)
	assert.Empty(out.Results[0].Snippet)
	assert.Zero(out.Results[0].MatchOrdinal)
	assert.Nil(out.NextCursor)
}

func TestIsCleanStdioShutdown(t *testing.T) {
	assert := assert.New(t)

	t.Parallel()
	assert.True(isCleanStdioShutdown(nil))
	assert.True(isCleanStdioShutdown(context.Canceled))
	assert.True(isCleanStdioShutdown(io.EOF))
	assert.True(isCleanStdioShutdown(fmt.Errorf("wrap: %w", io.EOF)))
	assert.True(isCleanStdioShutdown(errors.New("server is closing: EOF")))
	assert.True(isCleanStdioShutdown(errors.New("connection closed")))
	assert.False(isCleanStdioShutdown(errors.New("boom")))
	assert.False(isCleanStdioShutdown(errors.New("open db: permission denied")))
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// TestServeStdio_ClientDisconnectIsClean drives a full session over an
// IOTransport and then closes the input pipe (an abrupt stdin EOF, as
// when a client process exits). Whatever Run returns - nil or the SDK's
// "server is closing" error, which races on timing - must be classified
// as a clean shutdown. This guards against an SDK message change silently
// turning client disconnects into fatal exits.
func TestServeStdio_ClientDisconnectIsClean(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	msgs := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized"}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
`
	// Retry to exercise both the nil and the "server is closing" race
	// outcomes; both must be recognized as clean.
	for range 30 {
		srv := newServer(ServeOptions{
			Service: service.NewDirectBackend(d, nil),
			Now:     func() time.Time { return fixedNow },
		})
		pr, pw := io.Pipe()
		tr := &mcp.IOTransport{Reader: pr, Writer: nopWriteCloser{io.Discard}}
		done := make(chan error, 1)
		go func() { done <- srv.Run(t.Context(), tr) }()
		_, _ = io.WriteString(pw, msgs)
		require.NoError(t, pw.Close())
		select {
		case err := <-done:
			assert.True(t, isCleanStdioShutdown(err),
				"client disconnect must be clean, got %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("server did not return after client disconnect")
		}
	}
}

func TestWithBearerAuth(t *testing.T) {
	assert := assert.New(t)

	t.Parallel()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := func(auth string) *http.Request {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		return r
	}
	serve := func(h http.Handler, auth string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req(auth))
		return rec.Code
	}

	// Empty token -> no auth wrapper, request passes through.
	assert.Equal(http.StatusOK, serve(withBearerAuth(ok, ""), ""))

	h := withBearerAuth(ok, "s3cret")
	assert.Equal(http.StatusUnauthorized, serve(h, ""), "missing header")
	assert.Equal(http.StatusUnauthorized, serve(h, "Bearer wrong"), "wrong token")
	assert.Equal(http.StatusUnauthorized, serve(h, "s3cret"), "missing Bearer prefix")
	assert.Equal(http.StatusOK, serve(h, "Bearer s3cret"), "correct token")
}

// TestHTTPHandler_DNSRebindingProtection guards the SDK's built-in
// localhost protection: a request reaching a loopback listener with a
// non-loopback Host header (the DNS-rebinding signature) must be
// rejected. This is regression coverage in case an SDK upgrade flips the
// default or DisableLocalhostProtection is ever set.
func TestHTTPHandler_DNSRebindingProtection(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	ts := httptest.NewServer(newHTTPHandler(ServeOptions{
		Service: service.NewDirectBackend(d, nil),
	}))
	t.Cleanup(ts.Close)

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"0"}}}`
	do := func(host string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL, strings.NewReader(body))
		require.NoError(t, err)
		if host != "" {
			req.Host = host
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	assert.Equal(t, http.StatusForbidden, do("evil.example:1234"),
		"spoofed non-loopback Host must be rejected (DNS rebinding)")
	assert.NotEqual(t, http.StatusForbidden, do(""),
		"legit loopback Host must pass the rebinding check")
}

// TestServeHTTP_ShutsDownOnContextCancel verifies the StreamableHTTP
// serve path tears down gracefully when its context is cancelled,
// returning context.Canceled (which the command treats as a clean exit).
func TestServeHTTP_ShutsDownOnContextCancel(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- ServeHTTP(ctx, ServeOptions{
			Service: service.NewDirectBackend(d, nil),
		}, "127.0.0.1:0")
	}()
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("ServeHTTP did not return after context cancel")
	}
}
