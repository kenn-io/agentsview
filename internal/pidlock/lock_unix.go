//go:build !windows

package pidlock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func openLockedFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening lock: %w", err)
	}
	if err := syscall.Flock(
		int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB,
	); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) ||
			errors.Is(err, syscall.EAGAIN) {
			return nil, lockHeldError(path, err)
		}
		return nil, fmt.Errorf("locking: %w", err)
	}
	return file, nil
}

func releaseLockedFile(file *os.File) error {
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlocking lock: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing lock: %w", closeErr)
	}
	return nil
}
