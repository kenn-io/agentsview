//go:build unix

package chromehost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shortDir returns a private directory short enough to hold a fallback socket;
// t.TempDir embeds the test name and can exceed the socket path limit.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "av") //nolint:usetesting // Keep the socket path below macOS's limit.
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	return dir
}

func TestSocketPathFallbackUsesPrivateRuntimeRoot(t *testing.T) {
	runtimeRoot, tempRoot, group, shared := shortDir(t), shortDir(t), shortDir(t), shortDir(t)
	require.NoError(t, os.Chmod(group, 0o750))
	require.NoError(t, os.Chmod(shared, os.ModeSticky|0o777))
	long := filepath.Join(t.TempDir(), strings.Repeat("data", 25))
	other := filepath.Join(t.TempDir(), strings.Repeat("atad", 25))

	t.Run("short data dir", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
		dir := shortDir(t)
		socket, err := SocketPath(dir)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, "chrome", "host.sock"), socket)
	})
	t.Run("XDG_RUNTIME_DIR", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", runtimeRoot)
		t.Setenv("TMPDIR", tempRoot)
		socket, err := SocketPath(long)
		require.NoError(t, err)
		assert.Equal(t, runtimeRoot, filepath.Dir(filepath.Dir(socket)))
		assert.Regexp(t, `/av-chrome-[0-9a-f]{16}/host\.sock$`, socket)
		again, err := SocketPath(long)
		require.NoError(t, err)
		assert.Equal(t, socket, again)
		distinct, err := SocketPath(other)
		require.NoError(t, err)
		assert.NotEqual(t, socket, distinct)
	})
	t.Run("temp dir when runtime dir is not private", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", group)
		t.Setenv("TMPDIR", tempRoot)
		socket, err := SocketPath(long)
		require.NoError(t, err)
		assert.Equal(t, tempRoot, filepath.Dir(filepath.Dir(socket)))
	})
	t.Run("temp dir when runtime dir is unset", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", "")
		t.Setenv("TMPDIR", tempRoot+"/")
		socket, err := SocketPath(long)
		require.NoError(t, err)
		assert.Equal(t, tempRoot, filepath.Dir(filepath.Dir(socket)))
	})
	t.Run("no private root", func(t *testing.T) {
		t.Setenv("XDG_RUNTIME_DIR", group)
		t.Setenv("TMPDIR", shared)
		_, err := SocketPath(long)
		require.ErrorContains(t, err, "set XDG_RUNTIME_DIR")
		require.ErrorContains(t, err, "not mode 0700")
		assert.NotContains(t, err.Error(), "\n", "the serve log prints this as one line")
	})
}
