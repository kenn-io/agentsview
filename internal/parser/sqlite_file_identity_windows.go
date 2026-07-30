//go:build windows

package parser

import (
	"os"

	"golang.org/x/sys/windows"
)

func sqliteFileIdentity(path string, _ os.FileInfo) (inode, device uint64) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer file.Close()

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(
		windows.Handle(file.Fd()), &info,
	); err != nil {
		return 0, 0
	}
	fileIndex := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return fileIndex, uint64(info.VolumeSerialNumber)
}
