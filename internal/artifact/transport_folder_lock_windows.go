//go:build windows

package artifact

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryLockFolderFile(file *os.File) (bool, error) {
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&windows.Overlapped{},
	)
	switch {
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION),
		errors.Is(err, windows.ERROR_IO_PENDING):
		return false, nil
	case err != nil && !errors.Is(err, windows.Errno(0)):
		return false, err
	default:
		return true, nil
	}
}

func unlockFolderFile(file *os.File) error {
	err := windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		1,
		0,
		&windows.Overlapped{},
	)
	if errors.Is(err, windows.Errno(0)) {
		return nil
	}
	return err
}
