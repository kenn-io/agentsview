//go:build unix && (!linux || mips || mipsle || mips64 || mips64le)

package parser

import (
	"os"
	"syscall"
)

func sourceFileIdentity(info os.FileInfo) (inode, device uint64) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Ino, uint64(stat.Dev)
	}
	return 0, 0
}
