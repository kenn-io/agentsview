//go:build windows

package sync

import "golang.org/x/sys/windows"

func codexStagingAvailableBytes(dir string) (uint64, error) {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var available, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(
		path, &available, &total, &totalFree,
	); err != nil {
		return 0, err
	}
	return available, nil
}
