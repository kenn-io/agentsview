package kept

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDiskEvictsLeastRecentlyOpened(t *testing.T) {
	t.Run("bytes", func(t *testing.T) {
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
	})
	t.Run("files", func(t *testing.T) {
		t.Setenv("CACHE_DIRECTORY", t.TempDir())
		disk := Disk{MaxFiles: 2}
		require.NoError(t, disk.Open("test-usage", "mirror", 1024, 0))
		require.True(t, disk.Put("first", "v1", []byte("one")))
		require.True(t, disk.Put("second", "v1", []byte("two")))
		old := time.Now().Add(-time.Hour)
		require.NoError(t, os.Chtimes(disk.reports.disk.path("first"), old, old))
		require.True(t, disk.Put("third", "v1", []byte("three")))
		_, ok := disk.Get("first", "v1")
		require.False(t, ok)
		for _, key := range []string{"second", "third"} {
			_, ok := disk.Get(key, "v1")
			require.True(t, ok)
		}
	})
}

func TestDiskLimitsAndUnavailableDirectory(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	var disk Disk
	require.NoError(t, disk.Open("test-usage", "mirror", 128, 0))
	require.False(t, disk.Put("large", "v", make([]byte, 128)))
	require.True(t, disk.Put("boundary", "v", make([]byte, 128-len(disk.reports.disk.header("v")))))
	_, err := os.Stat(disk.reports.disk.path("large"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, os.WriteFile(disk.reports.disk.path("large"), append(disk.reports.disk.header("v"), make([]byte, 128)...), 0o600))
	_, ok := disk.Get("large", "v")
	require.False(t, ok)
	require.True(t, disk.Put("small", "v", []byte("body")))
	t.Setenv("CACHE_DIRECTORY", disk.reports.disk.path("small"))
	var unavailable Disk
	require.Error(t, unavailable.Open("test-usage", "mirror", 128, 0))
	require.False(t, unavailable.Put("small", "v", []byte("body")))
	_, ok = unavailable.Get("small", "v")
	require.False(t, ok)
}

func TestReadsDoSaved(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	var disk Disk
	require.NoError(t, disk.Open("saved-reads", "archive", 1024, 0))
	encode := func(s string) ([]byte, error) { return []byte(s), nil }
	decode := func(b []byte) (string, error) {
		if string(b) == "corrupt" {
			return "", errors.New("corrupt")
		}
		return string(b), nil
	}
	loads := 0
	load := func(context.Context) (string, error) { loads++; return "fresh", nil }
	var first Reads[string]
	got, err := first.DoSaved(t.Context(), &disk, "slot", "memory1", "saved1", encode, decode, load)
	require.NoError(t, err)
	require.Equal(t, "fresh", got)
	var second Reads[string]
	got, err = second.DoSaved(t.Context(), &disk, "slot", "memory2", "saved1", encode, decode, load)
	require.NoError(t, err)
	require.Equal(t, "fresh", got)
	require.Equal(t, 1, loads)
	path := disk.reports.disk.path("slot")
	old := time.Now().Add(-31 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))
	encodeCalls := 0
	countEncode := func(s string) ([]byte, error) { encodeCalls++; return []byte(s), nil }
	_, err = second.DoSaved(t.Context(), &disk, "slot", "memory2", "saved1", countEncode, decode, load)
	require.NoError(t, err)
	require.Zero(t, encodeCalls)
	disk.Sweep(time.Now())
	var third Reads[string]
	_, err = third.DoSaved(t.Context(), &disk, "slot", "memory3", "saved1", encode, decode, load)
	require.NoError(t, err)
	require.Equal(t, 1, loads)
	require.NoError(t, os.Remove(path))
	_, err = third.DoSaved(t.Context(), &disk, "slot", "memory3", "saved1", encode, decode, load)
	require.NoError(t, err)
	var fourth Reads[string]
	_, err = fourth.DoSaved(t.Context(), &disk, "slot", "memory4", "saved1", encode, decode, load)
	require.NoError(t, err)
	require.Equal(t, 1, loads)
	var fifth Reads[string]
	_, err = fifth.DoSaved(t.Context(), &disk, "slot", "memory5", "saved2", encode, decode, load)
	require.NoError(t, err)
	require.Equal(t, 2, loads)
	disk.Put("slot", "saved2", []byte("corrupt"))
	var sixth Reads[string]
	_, err = sixth.DoSaved(t.Context(), &disk, "slot", "memory6", "saved2", encode, decode, load)
	require.NoError(t, err)
	require.Equal(t, 3, loads)
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
	data, err := json.Marshal(map[string]string{"future": "snippet"})
	require.NoError(t, err)
	disk.Put("future", "v1", data)
	got, retain, err := LoadJSON(&disk, "future", "v1", nil, func() ([]string, error) { return []string{"current"}, nil })
	require.NoError(t, err)
	require.True(t, retain)
	require.Equal(t, []string{"current"}, got)
}
