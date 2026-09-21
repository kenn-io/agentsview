//go:build windows

package parser

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const codexNTFSEpochOffset = int64(116444736000000000)

type codexIndexWindowsFileBasicInfo struct {
	creationTime   int64
	lastAccessTime int64
	lastWriteTime  int64
	changeTime     int64
	fileAttributes uint32
	_              uint32
}

func codexIndexChangeTimeForFile(file *os.File, _ os.FileInfo) (int64, bool) {
	if file == nil {
		return 0, false
	}
	var info codexIndexWindowsFileBasicInfo
	err := windows.GetFileInformationByHandleEx(
		windows.Handle(file.Fd()),
		windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil || info.changeTime == 0 {
		return 0, false
	}
	return (info.changeTime - codexNTFSEpochOffset) * 100, true
}

func codexIndexChangeTime(path string, info os.FileInfo) (int64, bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer file.Close()
	return codexIndexChangeTimeForFile(file, info)
}
