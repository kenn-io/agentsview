//go:build linux

package sync

import (
	"os"
	"syscall"
)

func fileChangeTime(_ string, info os.FileInfo) (int64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Ctim.Sec*1_000_000_000 + stat.Ctim.Nsec, true
}
