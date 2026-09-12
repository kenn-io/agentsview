package mcpdiscovery

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishedURLConnectsToListener(t *testing.T) {
	for _, tc := range []struct {
		name    string
		network string
		address string
		host    string
	}{
		{"IPv4 wildcard", "tcp4", "0.0.0.0:0", "127.0.0.1"},
		{"IPv6 wildcard", "tcp6", "[::]:0", "::1"},
		{"IPv4 loopback", "tcp4", "127.0.0.1:0", "127.0.0.1"},
		{"IPv6 loopback", "tcp6", "[::1]:0", "::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen(tc.network, tc.address)
			if err != nil && tc.network == "tcp6" {
				t.Skipf("IPv6 listener unavailable: %v", err)
			}
			require.NoError(t, err)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/mcp" {
					http.NotFound(w, r)
					return
				}
				_, _ = io.WriteString(w, "discovered listener")
			}))
			require.NoError(t, server.Listener.Close())
			server.Listener = listener
			server.Start()
			t.Cleanup(server.Close)
			dir := t.TempDir()
			require.NoError(t, os.Chmod(dir, 0o700))
			cleanup, err := Publish(dir, listener.Addr().String(), "", "")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, cleanup()) })
			rows, err := List(dir)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			endpoint, err := url.Parse(rows[0].URL)
			require.NoError(t, err)
			assert.Equal(t, tc.host, endpoint.Hostname())
			client := server.Client()
			client.Timeout = 5 * time.Second
			response, err := client.Get(rows[0].URL)
			require.NoError(t, err)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, response.StatusCode)
			assert.Equal(t, "discovered listener", string(body))
		})
	}
}

// Listener publication, status, and cleanup are the client discovery contract.
func TestPublishedListenerStatusAndCleanup(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })
	cleanup, err := Publish(dir, listener.Addr().String(), "test-listener-token", "http://127.0.0.1:4321")
	require.NoError(t, err)
	rows, err := List(dir)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "http://"+listener.Addr().String()+"/mcp", rows[0].URL)
	assert.Equal(t, "http://127.0.0.1:4321", rows[0].BackendURL)
	token, err := os.ReadFile(rows[0].TokenPath)
	require.NoError(t, err)
	assert.Equal(t, "test-listener-token", string(token))
	require.NoError(t, cleanup())
	rows, err = List(dir)
	require.NoError(t, err)
	assert.Empty(t, rows)
}
