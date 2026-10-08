package git

import (
	"os"

	"golang.org/x/sys/windows"
)

func repoRootAccessible(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func repoRootOwned(path string) bool {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return false
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && windows.EqualSid(owner, user.User.Sid)
}

// Git for Windows reports zero for st_dev during repository discovery.
func repoRootDevice(_ string) (uint64, error) {
	return 0, nil
}
