//go:build !windows

package git

import "golang.org/x/sys/unix"

func repoRootDevice(path string) (uint64, error) {
	var stat unix.Stat_t
	err := unix.Stat(path, &stat)
	return uint64(stat.Dev), err
}
