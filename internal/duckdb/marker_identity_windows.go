//go:build windows

package duckdb

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// fileIdentityForPath returns the filesystem identity (volume serial number
// plus 64-bit file index) of the file at path. The handle is opened with
// FILE_READ_ATTRIBUTES and full sharing so the call succeeds even while a
// live DuckDB process holds the file open: the DuckDB lock protects the
// file's content, not its attributes, which is the same property the
// probe's own stat and PrimeFileIdentity's os.SameFile self-compare (which
// loads Windows file IDs on locked files) already rely on.
func fileIdentityForPath(path string) (fileIdentity, error) {
	namePtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fileIdentity{}, fmt.Errorf(
			"encoding %s for file identity: %w", path, err,
		)
	}
	handle, err := windows.CreateFile(
		namePtr,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return fileIdentity{}, fmt.Errorf(
			"opening %s for file identity: %w", path, err,
		)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fileIdentity{}, fmt.Errorf(
			"reading file identity of %s: %w", path, err,
		)
	}
	return fileIdentity{
		A: uint64(info.VolumeSerialNumber),
		B: uint64(info.FileIndexHigh),
		C: uint64(info.FileIndexLow),
	}, nil
}
