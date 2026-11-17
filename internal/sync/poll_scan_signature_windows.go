//go:build windows

package sync

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pollScanFileSignature reads every field from one fresh handle. Directory
// listings can report a stale size while a writer holds the file open. The
// handle reads attributes only and shares delete access, so an agent can still
// rename or remove the file during the scan.
func pollScanFileSignature(path string, _ fs.DirEntry) (pollScanSignature, error) {
	name, err := windows.UTF16PtrFromString(pollScanLongPath(path))
	if err != nil {
		return pollScanSignature{}, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			err = fs.ErrNotExist
		}
		return pollScanSignature{}, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer func() { _ = windows.CloseHandle(handle) }() // Attribute-only handle.
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return pollScanSignature{}, &os.PathError{Op: "GetFileInformationByHandle", Path: path, Err: err}
	}
	var basic windowsFileBasicInfo
	if err := windows.GetFileInformationByHandleEx(
		handle, windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)),
	); err != nil {
		return pollScanSignature{}, &os.PathError{Op: "GetFileInformationByHandleEx", Path: path, Err: err}
	}
	if basic.changeTime == 0 {
		// FAT volumes keep no change time.
		return pollScanSignature{}, errPollScanIneligible
	}
	return pollScanSignature{
		size:   int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow),
		mtime:  info.LastWriteTime.Nanoseconds(),
		ctime:  basic.changeTime,
		inode:  int64(uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)),
		device: int64(info.VolumeSerialNumber),
	}, nil
}

// readPollScanDirectory lists through Root.Open, which shares delete access on
// Windows, so agents can rename a directory while the scan lists it. The root
// handle lacks that sharing and closes at once.
func readPollScanDirectory(path string) ([]fs.DirEntry, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	dir, err := root.Open(".")
	if closeErr := root.Close(); err == nil && closeErr != nil {
		_ = dir.Close()
		return nil, closeErr
	}
	if err != nil {
		return nil, err
	}
	defer dir.Close() // Read-only directory handle.
	return dir.ReadDir(-1)
}

// pollScanLongPath keeps os.Open's long-path support for direct CreateFile
// calls by using the extended-length form of long absolute drive and UNC paths.
func pollScanLongPath(path string) string {
	if len(path) < windows.MAX_PATH-12 || !filepath.IsAbs(path) ||
		strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) {
		return path
	}
	path = filepath.Clean(path)
	if unc, ok := strings.CutPrefix(path, `\\`); ok {
		return `\\?\UNC\` + unc
	}
	return `\\?\` + path
}
