package mcpdiscovery

import (
	"net"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
