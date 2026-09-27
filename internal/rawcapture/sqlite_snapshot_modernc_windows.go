package rawcapture

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

func sqliteSnapshotModerncConnectionIdentity(
	tls *libc.TLS, database, name uintptr,
) (string, error) {
	argumentSize := int(unsafe.Sizeof(uintptr(0)))
	argument := tls.Alloc(argumentSize)
	defer tls.Free(argumentSize)
	*(*uintptr)(unsafe.Pointer(argument)) = 0
	result := sqlite3.Xsqlite3_file_control(
		tls, database, name,
		int32(sqlite3.SQLITE_FCNTL_WIN32_GET_HANDLE), argument,
	)
	if result != sqlite3.SQLITE_OK {
		return "", fmt.Errorf("rawcapture: inspect SQLite source identity: result %d", result)
	}
	file := windows.Handle(*(*uintptr)(unsafe.Pointer(argument)))
	if file == 0 || file == windows.InvalidHandle {
		return "", fmt.Errorf("rawcapture: SQLite source has no open file")
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(file, &info); err != nil {
		return "", fmt.Errorf("rawcapture: inspect SQLite source identity: %w", err)
	}
	fileIndex := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return fmt.Sprintf("%d:%d", uint64(info.VolumeSerialNumber), fileIndex), nil
}
