//go:build !(windows && arm64)

package duckdb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFileIdentityForPathDistinguishesFilesAndIsStable exercises the
// platform identity helper with assertions that hold on every platform:
// two coexisting files must have different identities, re-reading the same
// file's identity must be stable, the identity of a real file is never the
// zero value, and a missing file errors.
func TestFileIdentityForPathDistinguishesFilesAndIsStable(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.duckdb")
	pathB := filepath.Join(dir, "b.duckdb")
	require.NoError(t, os.WriteFile(pathA, []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(pathB, []byte("b"), 0o644))

	idA, err := fileIdentityForPath(pathA)
	require.NoError(t, err)
	idB, err := fileIdentityForPath(pathB)
	require.NoError(t, err)
	assert.NotEqual(t, idA, idB, "coexisting files must have distinct identities")
	assert.False(t, idA.isZero(), "a real file's identity must not be the zero value")
	assert.False(t, idB.isZero(), "a real file's identity must not be the zero value")

	again, err := fileIdentityForPath(pathA)
	require.NoError(t, err)
	assert.Equal(t, idA, again, "re-reading the same file's identity must be stable")

	_, err = fileIdentityForPath(filepath.Join(dir, "missing.duckdb"))
	assert.Error(t, err, "a missing file must error, not return a zero identity")
}

// TestFileIdentityForPathChangesAcrossRenameReplacement pins the property
// the marker binding relies on: after a different file is renamed over a
// path (the shape of both a rebuild swap and a manual replacement), the
// identity read through the path is the replacement's, not the original's.
func TestFileIdentityForPathChangesAcrossRenameReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.duckdb")
	replacement := filepath.Join(dir, "m.duckdb.new")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))
	require.NoError(t, os.WriteFile(replacement, []byte("new"), 0o644))

	before, err := fileIdentityForPath(path)
	require.NoError(t, err)
	replacementID, err := fileIdentityForPath(replacement)
	require.NoError(t, err)
	require.NoError(t, os.Rename(replacement, path))

	after, err := fileIdentityForPath(path)
	require.NoError(t, err)
	assert.Equal(t, replacementID, after,
		"the path must resolve to the replacement's identity after the rename")
	assert.NotEqual(t, before, after,
		"a rename replacement must change the identity seen through the path")
}
