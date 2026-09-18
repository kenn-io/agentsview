//go:build unix

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

// sourceFileHandleIdentity returns the stable filesystem identity of an open
// file: inode and device on Unix. Zeros mean the identity is unavailable.
func sourceFileHandleIdentity(f *os.File) (id, volume uint64) {
	info, err := f.Stat()
	if err != nil {
		return 0, 0
	}
	return sourceFileIdentity(info)
}
