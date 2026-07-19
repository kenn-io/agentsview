//go:build !windows

package duckdb

import (
	"fmt"
	"os"
	"syscall"
)

// fileIdentityForPath returns the filesystem identity (device, inode) of
// the file at path. os.Stat works on files a live DuckDB process holds
// open: the DuckDB lock protects the file's content, not its attributes,
// which is the same property the probe's own stat and PrimeFileIdentity
// already rely on.
func fileIdentityForPath(path string) (fileIdentity, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileIdentity{}, fmt.Errorf(
			"statting %s for file identity: %w", path, err,
		)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, fmt.Errorf(
			"reading file identity of %s: unexpected FileInfo.Sys type %T",
			path, info.Sys(),
		)
	}
	// Dev is int32 on darwin and uint64 on linux; the conversion is a
	// no-op where the types already match.
	return fileIdentity{
		A: uint64(st.Dev),
		B: st.Ino,
	}, nil
}
