//go:build !windows

package parser

import "os"

func sourceFileIdentityForFile(_ *os.File, info os.FileInfo) (inode, device uint64) {
	return sourceFileIdentity(info)
}

func sourceFileIdentityForPath(_ string, info os.FileInfo) (inode, device uint64) {
	return sourceFileIdentity(info)
}
