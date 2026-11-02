//go:build !windows

package sync

import "os"

func sourceScanFileSignature(path string, entry os.DirEntry) (sourceScanSignature, bool, error) {
	info, err := entry.Info()
	if entry.Type()&os.ModeSymlink != 0 {
		info, err = os.Stat(path)
	}
	if err != nil || !info.Mode().IsRegular() {
		return sourceScanSignature{}, false, err
	}
	inode, device := getFileIdentity(path, info)
	return sourceScanSignature{size: info.Size(), mtime: info.ModTime().UnixNano(), inode: inode, device: device}, true, nil
}
