//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestDaemonRuntimeFindsLegacyWindowsStartLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.lock")
	assert.False(t, isExternalDaemonStarting(dir))
	assert.NoFileExists(t, path)

	stdin := startExternalDaemonStartLockHelper(t, path)
	assert.True(t, isExternalDaemonStarting(dir))

	require.NoError(t, stdin.Close())
	require.Eventually(t, func() bool {
		return !isExternalDaemonStarting(dir)
	}, 5*time.Second, 10*time.Millisecond, "legacy daemon start lock should be released")
	assert.FileExists(t, path)

	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	assert.False(t, isExternalDaemonStarting(dir), "an unopenable lock is not a starting daemon")
	assert.DirExists(t, path)
}

func TestDaemonRuntimePreservesArchiveChangeTime(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o700))
	paths := []string{
		filepath.Join(dir, "sentinel"),
		filepath.Join(dir, "assets", "sentinel"),
	}
	changeTime := func(path string) int64 {
		value, ok := syncpkg.ChangeTime(path)
		require.True(t, ok)
		return value
	}
	before := make([]int64, len(paths))
	for i, path := range paths {
		require.NoError(t, os.WriteFile(path, []byte("archive content"), 0o600))
		before[i] = changeTime(path)
	}
	assert.False(t, IsDaemonActive(dir))
	assert.Nil(t, FindDaemonRuntime(dir))
	assert.False(t, IsDaemonStarting(dir))
	_, err := WriteDaemonRuntime(dir, "127.0.0.1", 1, "test", false)
	require.NoError(t, err)
	RemoveDaemonRuntime(dir)
	for i, path := range paths {
		assert.Equal(t, before[i], changeTime(path), "%s ChangeTime changed", path)
	}
}

func TestDaemonRuntimeFindsLegacyWindowsRecord(t *testing.T) {
	dir := t.TempDir()
	host, port := testPingServer(t)
	path, err := WriteDaemonRuntime(dir, host, port, "test", false)
	require.NoError(t, err)
	legacyPath := filepath.Join(dir, filepath.Base(path))
	require.NoError(t, os.Rename(path, legacyPath))

	rt := FindDaemonRuntime(dir)
	require.NotNil(t, rt)
	assert.Equal(t, legacyPath, rt.Record.SourcePath)
}
