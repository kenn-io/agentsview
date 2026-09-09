//go:build windows

package parser

import (
	"os"

	"golang.org/x/sys/windows"
)

// sourceFileIdentityForFile returns the stable Windows identity of the exact
// descriptor being parsed: file index plus volume serial number.
func sourceFileIdentityForFile(file *os.File, _ os.FileInfo) (inode, device uint64) {
	if file == nil {
		return 0, 0
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(
		windows.Handle(file.Fd()), &info,
	); err != nil {
		return 0, 0
	}
	fileIndex := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return fileIndex, uint64(info.VolumeSerialNumber)
}

// sourceFileIdentityForPath opens path and returns the identity of that path's
// current file. Snapshot parsers must use sourceFileIdentityForFile instead so
// a concurrent path replacement cannot relabel an already-open descriptor.
func sourceFileIdentityForPath(path string, info os.FileInfo) (inode, device uint64) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer file.Close()
	return sourceFileIdentityForFile(file, info)
}
