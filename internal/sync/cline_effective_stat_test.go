package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClineEffectiveStat_TeammateFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	sessDir := filepath.Join(dir, "sess-123")
	require.NoError(os.MkdirAll(sessDir, 0o755))

	metaPath := filepath.Join(sessDir, "sess-123.json")
	require.NoError(os.WriteFile(metaPath, []byte(`{"id":"sess-123"}`), 0o644))

	t0 := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	require.NoError(os.Chtimes(metaPath, t0, t0))
	metaInfo, err := os.Stat(metaPath)
	require.NoError(err)

	// 1. Only metadata file exists.
	size, mtime := clineEffectiveStat(metaPath, metaInfo)
	assert.Equal(metaInfo.Size(), size)
	assert.Equal(metaInfo.ModTime().UnixNano(), mtime)

	// 2. Primary transcript file added with later mtime.
	msgPath := filepath.Join(sessDir, "sess-123.messages.json")
	msgData := []byte(`{"messages":[]}`)
	require.NoError(os.WriteFile(msgPath, msgData, 0o644))
	t1 := t0.Add(2 * time.Minute)
	require.NoError(os.Chtimes(msgPath, t1, t1))

	size, mtime = clineEffectiveStat(metaPath, metaInfo)
	expectedSize := metaInfo.Size() + int64(len(msgData))
	assert.Equal(expectedSize, size)
	assert.Equal(t1.UnixNano(), mtime)

	// 3. Teammate transcript file added with even later mtime.
	tmPath := filepath.Join(sessDir, "git-scout__t1abc.messages.json")
	tmData := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	require.NoError(os.WriteFile(tmPath, tmData, 0o644))
	t2 := t1.Add(3 * time.Minute)
	require.NoError(os.Chtimes(tmPath, t2, t2))

	size, mtime = clineEffectiveStat(metaPath, metaInfo)
	expectedSize += int64(len(tmData))
	assert.Equal(expectedSize, size)
	assert.Equal(t2.UnixNano(), mtime)

	// 4. Ignored files (directories, hidden, non-matching) must not affect size or mtime.
	require.NoError(os.MkdirAll(filepath.Join(sessDir, "dir__sub.messages.json"), 0o755))
	require.NoError(os.WriteFile(filepath.Join(sessDir, ".hidden__sub.messages.json"), []byte("skip"), 0o644))
	require.NoError(os.WriteFile(filepath.Join(sessDir, "unrelated.txt"), []byte("skip"), 0o644))
	require.NoError(os.WriteFile(filepath.Join(sessDir, "singlepart.messages.json"), []byte("skip"), 0o644))

	sizeAfterIgnored, mtimeAfterIgnored := clineEffectiveStat(metaPath, metaInfo)
	assert.Equal(expectedSize, sizeAfterIgnored)
	assert.Equal(t2.UnixNano(), mtimeAfterIgnored)
}
