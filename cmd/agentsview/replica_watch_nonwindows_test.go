//go:build !windows

package main

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func replicaWatchDataDirCheck(t *testing.T, dir string) func() {
	t.Helper()
	require.NoError(t, os.Chmod(dir, 0o755))
	return func() {
		t.Helper()
		info, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	}
}
