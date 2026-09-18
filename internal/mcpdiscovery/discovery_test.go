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
			assert := assert.New(t)
			require := require.New(t)

			listener, err := (&net.ListenConfig{}).Listen(t.Context(), tc.network, tc.address)
			if err != nil && tc.network == "tcp6" {
				t.Skipf("IPv6 listener unavailable: %v", err)
			}
			require.NoError(err)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/mcp" {
					http.NotFound(w, r)
					return
				}
				_, _ = io.WriteString(w, "discovered listener")
			}))
			require.NoError(server.Listener.Close())
			server.Listener = listener
			server.Start()
			t.Cleanup(server.Close)
			dir := t.TempDir()
			require.NoError(os.Chmod(dir, 0o700))
			cleanup, err := Publish(dir, listener.Addr().String(), "", "")
			require.NoError(err)
			t.Cleanup(func() { require.NoError(cleanup()) })
			rows, err := List(dir)
			require.NoError(err)
			require.Len(rows, 1)
			endpoint, err := url.Parse(rows[0].URL)
			require.NoError(err)
			assert.Equal(tc.host, endpoint.Hostname())
			client := server.Client()
			client.Timeout = 5 * time.Second
			response, err := client.Get(rows[0].URL)
			require.NoError(err)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			require.NoError(err)
			assert.Equal(http.StatusOK, response.StatusCode)
			assert.Equal("discovered listener", string(body))
		})
	}
}

// Listener publication, status, and cleanup are the client discovery contract.
func TestPublishedListenerStatusAndCleanup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	require.NoError(os.Chmod(dir, 0o700))
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(listener.Close()) })
	cleanup, err := Publish(dir, listener.Addr().String(), "test-listener-token", "http://127.0.0.1:4321")
	require.NoError(err)
	rows, err := List(dir)
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal("http://"+listener.Addr().String()+"/mcp", rows[0].URL)
	assert.Equal("http://127.0.0.1:4321", rows[0].BackendURL)
	token, err := os.ReadFile(rows[0].TokenPath)
	require.NoError(err)
	assert.Equal("test-listener-token", string(token))
	require.NoError(cleanup())
	rows, err = List(dir)
	require.NoError(err)
	assert.Empty(rows)
}
