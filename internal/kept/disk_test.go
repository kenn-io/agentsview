package kept

import (
	"encoding/json/v2"
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
	require.NoError(t, os.Chtimes(disk.reports.disk.path("third"), old, old))
	info, err := os.Stat(disk.reports.disk.path("first"))
	require.NoError(t, err)
	var trimmed Disk
	require.NoError(t, trimmed.Open("test-usage", "mirror", 1024, info.Size()))
	_, ok = trimmed.Get("third", "v1")
	require.False(t, ok)
	payload, ok = trimmed.Get("first", "v1")
	require.True(t, ok)
	require.Equal(t, "one", string(payload))
}

func TestDropJSON(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	var disk Disk
	require.NoError(t, disk.Open("test-search", "archive", 1024, 0))
	disk.Put("parts", "v1", []byte("part data"))
	_, retain, err := LoadJSON(&disk, "page", "v1", nil, func() (string, error) { return "secret snippet", nil })
	require.NoError(t, err)
	require.True(t, retain)
	disk.DropJSON("")
	_, ok := disk.Get("page", jsonVersion+"v1")
	require.False(t, ok)
	part, ok := disk.Get("parts", "v1")
	require.True(t, ok)
	require.Equal(t, "part data", string(part))
	data, err := json.Marshal("future snippet")
	require.NoError(t, err)
	disk.Put("future-other", "json|v99|v1", data)
	require.True(t, disk.ObserveJSON("v2"))
	_, ok = disk.Get("future-other", "json|v99|v1")
	require.False(t, ok)
}

func TestDiskLimitsAndUnavailableDirectory(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	var disk Disk
	require.NoError(t, disk.Open("test-usage", "mirror", 128, 0))
	disk.Put("large", "v", make([]byte, 128))
	_, err := os.Stat(disk.reports.disk.path("large"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, os.WriteFile(disk.reports.disk.path("large"), append(disk.reports.disk.header("v"), make([]byte, 128)...), 0o600))
	_, ok := disk.Get("large", "v")
	require.False(t, ok)
	disk.Put("small", "v", []byte("body"))
	t.Setenv("CACHE_DIRECTORY", disk.reports.disk.path("small"))
	var unavailable Disk
	require.Error(t, unavailable.Open("test-usage", "mirror", 128, 0))
	unavailable.Put("small", "v", []byte("body"))
	_, ok = unavailable.Get("small", "v")
	require.False(t, ok)
}

func TestLoadJSON(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	var disk Disk
	require.NoError(t, disk.Open("test-search", "archive", 1024, 0))
	loads := 0
	load := func() ([]string, error) { loads++; return nil, nil }
	_, retain, err := LoadJSON(&disk, "search", "v1", nil, load)
	require.NoError(t, err)
	require.True(t, retain)
	_, retain, err = LoadJSON(&disk, "search", "v1", func() bool { return false }, load)
	require.NoError(t, err)
	require.False(t, retain)
	require.Equal(t, 1, loads)
	_, retain, err = LoadJSON(&disk, "racing", "v1", func() bool { disk.DropJSON(""); return true }, func() ([]string, error) { return []string{"secret snippet"}, nil })
	require.NoError(t, err)
	require.False(t, retain)
	_, ok := disk.Get("racing", jsonVersion+"v1")
	require.False(t, ok)
	data, err := json.Marshal(map[string]string{"future": "snippet"})
	require.NoError(t, err)
	disk.Put("future", jsonVersion+"v1", data)
	got, retain, err := LoadJSON(&disk, "future", "v1", nil, func() ([]string, error) { return []string{"current"}, nil })
	require.NoError(t, err)
	require.True(t, retain)
	require.Equal(t, []string{"current"}, got)
}
