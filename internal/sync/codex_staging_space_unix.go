//go:build aix || darwin || dragonfly || freebsd || linux || openbsd

package sync

import "golang.org/x/sys/unix"

func codexStagingAvailableBytes(dir string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
