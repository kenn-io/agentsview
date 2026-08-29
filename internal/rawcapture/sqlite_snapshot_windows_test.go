package rawcapture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPinSQLiteSnapshotPathPreventsReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	writeSQLiteCaptureTestDB(t, path, "original")
	expected, err := os.Stat(path)
	require.NoError(t, err)
	pin, err := pinSQLiteSnapshotPath(path, expected)
	require.NoError(t, err)

	moved := path + ".moved"
	require.Error(t, os.Rename(path, moved))
	require.NoError(t, pin.Close())
	require.NoError(t, os.Rename(path, moved))
}
