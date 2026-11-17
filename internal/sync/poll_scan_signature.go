//go:build !windows

package sync

import (
	"io/fs"
	"os"
)

// pollScanFileSignature reads one fresh lstat. A platform without change times
// cannot see an equal-size rewrite with a restored modification time, so it
// leaves the root ineligible.
func pollScanFileSignature(path string, entry fs.DirEntry) (pollScanSignature, error) {
	info, err := entry.Info()
	if err != nil {
		return pollScanSignature{}, err
	}
	ctime, ok := fileChangeTime(path, info)
	if !ok {
		return pollScanSignature{}, errPollScanIneligible
	}
	inode, device := getFileIdentity(path, info)
	return pollScanSignature{
		size: info.Size(), mtime: info.ModTime().UnixNano(), ctime: ctime,
		inode: inode, device: device,
	}, nil
}

func readPollScanDirectory(path string) ([]fs.DirEntry, error) {
	return os.ReadDir(path)
}
