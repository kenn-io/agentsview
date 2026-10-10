//go:build windows

package requestsign

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

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
