//go:build windows

package parser

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type codexIndexWindowsFileBasicInfo struct {
	creationTime   int64
	lastAccessTime int64
	lastWriteTime  int64
	changeTime     int64
	fileAttributes uint32
	_              uint32
}

func codexIndexChangeTime(path string, _ os.FileInfo) (int64, bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer file.Close()

	var info codexIndexWindowsFileBasicInfo
	err = windows.GetFileInformationByHandleEx(
		windows.Handle(file.Fd()),
		windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil || info.changeTime == 0 {
		return 0, false
	}
	return info.changeTime, true
}
