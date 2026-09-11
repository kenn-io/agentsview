package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestInstallationIdentityDuringRenameHandle(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		name := "ensure"
		if readOnly {
			name = "read"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			const id = "0123456789abcdef0123456789abcdef"
			temporary := filepath.Join(dir, ".installation-proof")
			require.NoError(t, os.WriteFile(temporary, []byte(id+"\n"), 0o600))
			path16, err := windows.UTF16PtrFromString(temporary)
			require.NoError(t, err)
			// A rename publishes the name before closing its DELETE-access handle.
			handle, err := windows.CreateFile(path16, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, windows.CloseHandle(handle)) })
			path := filepath.Join(dir, installationIDFilename)
			require.NoError(t, os.Rename(temporary, path))
			_, err = os.ReadFile(path)
			require.ErrorIs(t, err, windows.ERROR_SHARING_VIOLATION)
			cfg := Config{DataDir: dir}
			if readOnly {
				err = cfg.readInstallationID()
			} else {
				err = cfg.ensureInstallationID()
			}
			require.NoError(t, err)
			require.Equal(t, id, cfg.InstallationID)
			_, err = os.Stat(filepath.Join(dir, configFileName+".lock"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestInstallationIdentityLongPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("a", 100), strings.Repeat("b", 100), strings.Repeat("c", 100))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	const id = "0123456789abcdef0123456789abcdef"
	path := filepath.Join(dir, installationIDFilename)
	require.NoError(t, os.WriteFile(path, []byte(id+"\n"), 0o600))
	cfg := Config{DataDir: dir}
	require.NoError(t, cfg.ensureInstallationID())
	require.Equal(t, id, cfg.InstallationID)
}
