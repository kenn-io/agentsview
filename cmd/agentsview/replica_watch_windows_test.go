//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testutil"
)

func replicaWatchDataDirCheck(t *testing.T, dir string) func() {
	t.Helper()
	path := filepath.Join(dir, "sentinel")
	require.NoError(t, os.WriteFile(path, []byte("archive content"), 0o600))
	before := testutil.FileChangeTime(t, path)

	return func() {
		t.Helper()
		assert.Equal(t, before, testutil.FileChangeTime(t, path), "%s ChangeTime changed", path)
	}
}
