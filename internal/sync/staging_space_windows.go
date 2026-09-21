//go:build windows

package sync

import (
	"golang.org/x/sys/windows"
)

// stagingDirFreeBytes returns the free bytes available in dir.
func stagingDirFreeBytes(dir string) (free uint64, ok bool, err error) {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false, err
	}
	var available, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(
		path, &available, &total, &totalFree,
	); err != nil {
		return 0, false, err
	}
	return available, true, nil
}
