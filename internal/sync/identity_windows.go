//go:build windows

package sync

import "os"

// getFileIdentity returns zeros on Windows. Native file
// identity (NTFS FileID + volume serial) requires the
// GetFileInformationByHandle API; treating identity as
// unknown here disables the identity-changed check and
// falls back to size/mtime, which is the pre-existing
// behavior.
func getFileIdentity(info os.FileInfo) (inode, device int64) {
	return 0, 0
}
