//go:build windows

package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestDaemonRuntimeFindsLegacyWindowsStartLock(t *testing.T) {
	if path := os.Getenv("AGENTSVIEW_LEGACY_START_LOCK_HELPER"); path != "" {
		lock := flock.New(path)
		require.NoError(t, lock.Lock())
		fmt.Println("ready")
		_, err := io.Copy(io.Discard, os.Stdin)
		require.NoError(t, err)
		require.NoError(t, lock.Close())
		return
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.lock")
	assert.False(t, IsDaemonStarting(dir))
	assert.False(t, isExternalDaemonStarting(dir))
	assert.NoFileExists(t, path)

	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestDaemonRuntimeFindsLegacyWindowsStartLock$")
	cmd.Env = append(os.Environ(), "AGENTSVIEW_LEGACY_START_LOCK_HELPER="+path)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	waited := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", ready)
	assert.True(t, IsDaemonStarting(dir))
	assert.True(t, isExternalDaemonStarting(dir))

	require.NoError(t, stdin.Close())
	err = cmd.Wait()
	waited = true
	require.NoError(t, err)
	assert.False(t, IsDaemonStarting(dir))
	assert.False(t, isExternalDaemonStarting(dir))
	assert.FileExists(t, path)

	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	assert.False(t, IsDaemonStarting(dir), "an unopenable lock is not a starting daemon")
	assert.False(t, isExternalDaemonStarting(dir))
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
	for range 3 {
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
}

func TestDaemonRuntimeFindsLegacyWindowsRecord(t *testing.T) {
	dir := t.TempDir()
	host, port := testPingServer(t)
	path, err := WriteDaemonRuntime(dir, host, port, "test", false)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "run"), filepath.Dir(path))
	legacyPath := filepath.Join(dir, filepath.Base(path))
	require.NoError(t, os.Rename(path, legacyPath))

	rt := FindDaemonRuntime(dir)
	require.NotNil(t, rt)
	assert.Equal(t, legacyPath, rt.Record.SourcePath)
	records, err := daemonStatusRecords(dir, "")
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, legacyPath, records[0].SourcePath)
	assert.True(t, hasLiveDaemonRuntime(dir))
	assert.True(t, hasLiveWritableDaemonRuntime(dir))
	assert.Len(t, liveDaemonRecords(dir), 1)
	writable, err := writableDaemonRecords(dir, "")
	require.NoError(t, err)
	assert.Len(t, writable, 1)
	assert.FileExists(t, legacyPath)
}
