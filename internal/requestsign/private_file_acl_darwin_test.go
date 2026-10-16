//go:build darwin && cgo

package requestsign

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadSecretChecksDarwinACL(t *testing.T) {
	current, err := user.Current()
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		acl     string
		private bool
	}{
		{name: "mode only", private: true},
		{name: "deny entry", acl: "everyone deny execute", private: true},
		{name: "current user", acl: "user:" + current.Username + " allow read", private: true},
		{name: "root", acl: "user:root allow read", private: true},
		{name: "other readers", acl: "everyone allow read"},
		{name: "other writers", acl: "everyone allow write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			secret := bytes.Repeat([]byte("k"), 64)
			require.NoError(t, os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(secret)), 0o600))
			if tc.acl != "" {
				addSigningTestACL(t, path, tc.acl)
			}
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

			got, err := ReadSecret(path)

			if tc.private {
				require.NoError(t, err)
				assert.Equal(t, secret, got)
			} else {
				require.ErrorContains(t, err, "private regular files")
				assert.Nil(t, got)
			}
		})
	}
}

func TestGenerateSecretFileChecksInheritedDarwinACL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		acl     string
		private bool
	}{
		{name: "mode only", private: true},
		{name: "inherited deny", acl: "everyone deny execute,file_inherit,only_inherit", private: true},
		{name: "inherited readers", acl: "everyone allow read,file_inherit,only_inherit"},
		{name: "inherited writers", acl: "everyone allow write,file_inherit,only_inherit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.acl != "" {
				addSigningTestACL(t, dir, tc.acl)
			}
			path := filepath.Join(dir, "key")

			err := GenerateSecretFile(path)

			if tc.private {
				require.NoError(t, err)
				secret, err := ReadSecret(path)
				require.NoError(t, err)
				assert.Len(t, secret, 64)
			} else {
				require.ErrorContains(t, err, "private regular files")
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Empty(t, data, "rejected inherited ACL must never receive key bytes")
			}
		})
	}
}

func addSigningTestACL(t *testing.T, path, acl string) {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), "/bin/chmod", "+a", acl, path).CombinedOutput()
	require.NoError(t, err, "%s", output)
}
