//go:build !windows

package duckdb

import (
	"fmt"
	"os"
	"syscall"
)

// checkWorkDirPrivate verifies the mirror work directory belongs to the
// current user and is not writable by group or other, so nothing else on
// the machine can plant symlinks or swap rebuild temp files inside it (see
// ensureMirrorWorkDir). Directories created by older builds used 0755,
// which passes: only the write bits matter.
func checkWorkDirPrivate(workDir string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf(
			"duckdb mirror work directory %s: unsupported stat result %T",
			workDir, info.Sys(),
		)
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf(
			"duckdb mirror work directory %s is owned by uid %d, not the "+
				"current user (uid %d); remove it and retry",
			workDir, stat.Uid, os.Geteuid(),
		)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf(
			"duckdb mirror work directory %s is writable by group or other "+
				"(mode %04o); run chmod go-w on it or remove it and retry",
			workDir, perm,
		)
	}
	return nil
}
