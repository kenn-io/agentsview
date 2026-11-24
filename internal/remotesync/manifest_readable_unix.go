//go:build !windows

package remotesync

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func archiveFileReadable(path string) (bool, error) {
	err := unix.Faccessat(unix.AT_FDCWD, path, unix.R_OK, unix.AT_EACCESS)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.EINVAL) ||
		errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EOPNOTSUPP) {
		return probeOpenReadable(path)
	}
	if os.IsNotExist(err) || os.IsPermission(err) ||
		errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return false, nil
	}
	return false, err
}
