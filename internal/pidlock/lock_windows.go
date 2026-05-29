//go:build windows

package pidlock

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func openLockedFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening lock: %w", err)
	}
	var overlapped windows.Overlapped
	err = windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&overlapped,
	)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, lockHeldError(path, err)
		}
		return nil, fmt.Errorf("locking: %w", err)
	}
	return file, nil
}

func releaseLockedFile(file *os.File) error {
	var overlapped windows.Overlapped
	unlockErr := windows.UnlockFileEx(
		windows.Handle(file.Fd()), 0, 1, 0, &overlapped,
	)
	closeErr := file.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlocking lock: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing lock: %w", closeErr)
	}
	return nil
}
