//go:build windows

package requestsign

import (
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestReadSecretRejectsWorldReadableACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	secret := signingKey(t).Secret
	file, err := createPrivateFile(path)
	require.NoError(t, err)
	require.NoError(t, ensurePrivateFile(file))
	_, err = file.WriteString(base64.StdEncoding.EncodeToString(secret))
	require.NoError(t, err)
	require.NoError(t, setSigningFileACLForTest(t, file, "D:P(A;;FA;;;WD)"))
	require.NoError(t, file.Close())
	_, err = ReadSecret(path)
	require.Error(t, err)
}

func TestEnsurePrivateFileReplacesInheritedACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.db")
	file, err := createPrivateFile(path)
	require.NoError(t, err)
	require.NoError(t, setSigningFileACLForTest(t, file, "D:P(A;;FA;;;WD)"))
	require.NoError(t, ensurePrivateFile(file))
	assert.True(t, isPrivateRegularFile(file))
	assert.NoError(t, file.Close())
}

func setSigningFileACLForTest(t *testing.T, file *os.File, sddl string) error {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if dacl == nil {
		return errors.New("test security descriptor has no DACL")
	}
	return windows.SetSecurityInfo(
		windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	)
}

func TestCreatePrivateFileRejectsInheritedReadersBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	require.NoError(t, err)
	dacl, _, err := sd.DACL()
	require.NoError(t, err)
	require.NoError(t, windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil))
	runtime.KeepAlive(sd)

	var token windows.Token
	require.NoError(t, windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE, &token))
	defer token.Close()
	var impersonation windows.Token
	require.NoError(t, windows.DuplicateTokenEx(token,
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_IMPERSONATE,
		nil, windows.SecurityImpersonation, windows.TokenImpersonation, &impersonation))
	defer impersonation.Close()
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, err)
	restrictedSID := windows.SIDAndAttributes{Sid: world}
	var restricted windows.Token
	// The second access check requires an Everyone ACE, even for an administrator.
	createRestrictedToken := windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")
	const disableMaxPrivilege = 1
	ok, _, callErr := createRestrictedToken.Call(uintptr(impersonation), disableMaxPrivilege,
		0, 0, 0, 0, 1, uintptr(unsafe.Pointer(&restrictedSID)), uintptr(unsafe.Pointer(&restricted)))
	runtime.KeepAlive(restrictedSID)
	require.NotZero(t, ok, "CreateRestrictedToken: %v", callErr)
	defer restricted.Close()

	openAsInheritedReader := func(path string) (*os.File, error) {
		pathPtr, err := windows.UTF16PtrFromString(path)
		require.NoError(t, err)
		runtime.LockOSThread()
		require.NoError(t, windows.SetThreadToken(nil, restricted))
		handle, openErr := windows.CreateFile(pathPtr, windows.GENERIC_READ,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		// A failed revert must not return an impersonating thread to Go's pool.
		require.NoError(t, windows.RevertToSelf())
		runtime.UnlockOSThread()
		if openErr != nil {
			return nil, openErr
		}
		return os.NewFile(uintptr(handle), path), nil
	}

	// An inherited reader keeps access even after the owner restricts the ACL.
	publicPath := filepath.Join(dir, "public")
	publicPathPtr, err := windows.UTF16PtrFromString(publicPath)
	require.NoError(t, err)
	publicHandle, err := windows.CreateFile(publicPathPtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	require.NoError(t, err)
	inherited := os.NewFile(uintptr(publicHandle), publicPath)
	require.NotNil(t, inherited)
	defer inherited.Close()
	public, err := openAsInheritedReader(publicPath)
	require.NoError(t, err, "the reader must be able to open inherited files")
	defer public.Close()
	require.NoError(t, ensurePrivateFile(inherited))
	_, err = inherited.WriteString("readable fixture")
	require.NoError(t, err)
	publicData, err := io.ReadAll(public)
	require.NoError(t, err)
	assert.Equal(t, "readable fixture", string(publicData),
		"restricting an ACL does not revoke the reader's existing handle")

	path := filepath.Join(dir, "secret")
	file, err := createPrivateFile(path)
	require.NoError(t, err)
	defer file.Close()
	reader, err := openAsInheritedReader(path)
	if reader != nil {
		defer reader.Close()
	}
	require.ErrorIs(t, err, windows.ERROR_ACCESS_DENIED,
		"creation must deny inherited readers before ensurePrivateFile or any writes")
	require.NoError(t, ensurePrivateFile(file))
	_, err = file.WriteString("private fixture")
	require.NoError(t, err)
	data, err := os.ReadFile(path)
	require.NoError(t, err, "the owner must retain read and write access")
	assert.Equal(t, "private fixture", string(data))
}
