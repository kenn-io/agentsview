//go:build !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !windows

package artifact

import (
	"errors"
	"os"
)

func tryLockFolderFile(*os.File) (bool, error) {
	return false, errors.ErrUnsupported
}

func unlockFolderFile(*os.File) error {
	return errors.ErrUnsupported
}
