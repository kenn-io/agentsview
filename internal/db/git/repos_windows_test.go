package git

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestFindRepoRoot_WindowsOwnership(t *testing.T) {
	skipIfNoGit(t)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	require.NoError(t, err)
	member, err := windows.Token(0).IsMember(administrators)
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		owner *windows.SID
	}{
		{"current user", user.User.Sid},
		{"administrators", administrators},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "administrators" && !member {
				t.Skip("setting Administrators ownership requires an elevated administrator")
			}
			repo := initBareRepo(t)
			for _, path := range []string{repo, filepath.Join(repo, ".git")} {
				require.NoError(t, windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
					windows.OWNER_SECURITY_INFORMATION, tc.owner, nil, nil, nil))
				descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
				require.NoError(t, err)
				owner, _, err := descriptor.Owner()
				require.NoError(t, err)
				require.NotNil(t, owner)
				require.True(t, windows.EqualSid(tc.owner, owner), "fixture owner must match the test case")
			}
			want := canonAll([]string{repo})[0]
			require.Equal(t, want, findRepoRoot(t.Context(), repo))
			t.Setenv("PATH", t.TempDir())
			assert.Equal(t, want, findRepoRoot(t.Context(), mkdirIn(t, repo, "unseen")),
				"Git-owned repositories must reuse the cached root without another Git process")
		})
	}
}
