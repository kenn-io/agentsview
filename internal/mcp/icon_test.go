package mcp

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Check the identity received by a client, independently of the server builder.
func assertServerIcon(t *testing.T, info *mcp.Implementation, version string) {
	t.Helper()
	require.NotNil(t, info)
	assert.Equal(t, "agentsview", info.Name)
	assert.Equal(t, "agentsview session history", info.Title)
	assert.Equal(t, version, info.Version)
	assert.Equal(t, "https://agentsview.io", info.WebsiteURL)
	require.Len(t, info.Icons, 1)
	icon := info.Icons[0]
	assert.Equal(t, "image/png", icon.MIMEType)
	assert.Equal(t, []string{"256x256"}, icon.Sizes)
	const prefix = "data:image/png;base64,"
	require.True(t, strings.HasPrefix(icon.Source, prefix))
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(icon.Source, prefix))
	require.NoError(t, err)
	require.Less(t, len(data), 16*1024, "keep server metadata small")
	img, err := png.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, 256, img.Bounds().Dx())
	assert.Equal(t, 256, img.Bounds().Dy())
}

func TestServerIconStdio(t *testing.T) {
	server, client := newInMemoryPair(t, newServer(ServeOptions{Version: "v1.2.3"}))
	t.Cleanup(func() {
		require.NoError(t, client.Close())
		require.NoError(t, server.Wait())
	})
	assertServerIcon(t, client.InitializeResult().ServerInfo, "v1.2.3")
}

func TestServerIconHTTP(t *testing.T) {
	server := httptest.NewServer(newHTTPHandler(ServeOptions{}))
	t.Cleanup(server.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "synthetic-client", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: server.URL + "/mcp", MaxRetries: -1,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	assertServerIcon(t, session.InitializeResult().ServerInfo, "dev")
}
