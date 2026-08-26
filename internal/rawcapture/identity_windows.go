//go:build windows

package rawcapture

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func stableFileIdentity(file *os.File, _ os.FileInfo) string {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(
		windows.Handle(file.Fd()), &info,
	); err != nil {
		return ""
	}
	fileIndex := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return fmt.Sprintf("%d:%d", uint64(info.VolumeSerialNumber), fileIndex)
}
