package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestReaderCapabilityRegistersRecallTool(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Agentsview-Recall-Queries", "non-recording")
		_, _ = w.Write([]byte(`{"api_version":4,"read_only":true}`))
	}))
	defer ts.Close()
	caps, err := servicehttp.ProbeHTTPServerCapabilities(t.Context(), ts.URL, "test")
	require.NoError(t, err)
	srv := newServer(ServeOptions{Service: servicehttp.NewHTTPBackendForServer(ts.URL, "test", caps)})
	serverSession, clientSession := newInMemoryPair(t, srv)
	tools, err := clientSession.ListTools(t.Context(), nil)
	require.NoError(t, err)
	names := make([]string, 0, len(tools.Tools))
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assert.Contains(t, names, ToolQueryRecall)
	require.NoError(t, clientSession.Close())
	require.NoError(t, serverSession.Wait())
}
