//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package artifact

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryLockFolderFile(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	switch {
	case errors.Is(err, unix.EWOULDBLOCK), errors.Is(err, unix.EAGAIN):
		return false, nil
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}

func unlockFolderFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
