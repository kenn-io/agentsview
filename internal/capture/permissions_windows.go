//go:build windows

package capture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// trustedInstallerSIDString is the fixed service SID for Windows Modules
// Installer, which owns system-managed volume roots on supported Windows hosts.
const trustedInstallerSIDString = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"

func createSecureCaptureDirectory(path string) error {
	allowed, err := captureDirectorySIDs()
	if err != nil {
		return err
	}
	descriptor, err := captureDirectorySecurityDescriptor(allowed)
	if err != nil {
		return err
	}
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	if err := windows.CreateDirectory(pathUTF16, &attributes); err != nil {
		return err
	}
	runtime.KeepAlive(descriptor)
	return verifyCaptureDirectoryDACL(path, allowed)
}

func captureDirectorySecurityDescriptor(
	allowed []*windows.SID,
) (*windows.SECURITY_DESCRIPTOR, error) {
	if len(allowed) == 0 {
		return nil, errors.New("capture directory has no trusted principals")
	}
	var sddl strings.Builder
	sddl.WriteString("O:")
	sddl.WriteString(allowed[0].String())
	sddl.WriteString("D:P")
	for _, sid := range allowed {
		sddl.WriteString("(A;OICI;GA;;;")
		sddl.WriteString(sid.String())
		sddl.WriteByte(')')
	}
	return windows.SecurityDescriptorFromString(sddl.String())
}

func verifyCaptureParentSafety(path string) error {
	allowed, err := captureDirectorySIDs()
	if err != nil {
		return err
	}
	for parent := filepath.Dir(filepath.Clean(path)); ; parent = filepath.Dir(parent) {
		if err := verifyWindowsParentDACL(parent, allowed); err != nil {
			return fmt.Errorf("unsafe capture parent %q: %w", parent, err)
		}
		if parent == filepath.Dir(parent) {
			return nil
		}
	}
}

func verifyWindowsParentDACL(path string, allowed []*windows.SID) error {
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	return verifyWindowsParentSecurity(descriptor, allowed)
}

func verifyWindowsParentSecurity(
	descriptor *windows.SECURITY_DESCRIPTOR,
	allowed []*windows.SID,
) error {
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	trustedInstaller, err := windows.StringToSid(trustedInstallerSIDString)
	if err != nil {
		return fmt.Errorf("resolving trusted Windows owner: %w", err)
	}
	if owner == nil || (!captureSIDAllowed(owner, allowed) &&
		!windows.EqualSid(owner, trustedInstaller)) {
		return errors.New("parent directory has an untrusted owner")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return errors.New("parent directory has an unrestricted DACL")
	}
	for index := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		// The capture directory is created atomically with a protected DACL,
		// so inheritable create/write permissions never apply to it. Validate
		// only rights that can replace an existing ancestor. Every descendant
		// parent is inspected separately with its effective DACL.
		directReplacement := ace.Header.AceFlags&windows.INHERIT_ONLY_ACE == 0 &&
			ace.Mask&captureParentReplacementRights() != 0
		if !directReplacement ||
			ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("parent DACL has an unsupported replacement-capable ACE")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !captureSIDAllowed(sid, allowed) {
			return errors.New("parent DACL permits replacement by another principal")
		}
	}
	return nil
}

func captureParentReplacementRights() windows.ACCESS_MASK {
	const fileDeleteChild windows.ACCESS_MASK = 0x40
	return windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
		windows.GENERIC_ALL | fileDeleteChild
}

func captureSIDAllowed(sid *windows.SID, allowed []*windows.SID) bool {
	for _, candidate := range allowed {
		if windows.EqualSid(sid, candidate) {
			return true
		}
	}
	return false
}

func secureCaptureDirectory(path string) error {
	if err := verifyCaptureDirectoryOwner(path); err != nil {
		return err
	}
	allowed, err := captureDirectorySIDs()
	if err != nil {
		return err
	}
	return applyCaptureDirectoryDACL(path, allowed)
}

func verifyCaptureDirectoryOwner(path string) error {
	if err := verifyCapturePathOwner(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("capture path is not a private directory")
	}
	return nil
}

func verifyCapturePathOwner(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("capture state contains a symbolic link")
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	allowed, err := captureDirectorySIDs()
	if err != nil {
		return err
	}
	if owner == nil || !captureSIDAllowed(owner, allowed) {
		return errors.New("capture state is not owned by a trusted principal")
	}
	return nil
}

func applyCaptureDirectoryDACL(path string, allowed []*windows.SID) error {
	var pinner runtime.Pinner
	defer pinner.Unpin()
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(allowed))
	for _, sid := range allowed {
		pinner.Pin(sid)
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return err
	}
	return verifyCaptureDirectoryDACL(path, allowed)
}

func captureDirectorySIDs() ([]*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	var unique []*windows.SID
	for _, sid := range []*windows.SID{user.User.Sid, system, administrators} {
		duplicate := false
		for _, existing := range unique {
			if windows.EqualSid(existing, sid) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			unique = append(unique, sid)
		}
	}
	return unique, nil
}

func verifyCaptureDirectoryDACL(path string, allowed []*windows.SID) error {
	descriptor, err := windows.GetNamedSecurityInfo(
		path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return verifyCaptureDirectorySecurity(descriptor, allowed)
}

func verifyExistingClaudeProviderRoot(path string) error {
	if err := verifyCaptureDirectoryOwner(path); err != nil {
		return err
	}
	allowed, err := captureDirectorySIDs()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return verifyClaudeProviderRootSecurity(descriptor, allowed)
}

func verifyClaudeProviderRootSecurity(
	descriptor *windows.SECURITY_DESCRIPTOR, allowed []*windows.SID,
) error {
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return errors.New("Claude provider root has an unrestricted DACL")
	}
	for index := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace.Mask&claudeProviderSensitiveRights() == 0 ||
			ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New(
				"Claude provider root has an unsupported sensitive ACE",
			)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !captureSIDAllowed(sid, allowed) {
			return errors.New(
				"Claude provider root grants sensitive access to another principal",
			)
		}
	}
	return nil
}

func claudeProviderSensitiveRights() windows.ACCESS_MASK {
	const fileDeleteChild windows.ACCESS_MASK = 0x40
	return windows.GENERIC_ALL | windows.GENERIC_READ | windows.GENERIC_WRITE |
		windows.FILE_READ_DATA | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
		windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | fileDeleteChild
}

func verifyCaptureDirectorySecurity(
	descriptor *windows.SECURITY_DESCRIPTOR, allowed []*windows.SID,
) error {
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("capture directory DACL is not protected")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return errors.New("capture directory has an unrestricted DACL")
	}
	seen := make([]bool, len(allowed))
	for index := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			return errors.New("capture directory DACL has an unsafe entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		matched := false
		for allowedIndex, candidate := range allowed {
			if windows.EqualSid(sid, candidate) {
				inheritance := uint8(windows.OBJECT_INHERIT_ACE |
					windows.CONTAINER_INHERIT_ACE)
				if captureDirectoryHasFullAccess(ace.Mask) &&
					ace.Header.AceFlags&inheritance == inheritance {
					seen[allowedIndex] = true
				}
				matched = true
				break
			}
		}
		if !matched {
			return errors.New("capture directory DACL grants an unexpected principal")
		}
	}
	for _, present := range seen {
		if !present {
			return errors.New(
				"capture directory DACL omits inheritable full access for a trusted principal",
			)
		}
	}
	return nil
}

func captureDirectoryHasFullAccess(mask windows.ACCESS_MASK) bool {
	if mask&windows.GENERIC_ALL != 0 {
		return true
	}
	// Windows maps GENERIC_ALL to FILE_ALL_ACCESS when it applies a
	// security descriptor to a directory. x/sys does not export that mask.
	const fileAllSpecificRights windows.ACCESS_MASK = 0x1ff
	full := windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE |
		fileAllSpecificRights
	return mask&full == full
}
