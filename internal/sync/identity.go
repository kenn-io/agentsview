//go:build unix

package sync

import (
	"os"
	"syscall"
)

// getFileIdentity extracts inode and device numbers from a
// file's stat info on Unix systems. Returns zeros if the
// stat data is unavailable in the expected form.
func getFileIdentity(info os.FileInfo) (inode, device int64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int64(stat.Ino), int64(stat.Dev)
	}
	return 0, 0
}
