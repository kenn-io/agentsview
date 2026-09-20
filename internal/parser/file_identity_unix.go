//go:build unix

package parser

import "os"

// sourceFileHandleIdentity returns the stable filesystem identity of an open
// file: inode and device on Unix. Zeros mean the identity is unavailable.
func sourceFileHandleIdentity(f *os.File) (id, volume uint64) {
	info, err := f.Stat()
	if err != nil {
		return 0, 0
	}
	return sourceFileIdentity(info)
}
