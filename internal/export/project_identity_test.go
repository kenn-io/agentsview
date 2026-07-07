package export

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeRemote(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{
			name: "scp github remote strips git suffix",
			raw:  "git@github.com:Org/Repo.git",
			want: "github.com/Org/Repo",
			ok:   true,
		},
		{
			name: "https github remote strips git suffix and trailing slash",
			raw:  "https://github.com/org/repo.git/",
			want: "github.com/org/repo",
			ok:   true,
		},
		{
			name: "ssh URL lowercases host",
			raw:  "ssh://git@GitHub.com/org/repo",
			want: "github.com/org/repo",
			ok:   true,
		},
		{
			name: "duplicate slashes collapsed",
			raw:  "https://github.com/org//repo.git",
			want: "github.com/org/repo",
			ok:   true,
		},
		{
			name: "host lowercased path case preserved",
			raw:  "https://GitHub.com/Org/Repo.git",
			want: "github.com/Org/Repo",
			ok:   true,
		},
		{
			name: "file remote ignored",
			raw:  "file:///tmp/repo.git",
			ok:   false,
		},
		{
			name: "local path remote ignored",
			raw:  "/srv/git/repo.git",
			ok:   false,
		},
		{
			name: "windows backslash drive path ignored",
			raw:  `C:\repo.git`,
			ok:   false,
		},
		{
			name: "windows slash drive path ignored",
			raw:  "C:/repo.git",
			ok:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := NormalizeGitRemote(tt.raw)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSanitizeGitRemoteForStorageStripsUserInfo(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "https token userinfo",
			raw:  "https://" + "user:token@" + "Example.com/org/repo.git",
			want: "https://Example.com/org/repo.git",
		},
		{
			name: "ssh url userinfo",
			raw:  "ssh://git@github.com/org/repo.git",
			want: "ssh://github.com/org/repo.git",
		},
		{
			name: "scp user prefix",
			raw:  "git@github.com:Org/Repo.git",
			want: "github.com:Org/Repo.git",
		},
		{
			name: "scp token-shaped userinfo",
			raw:  "user:token@" + "example.com:Org/Repo.git",
			want: "example.com:Org/Repo.git",
		},
		{
			name: "no userinfo",
			raw:  "https://github.com/org/repo.git",
			want: "https://github.com/org/repo.git",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SanitizeGitRemoteForStorage(tt.raw))
		})
	}
}

func TestProjectIdentitySelectRemotePrefersOriginOtherwiseAlphabetical(t *testing.T) {
	name, raw, ok := SelectRemote(map[string]string{
		"upstream": "https://github.com/acme/upstream.git",
		"origin":   "git@github.com:acme/app.git",
	})
	require.True(t, ok)
	assert.Equal(t, "origin", name)
	assert.Equal(t, "git@github.com:acme/app.git", raw)

	name, raw, ok = SelectRemote(map[string]string{
		"zeta": "https://github.com/acme/zeta.git",
		"beta": "https://github.com/acme/beta.git",
	})
	require.True(t, ok)
	assert.Equal(t, "beta", name)
	assert.Equal(t, "https://github.com/acme/beta.git", raw)
}

func TestProjectIdentityRootPathFallbackNormalizesLocalPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink path separators in this contract are POSIX-specific")
	}
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	linkRoot := filepath.Join(base, "link")
	require.NoError(t, mkdirAll(realRoot))
	require.NoError(t, symlink(realRoot, linkRoot))

	got, ok, err := NormalizeRootPath(linkRoot + string(filepath.Separator))
	require.NoError(t, err)
	require.True(t, ok)
	expectedRoot, ok, err := NormalizeRootPath(realRoot)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, expectedRoot, got)

	identity := BuildProjectIdentity(ProjectIdentityInput{RootPath: linkRoot + "/"})
	require.NotEmpty(t, identity.Key)
	assert.Equal(t, "root_path", identity.KeySource)
	assert.Equal(t, expectedRoot, identity.RootPath)
}

func TestProjectIdentityStoredRootPathMatchesLiveSymlinkNormalization(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	linkRoot := filepath.Join(base, "link")
	require.NoError(t, mkdirAll(realRoot))
	if err := symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	live := BuildProjectIdentity(ProjectIdentityInput{RootPath: linkRoot})
	stored := BuildStoredProjectIdentity(ProjectIdentityInput{RootPath: linkRoot})

	require.NotEmpty(t, live.Key)
	require.NotEmpty(t, stored.Key)
	assert.Equal(t, live.Key, stored.Key)
	assert.Equal(t, live.RootPath, stored.RootPath)
}

func TestProjectIdentityWindowsDriveRootPathsAreLocal(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "backslash drive path",
			raw:  `C:\repo\`,
			want: "C:/repo",
		},
		{
			name: "slash drive path",
			raw:  "C:/repo/",
			want: "C:/repo",
		},
		{
			name: "parent segments stay within drive root",
			raw:  "C:/../repo",
			want: "C:/repo",
		},
		{
			name: "lowercase drive uppercased",
			raw:  "c:/repo",
			want: "C:/repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := NormalizeRootPath(tt.raw)
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, tt.want, got)

			stored, ok := NormalizeStoredRootPath(tt.raw)
			require.True(t, ok)
			assert.Equal(t, tt.want, stored)

			identity := BuildStoredProjectIdentity(ProjectIdentityInput{RootPath: tt.raw})
			require.NotEmpty(t, identity.Key)
			assert.Equal(t, ProjectIdentityKeySourceRootPath, identity.KeySource)
			assert.Equal(t, tt.want, identity.RootPath)
			assert.True(t, identity.MachineLocal)
		})
	}

	_, ok, err := NormalizeRootPath("host:/srv/repo")
	require.NoError(t, err)
	assert.False(t, ok)

	_, ok = NormalizeStoredRootPath("host:/srv/repo")
	assert.False(t, ok)
}

func TestProjectIdentityStoredRootPathAcceptsPOSIXAbsolutePath(t *testing.T) {
	got, ok := NormalizeStoredRootPath(`/fixtures\repo/../repo/worktree/`)
	require.True(t, ok)
	assert.Equal(t, "/fixtures/repo/worktree", got)

	identity := BuildStoredProjectIdentity(ProjectIdentityInput{
		RootPath: `/fixtures\repo/../repo/worktree/`,
	})
	require.NotEmpty(t, identity.Key)
	assert.Equal(t, ProjectIdentityKeySourceRootPath, identity.KeySource)
	assert.Equal(t, "/fixtures/repo/worktree", identity.RootPath)
}

func TestProjectIdentityKeysUseTypedSHA256Inputs(t *testing.T) {
	remoteIdentity := BuildProjectIdentity(ProjectIdentityInput{
		GitRemote: "git@github.com:Org/Repo.git",
	})
	assert.Equal(t,
		"sha256:"+sha256Hex("git_remote\n"+"github.com/Org/Repo"),
		remoteIdentity.Key,
	)
	assert.Equal(t, "git_remote", remoteIdentity.KeySource)

	root, ok, err := NormalizeRootPath(t.TempDir())
	require.NoError(t, err)
	require.True(t, ok)

	rootIdentity := BuildProjectIdentity(ProjectIdentityInput{RootPath: root})
	assert.Equal(t, "sha256:"+sha256Hex("root_path\n"+root), rootIdentity.Key)
	assert.Equal(t, "root_path", rootIdentity.KeySource)
}

func TestProjectIdentityJSONUsesNormalizedRemoteField(t *testing.T) {
	identity := BuildProjectIdentity(ProjectIdentityInput{
		GitRemote: "git@github.com:Org/Repo.git",
	})

	data, err := json.Marshal(identity)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, "github.com/Org/Repo", got["normalized_remote"])
	assert.NotContains(t, got, "git_remote")
}

func TestProjectIdentityBuildProjectsMapResolvesUnknownAndAmbiguous(t *testing.T) {
	observations := []ProjectIdentityObservation{
		{
			Project:   "app",
			Machine:   "laptop",
			RootPath:  "/tmp/ignored",
			GitRemote: "git@github.com:Org/Repo.git",
			Key:       "stale-derived-value",
		},
		{
			Project:   "ambiguous",
			Machine:   "laptop",
			GitRemote: "https://github.com/org/one.git",
		},
		{
			Project:   "ambiguous",
			Machine:   "desktop",
			GitRemote: "https://github.com/org/two.git",
		},
	}

	got := BuildProjectsMap([]string{"app", "missing", "ambiguous"}, observations)

	require.Equal(t, ProjectResolutionResolved, got["app"].Resolution)
	require.NotNil(t, got["app"].Identity)
	assert.Equal(t, "github.com/Org/Repo", got["app"].Identity.NormalizedRemote)
	assert.Equal(t, "sha256:"+sha256Hex("git_remote\n"+"github.com/Org/Repo"), got["app"].Identity.Key)

	assert.Equal(t, ProjectResolutionUnknown, got["missing"].Resolution)
	assert.Nil(t, got["missing"].Identity)

	assert.Equal(t, ProjectResolutionAmbiguous, got["ambiguous"].Resolution)
	assert.Nil(t, got["ambiguous"].Identity)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestIsAutomountNamespacePath(t *testing.T) {
	tests := []struct {
		name string
		goos string
		path string
		want bool
	}{
		{"darwin home root", "darwin", "/home", true},
		{"darwin home child", "darwin", "/home/user/repo", true},
		{"darwin net child", "darwin", "/net/host/share", true},
		{"darwin network servers", "darwin", "/Network/Servers/x", true},
		{"darwin prefix collision homework", "darwin", "/homework/repo", false},
		{"darwin prefix collision netdata", "darwin", "/netdata", false},
		{"darwin regular path", "darwin", "/Users/user/repo", false},
		{"linux home is real", "linux", "/home/user/repo", false},
		{"windows never matches", "windows", "/home/user", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsAutomountNamespacePath(tt.goos, tt.path))
		})
	}
}

// TestNormalizeStoredRootPathSkipsAutomountNamespace pins that on macOS a
// stored /home/... root path normalizes to its cleaned form without touching
// the filesystem: resolving it through the automounter is both futile (the
// path names a directory on another machine) and expensive (each probe wakes
// automountd/opendirectoryd, and negative results are not cached).
func TestNormalizeStoredRootPathSkipsAutomountNamespace(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("automount namespaces are a darwin-only concern")
	}
	got, ok := NormalizeStoredRootPath("/home/user/work/repo")
	require.True(t, ok)
	assert.Equal(t, "/home/user/work/repo", got)

	normalized, ok, err := NormalizeRootPath("/home/user/work/repo")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "/home/user/work/repo", normalized)
}
