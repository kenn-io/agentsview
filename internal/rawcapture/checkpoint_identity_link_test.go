package rawcapture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckpointFileIdentityHardlinkAndReplacement(t *testing.T) {
	dir := t.TempDir()
	path, link, next := filepath.Join(dir, "original"), filepath.Join(dir, "link"), filepath.Join(dir, "replacement")
	require.NoError(t, os.WriteFile(path, []byte("one"), 0o600))
	if err := os.Link(path, link); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	require.NoError(t, os.WriteFile(next, []byte("two"), 0o600))
	identity := func(name string) string {
		file, err := os.Open(name)
		require.NoError(t, err)
		defer file.Close()
		info, err := file.Stat()
		require.NoError(t, err)
		return CheckpointFileIdentity(file, info)
	}
	first := identity(path)
	require.NotEmpty(t, first)
	require.Equal(t, first, identity(link))
	require.NotEqual(t, first, identity(next))
}
