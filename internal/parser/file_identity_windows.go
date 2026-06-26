//go:build windows

package parser

import "os"

func sourceFileIdentity(info os.FileInfo) (inode, device uint64) {
	return 0, 0
}
