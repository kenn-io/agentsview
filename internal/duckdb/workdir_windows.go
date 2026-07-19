//go:build windows

package duckdb

import "os"

// checkWorkDirPrivate is a no-op on Windows: access control there is
// ACL-based, so Unix ownership and mode-bit checks do not apply. The
// symlink and directory-type checks in ensureMirrorWorkDir still run.
func checkWorkDirPrivate(_ string, _ os.FileInfo) error { return nil }
