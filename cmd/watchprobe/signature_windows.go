package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

type fileBasicInfo struct {
	Creation, Access, Write, Change int64
	Attributes                      uint32
	Padding                         uint32
}
type fileStandardInfo struct {
	Allocation, Size  int64
	Links             uint32
	Delete, Directory byte
	Padding           [2]byte
}
type fileIDInfo struct {
	Volume   uint64
	Identity [16]byte
}

func freshSignature(path string) (signature, error) {
	text, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return signature{}, err
	}
	handle, err := windows.CreateFile(text, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return signature{}, err
	}
	defer windows.CloseHandle(handle)
	var basic fileBasicInfo
	var standard fileStandardInfo
	var id fileIDInfo
	for _, q := range []struct {
		kind uint32
		ptr  unsafe.Pointer
		size uintptr
	}{{windows.FileBasicInfo, unsafe.Pointer(&basic), unsafe.Sizeof(basic)}, {windows.FileStandardInfo, unsafe.Pointer(&standard), unsafe.Sizeof(standard)}, {windows.FileIdInfo, unsafe.Pointer(&id), unsafe.Sizeof(id)}} {
		if err = windows.GetFileInformationByHandleEx(handle, q.kind, (*byte)(q.ptr), uint32(q.size)); err != nil {
			return signature{}, err
		}
	}
	const epoch = 116444736000000000
	return signature{Size: standard.Size, MtimeNS: (basic.Write - epoch) * 100, ChangeNS: (basic.Change - epoch) * 100, Volume: id.Volume, Identity: id.Identity, IdentityKnown: true, ChangeKnown: basic.Change != 0}, nil
}
