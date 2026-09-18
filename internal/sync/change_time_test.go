//go:build darwin || linux

package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileChangeTimeDetectsSameStatRewrite(t *testing.T) {
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(os.WriteFile(path, []byte("before"), 0o600))

	beforeInfo, err := os.Stat(path)
	require.NoError(err)
	beforeChange, ok := fileChangeTime(path, beforeInfo)
	require.True(ok, "native change time unavailable")

	var afterInfo os.FileInfo
	afterChange := beforeChange
	deadline := time.Now().Add(2 * time.Second)
	for afterChange == beforeChange && time.Now().Before(deadline) {
		require.NoError(os.WriteFile(path, []byte("after!"), 0o600))
		require.NoError(os.Chtimes(
			path, beforeInfo.ModTime(), beforeInfo.ModTime(),
		))
		afterInfo, err = os.Stat(path)
		require.NoError(err)
		afterChange, ok = fileChangeTime(path, afterInfo)
		require.True(ok, "native change time unavailable after rewrite")
		if afterChange == beforeChange {
			time.Sleep(time.Millisecond)
		}
	}

	require.Equal(beforeInfo.Size(), afterInfo.Size(),
		"fixture must preserve size")
	require.Equal(beforeInfo.ModTime().UnixNano(), afterInfo.ModTime().UnixNano(),
		"fixture must restore mtime")
	assert.NotEqual(t, beforeChange, afterChange,
		"change time must catch a same-size rewrite with restored mtime")
}
