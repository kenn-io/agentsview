//go:build windows

package sync

import (
	"os"

	"golang.org/x/sys/windows"
)

// getFileIdentity returns the stable file index and volume serial exposed by
// Windows. Together they distinguish an atomic replacement from an append,
// even when the replacement is larger than the stored source.
func getFileIdentity(path string, _ os.FileInfo) (inode, device int64) {
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
	return int64(fileIndex), int64(info.VolumeSerialNumber)
}
