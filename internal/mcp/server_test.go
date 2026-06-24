package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	ctx := context.Background()
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

func TestNewServer_RegistersSixReadOnlyTools(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	srv := newServer(ServeOptions{
		Service: service.NewDirectBackend(d, nil),
		Now:     func() time.Time { return fixedNow },
	})
	require.NotNil(t, srv)

	st, ct := newInMemoryPair(t, srv)
	tools, err := ct.ListTools(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, tools.Tools, 6)
	for _, tl := range tools.Tools {
		require.NotNil(t, tl.Annotations, "tool %s missing annotations", tl.Name)
		require.True(t, tl.Annotations.ReadOnlyHint,
			"tool %s should be annotated read-only", tl.Name)
	}
	require.NoError(t, ct.Close())
	require.NoError(t, st.Wait())
}

func TestIsCleanStdioShutdown(t *testing.T) {
	t.Parallel()
	assert.True(t, isCleanStdioShutdown(nil))
	assert.True(t, isCleanStdioShutdown(context.Canceled))
	assert.True(t, isCleanStdioShutdown(io.EOF))
	assert.True(t, isCleanStdioShutdown(fmt.Errorf("wrap: %w", io.EOF)))
	assert.True(t, isCleanStdioShutdown(errors.New("server is closing: EOF")))
	assert.True(t, isCleanStdioShutdown(errors.New("connection closed")))
	assert.False(t, isCleanStdioShutdown(errors.New("boom")))
	assert.False(t, isCleanStdioShutdown(errors.New("open db: permission denied")))
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
		go func() { done <- srv.Run(context.Background(), tr) }()
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

// TestServeHTTP_ShutsDownOnContextCancel verifies the StreamableHTTP
// serve path tears down gracefully when its context is cancelled,
// returning context.Canceled (which the command treats as a clean exit).
func TestServeHTTP_ShutsDownOnContextCancel(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- ServeHTTP(ctx, ServeOptions{
			Service: service.NewDirectBackend(d, nil),
		}, "127.0.0.1:0")
	}()
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("ServeHTTP did not return after context cancel")
	}
}
