//go:build windows

package requestsign

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func createPrivateFile(path string) (*os.File, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("creating signing file handle failed")
	}
	return file, nil
}

func isPrivateRegularFile(file *os.File) bool {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	trusted, err := trustedSigningSIDs()
	if err != nil {
		return false
	}
	sd, err := windows.GetSecurityInfo(
		windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || !trustedSID(owner, trusted) {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false
	}
	var trustedAllow bool
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err = windows.GetAce(dacl, i, &ace); err != nil || ace == nil {
			return false
		}
		header := (*windows.ACE_HEADER)(unsafe.Pointer(ace))
		const sidOffset = int(unsafe.Offsetof(windows.ACCESS_ALLOWED_ACE{}.SidStart))
		if int(header.AceSize) < sidOffset+8 {
			return false
		}
		if header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE && header.AceType != windows.ACCESS_DENIED_ACE_TYPE {
			return false
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || sid.Len() > int(header.AceSize)-sidOffset {
			return false
		}
		if header.AceType == windows.ACCESS_ALLOWED_ACE_TYPE {
			if !trustedSID(sid, trusted) {
				return false
			}
			trustedAllow = true
		}
	}
	return trustedAllow
}

func ensurePrivateFile(file *os.File) error {
	if file == nil {
		return errors.New("file is not private")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user.User.Sid == nil {
		return errors.New("reading signing file ACL failed")
	}
	userSID := user.User.Sid.String()
	if userSID == "" {
		return errors.New("reading signing file ACL failed")
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + userSID + ")")
	if err != nil {
		return errors.New("creating signing file ACL failed")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return errors.New("creating signing file ACL failed")
	}
	if err = windows.SetSecurityInfo(
		windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	); err != nil {
		return errors.New("restricting signing file ACL failed")
	}
	if !isPrivateRegularFile(file) {
		return errors.New("signing file ACL is not private")
	}
	return nil
}

func trustedSigningSIDs() ([]*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user.User.Sid == nil {
		return nil, errors.New("reading signing file ACL failed")
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	return []*windows.SID{user.User.Sid, system, admins}, nil
}

func trustedSID(sid *windows.SID, trusted []*windows.SID) bool {
	if sid == nil || !sid.IsValid() {
		return false
	}
	for _, allowed := range trusted {
		if windows.EqualSid(sid, allowed) {
			return true
		}
	}
	return false
}
