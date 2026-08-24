package postgres

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenRawUploadSpoolSyncsParentDirectory(t *testing.T) {
	dataDir := t.TempDir()
	var syncedPath string

	root, err := openRawUploadSpool(dataDir, func(path string) error {
		syncedPath = path
		return nil
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })

	assert.Equal(t, dataDir, syncedPath)
	assert.DirExists(t, filepath.Join(dataDir, rawUploadSpoolDirectory))
}
