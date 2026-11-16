//go:build windows

package duckdb

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func activityFileID(file *os.File, _ os.FileInfo) string {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", info.VolumeSerialNumber, uint64(info.FileIndexHigh)<<32|uint64(info.FileIndexLow))
}
