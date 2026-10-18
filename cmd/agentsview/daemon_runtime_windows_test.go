//go:build windows

package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testutil"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/safefileio"
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

func TestDaemonRuntimeMakesNewDataDirPrivate(t *testing.T) {
	dir := t.TempDir()
	require.Error(t, safefileio.ValidatePrivateDir(dir), "temp dirs inherit their parent's DACL")

	IsDaemonActive(dir)
	assert.NoError(t, safefileio.ValidatePrivateDir(dir))
}

func TestDaemonRuntimePreservesArchiveChangeTime(t *testing.T) {
	dir := t.TempDir()
	// Earlier versions already made the data dir private.
	require.NoError(t, safefileio.EnsurePrivateDir(dir))
	legacyLock := filepath.Join(dir, "daemon.lock")
	require.NoError(t, os.WriteFile(legacyLock, nil, 0o600))
	// An upgraded data directory can contain a record from a crashed daemon.
	rec := daemon.NewRuntimeRecord(daemonService, "test", daemon.Endpoint{
		Network: daemon.NetworkTCP, Address: "127.0.0.1:1",
	})
	rec.PID = deadPID(t)
	legacyPath := filepath.Join(dir, fmt.Sprintf("daemon.%d.json", rec.PID))
	body, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(legacyPath, body, 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o700))
	paths := []string{
		filepath.Join(dir, "sentinel"),
		filepath.Join(dir, "assets", "sentinel"),
	}
	before := make([]int64, len(paths))
	for i, path := range paths {
		require.NoError(t, os.WriteFile(path, []byte("archive content"), 0o600))
		before[i] = testutil.FileChangeTime(t, path)
	}
	IsDaemonActive(dir)
	FindDaemonRuntime(dir)
	IsDaemonStarting(dir)
	_, err = WriteDaemonRuntime(dir, "127.0.0.1", 1, "test", false)
	require.NoError(t, err)
	RemoveDaemonRuntime(dir)
	for i, path := range paths {
		assert.Equal(t, before[i], testutil.FileChangeTime(t, path), "%s ChangeTime changed", path)
	}
}
