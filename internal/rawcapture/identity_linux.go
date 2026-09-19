//go:build linux && !mips && !mipsle && !mips64 && !mips64le

package rawcapture

import (
	"fmt"
	"os"
	"syscall"
)

func stableFileIdentity(_ *os.File, info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
}
