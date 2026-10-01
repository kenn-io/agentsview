package segfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenSpoolChildPinsDirectoryAcrossPathReplacement(t *testing.T) {
	spool := t.TempDir()
	incoming := filepath.Join(spool, SpoolIncomingDir)
	require.NoError(t, os.Mkdir(incoming, 0o755))
	parent, err := os.OpenRoot(spool)
	require.NoError(t, err)
	defer parent.Close()
	child, err := openSpoolChild(parent, SpoolIncomingDir, false)
	require.NoError(t, err)
	defer child.Close()

	original := filepath.Join(spool, "incoming-original")
	require.NoError(t, os.Rename(incoming, original))
	outside := t.TempDir()
	symlinkOrSkip(t, outside, incoming)

	require.NoError(t, child.WriteFile("pinned.txt", []byte("inside"), 0o600))
	assert.FileExists(t, filepath.Join(original, "pinned.txt"))
	assert.NoFileExists(t, filepath.Join(outside, "pinned.txt"))
}
