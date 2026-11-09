package kept

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDiskEvictsLeastRecentlyOpened(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	var disk Disk
	require.NoError(t, disk.Open("test-usage", "mirror", 1024, 0))
	disk.maxBytes = int64(2 * (len(disk.reports.disk.header("v1")) + len("one")))
	disk.Put("first", "v1", []byte("one"))
	disk.Put("second", "v1", []byte("two"))
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(disk.reports.disk.path("first"), old, old))
	payload, ok := disk.Get("first", "v1")
	require.True(t, ok)
	require.Equal(t, "one", string(payload))
	require.NoError(t, os.Chtimes(disk.reports.disk.path("second"), old, old))
	disk.Put("third", "v1", []byte("six"))
	_, ok = disk.Get("second", "v1")
	require.False(t, ok)
	var restarted Disk
	require.NoError(t, restarted.Open("test-usage", "mirror", 1024, disk.maxBytes))
	payload, ok = restarted.Get("first", "v1")
	require.True(t, ok)
	require.Equal(t, "one", string(payload))
	_, ok = restarted.Get("first", "other")
	require.False(t, ok)
}

func TestDiskLimitsAndUnavailableDirectory(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	var disk Disk
	require.NoError(t, disk.Open("test-usage", "mirror", 128, 0))
	disk.Put("large", "v", make([]byte, 128))
	_, ok := disk.Get("large", "v")
	require.False(t, ok)
	disk.Put("small", "v", []byte("body"))
	require.NoError(t, os.WriteFile(disk.reports.disk.path("small"), []byte("corrupt"), 0o600))
	_, ok = disk.Get("small", "v")
	require.False(t, ok)
	t.Setenv("CACHE_DIRECTORY", disk.reports.disk.path("small"))
	var unavailable Disk
	require.Error(t, unavailable.Open("test-usage", "mirror", 128, 0))
	unavailable.Put("small", "v", []byte("body"))
	_, ok = unavailable.Get("small", "v")
	require.False(t, ok)
}

func TestDiskEnforcesByteLimitAtStartup(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	var disk Disk
	require.NoError(t, disk.Open("test-usage", "mirror", 1024, 0))
	disk.Put("old", "v", []byte("old"))
	disk.Put("new", "v", []byte("new"))
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(disk.reports.disk.path("old"), old, old))
	info, err := os.Stat(disk.reports.disk.path("new"))
	require.NoError(t, err)
	var restarted Disk
	require.NoError(t, restarted.Open("test-usage", "mirror", 1024, info.Size()))
	_, ok := restarted.Get("old", "v")
	require.False(t, ok)
	data, ok := restarted.Get("new", "v")
	require.True(t, ok)
	require.Equal(t, "new", string(data))
}
