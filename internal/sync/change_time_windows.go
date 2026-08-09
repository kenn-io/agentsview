//go:build windows

package sync

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ntfsEpochOffset is the number of 100-nanosecond intervals between the NTFS
// epoch (January 1, 1601) and the Unix epoch (January 1, 1970). Multiplying
// by 100 converts to Unix nanoseconds.
const ntfsEpochOffset = 116444736000000000

type windowsFileBasicInfo struct {
	creationTime   int64
	lastAccessTime int64
	lastWriteTime  int64
	changeTime     int64
	fileAttributes uint32
	_              uint32
}

func fileChangeTime(path string, _ os.FileInfo) (int64, bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer file.Close()

	var info windowsFileBasicInfo
	err = windows.GetFileInformationByHandleEx(
		windows.Handle(file.Fd()),
		windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil || info.changeTime == 0 {
		return 0, false
	}
	// Convert NTFS time (100 ns intervals since 1601) to Unix nanoseconds
	// so the returned value is directly comparable with time.Time.UnixNano()
	// and os.FileInfo.ModTime().UnixNano(). On darwin/linux the syscall
	// layer already returns Unix epoch values; Windows needs the conversion.
	return (info.changeTime - ntfsEpochOffset) * 100, true
}
