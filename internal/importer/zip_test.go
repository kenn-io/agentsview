package importer

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createTestZip(t *testing.T, files map[string]string) string {
	t.Helper()

	zipPath := filepath.Join(t.TempDir(), "test.zip")
	f, err := os.Create(zipPath)
	require.NoError(t, err)
	w := zip.NewWriter(f)
	for name, content := range files {
		fw, err := w.Create(name)
		require.NoError(t, err)
		_, err = fw.Write([]byte(content))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	require.NoError(t, f.Close())
	return zipPath
}

func TestExtractZip(t *testing.T) {
	zipPath := createTestZip(t, map[string]string{
		"conversations.json": `[{"uuid":"test"}]`,
		"subdir/file.txt":    "hello",
	})

	dir, cleanup, err := ExtractZip(zipPath)
	require.NoError(t, err)
	defer cleanup()

	data, err := os.ReadFile(filepath.Join(dir, "conversations.json"))
	require.NoError(t, err)
	assert.Contains(t, string(data), "test")

	data, err = os.ReadFile(filepath.Join(dir, "subdir", "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(data))
}

func TestExtractZip_InvalidPath(t *testing.T) {
	_, _, err := ExtractZip("/nonexistent.zip")
	require.Error(t, err)
}
