//go:build windows

package testutil

import (
	"os"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

// FileChangeTime returns the NTFS change time in 100-nanosecond ticks since 1601.
func FileChangeTime(t *testing.T, path string) int64 {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()

	// FILE_BASIC_INFO stores timestamps as 100-nanosecond ticks since 1601.
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
