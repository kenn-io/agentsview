//go:build darwin || linux

package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileChangeTimeDetectsSameStatRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("before"), 0o600))

	beforeInfo, err := os.Stat(path)
	require.NoError(t, err)
	beforeChange, ok := fileChangeTime(path, beforeInfo)
	require.True(t, ok, "native change time unavailable")

	require.NoError(t, os.WriteFile(path, []byte("after!"), 0o600))
	require.NoError(t, os.Chtimes(
		path, beforeInfo.ModTime(), beforeInfo.ModTime(),
	))
	afterInfo, err := os.Stat(path)
	require.NoError(t, err)
	afterChange, ok := fileChangeTime(path, afterInfo)
	require.True(t, ok, "native change time unavailable after rewrite")

	require.Equal(t, beforeInfo.Size(), afterInfo.Size(),
		"fixture must preserve size")
	require.Equal(t, beforeInfo.ModTime().UnixNano(), afterInfo.ModTime().UnixNano(),
		"fixture must restore mtime")
	assert.NotEqual(t, beforeChange, afterChange,
		"change time must catch a same-size rewrite with restored mtime")
}
