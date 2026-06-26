//go:build unix

package parser

import (
	"os"
	"syscall"
)

func sourceFileIdentity(info os.FileInfo) (inode, device uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Ino), uint64(stat.Dev)
	}
	return 0, 0
}
