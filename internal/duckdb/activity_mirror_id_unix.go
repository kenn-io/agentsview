//go:build !windows

package duckdb

import (
	"fmt"
	"os"
	"syscall"

	"go.kenn.io/agentsview/internal/volumeid"
)

func activityFileID(_ *os.File, info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", volumeid.Stable(stat.Dev), stat.Ino)
}
