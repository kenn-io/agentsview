//go:build windows

package sync

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Directory enumeration can report stale size and write time while a writer
// holds the file open. Every signature field comes from one fresh handle.
// The handle reads attributes only and shares delete access, so an agent can
// rename or remove the source while the scanner holds it.
func sourceScanFileSignature(path string, _ os.DirEntry) (sourceScanSignature, bool, error) {
	name, err := windows.UTF16PtrFromString(sourceScanLongPath(path))
	if err != nil {
		return sourceScanSignature{}, false, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return sourceScanSignature{}, false, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { _ = windows.CloseHandle(handle) }() // Read-only metadata handle.
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return sourceScanSignature{}, false, &os.PathError{Op: "GetFileInformationByHandle", Path: path, Err: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return sourceScanSignature{}, false, nil
	}
	return sourceScanSignature{
		size:   int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow),
		mtime:  info.LastWriteTime.Nanoseconds(),
		inode:  int64(uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)),
		device: int64(info.VolumeSerialNumber),
	}, true, nil
}

// sourceScanLongPath keeps os.Open's long-path support for direct CreateFile
// calls by using the extended-length prefix on long absolute drive paths.
func sourceScanLongPath(path string) string {
	if len(path) < windows.MAX_PATH-12 || !filepath.IsAbs(path) || strings.HasPrefix(path, `\\`) {
		return path
	}
	return `\\?\` + filepath.Clean(path)
}
