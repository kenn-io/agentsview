//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestDaemonRuntimePreservesArchiveChangeTime(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "assets"), 0o700))
	paths := []string{
		filepath.Join(dir, "sentinel"),
		filepath.Join(dir, "assets", "sentinel"),
	}
	changeTime := func(path string) int64 {
		file, err := os.Open(path)
		require.NoError(t, err)
		defer file.Close()
		// FILE_BASIC_INFO uses four LARGE_INTEGER timestamps, followed by
		// the attributes and padding to an eight-byte boundary.
		var info struct {
			creationTime   int64
			lastAccessTime int64
			lastWriteTime  int64
			changeTime     int64
			fileAttributes uint32
			_              uint32
		}
		require.NoError(t, windows.GetFileInformationByHandleEx(
			windows.Handle(file.Fd()), windows.FileBasicInfo,
			(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)),
		))
		require.NotZero(t, info.changeTime)
		return info.changeTime
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
