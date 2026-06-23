package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
