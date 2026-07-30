//go:build unix

package parser

import "os"

func sqliteFileIdentity(_ string, info os.FileInfo) (inode, device uint64) {
	return sourceFileIdentity(info)
}
