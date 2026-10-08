//go:build !windows

package git

import "golang.org/x/sys/unix"

func repoRootAccessible(path string) bool {
	return unix.Access(path, unix.X_OK) == nil
}

func repoRootOwned(path string) bool {
	var stat unix.Stat_t
	return unix.Lstat(path, &stat) == nil && stat.Uid == uint32(unix.Geteuid())
}

func repoRootDevice(path string) (uint64, error) {
	var stat unix.Stat_t
	err := unix.Stat(path, &stat)
	return uint64(stat.Dev), err //nolint:unconvert // Dev is int32 on Darwin.
}
