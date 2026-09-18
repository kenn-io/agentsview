package duckdb

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSameMirrorFile pins the strict identity comparison replacement
// watchers rely on: os.SameFile alone is not enough because Windows loads
// a FileInfo's file identity lazily, so a rename that completed before the
// first comparison makes the stale FileInfo adopt the new file's identity
// and compare equal forever. ModTime/Size are captured eagerly by Stat on
// all platforms, so an in-place content change (same inode, os.SameFile
// true) must still be reported as a different file.
func TestSameMirrorFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.duckdb")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))
	first, err := os.Stat(path)
	require.NoError(t, err)
	again, err := os.Stat(path)
	require.NoError(t, err)

	assert.True(t, SameMirrorFile(first, again),
		"an untouched file must compare as the same mirror file")
	assert.False(t, SameMirrorFile(nil, again))
	assert.False(t, SameMirrorFile(first, nil))

	// Same path and inode, different mtime: rewrite in place and force a
	// distinct mtime so coarse filesystem timestamp granularity cannot make
	// the two stats accidentally equal.
	require.NoError(t, os.WriteFile(path, []byte("v2"), 0o644))
	require.NoError(t, os.Chtimes(
		path, time.Time{}, first.ModTime().Add(2*time.Second),
	))
	replaced, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, SameMirrorFile(first, replaced),
		"a changed mtime must be reported as a replaced mirror file "+
			"even when os.SameFile still matches")

	// Same mtime but different size must also be a mismatch.
	require.NoError(t, os.WriteFile(path, []byte("v3-longer"), 0o644))
	require.NoError(t, os.Chtimes(path, time.Time{}, first.ModTime()))
	resized, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, SameMirrorFile(first, resized))
}
