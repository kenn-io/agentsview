package mcp

import (
	"bytes"
	"encoding/base64"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Check the identity received by a client, independently of the server builder.
func assertServerIcon(t *testing.T, info *mcp.Implementation, version string) []byte {
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
	canonical, err := os.ReadFile("../../desktop/src-tauri/icons/128x128@2x.png")
	require.NoError(t, err)
	assert.Equal(t, canonical, data, "reuse the project's existing icon")
	return data
}

const iconInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"synthetic-client","version":"1"}}}`

func TestServerIconStdio(t *testing.T) {
	server, client := newInMemoryPair(t, newServer(ServeOptions{Version: "v1.2.3"}))
	t.Cleanup(func() {
		require.NoError(t, client.Close())
		require.NoError(t, server.Wait())
	})
	assertServerIcon(t, client.InitializeResult().ServerInfo, "v1.2.3")
}

func TestServerIconHTTP(t *testing.T) {
	handler := newHTTPHandler(ServeOptions{})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "synthetic-client", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: server.URL + "/mcp", MaxRetries: -1,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	declared := assertServerIcon(t, session.InitializeResult().ServerInfo, "dev")

	for _, token := range []string{"", "synthetic-icon-token"} {
		t.Run("token="+token, func(t *testing.T) {
			handler := newHTTPHandler(ServeOptions{Token: token})
			for _, path := range []string{"/icon.png", "/favicon.ico"} {
				for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
					t.Run(method+path, func(t *testing.T) {
						req := httptest.NewRequest(method, path, nil)
						req.Header.Set("Origin", "https://client.example")
						got := httptest.NewRecorder()
						handler.ServeHTTP(got, req)
						if method != http.MethodGet && method != http.MethodHead {
							assert.Equal(t, http.StatusMethodNotAllowed, got.Code)
							assert.Equal(t, "GET, HEAD", got.Header().Get("Allow"))
							return
						}
						require.Equal(t, http.StatusOK, got.Code)
						assert.Equal(t, "image/png", got.Header().Get("Content-Type"))
						assert.Equal(t, "nosniff", got.Header().Get("X-Content-Type-Options"))
						assert.Equal(t, "public, max-age=86400", got.Header().Get("Cache-Control"))
						assert.Equal(t, strconv.Itoa(len(declared)), got.Header().Get("Content-Length"))
						assert.Empty(t, got.Header().Get("WWW-Authenticate"))
						if method == http.MethodHead {
							assert.Empty(t, got.Body.Bytes())
						} else {
							assert.Equal(t, declared, got.Body.Bytes())
						}
					})
				}
			}
			if token == "" {
				return
			}
			// Public aliases must not expose MCP or neighboring paths without auth.
			for _, path := range []string{"/", "/mcp", "/icon.png/extra", "/favicon.ico/extra"} {
				for _, auth := range []string{"", "Bearer wrong"} {
					req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(iconInitialize))
					req.Header.Set("Authorization", auth)
					got := httptest.NewRecorder()
					handler.ServeHTTP(got, req)
					assert.Equal(t, http.StatusUnauthorized, got.Code, "path %s, auth %q", path, auth)
					assert.Equal(t, "Bearer", got.Header().Get("WWW-Authenticate"))
				}
			}
			for _, path := range []string{"/", "/mcp"} {
				req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1"+path, strings.NewReader(iconInitialize))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				got := httptest.NewRecorder()
				handler.ServeHTTP(got, req)
				assert.Equal(t, http.StatusOK, got.Code, "authorized MCP initialize at %s", path)
			}
		})
	}
}
