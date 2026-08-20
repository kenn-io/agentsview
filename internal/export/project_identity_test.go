package export

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
			name: "default ssh port omitted",
			raw:  "ssh://git@example.com:22/acme/app.git",
			want: "example.com/acme/app",
			ok:   true,
		},
		{
			name: "non-default ssh port retained",
			raw:  "ssh://git@example.com:2222/acme/app.git",
			want: "example.com:2222/acme/app",
			ok:   true,
		},
		{
			name: "non-default https port retained",
			raw:  "https://example.com:8443/acme/app.git",
			want: "example.com:8443/acme/app",
			ok:   true,
		},
		{
			name: "ipv6 host uses canonical brackets",
			raw:  "ssh://git@[2001:0db8::1]/acme/app.git",
			want: "[2001:db8::1]/acme/app",
			ok:   true,
		},
		{
			name: "ipv4 host remains unbracketed",
			raw:  "https://192.0.2.1/acme/app.git",
			want: "192.0.2.1/acme/app",
			ok:   true,
		},
		{
			name: "hostname trailing dot omitted",
			raw:  "https://GitHub.com./acme/app.git",
			want: "github.com/acme/app",
			ok:   true,
		},
		{
			name: "escaped separators and dot segments normalized",
			raw:  "https://example.com/acme%2Ftmp%2F..%2Fapp.git",
			want: "example.com/acme/app",
			ok:   true,
		},
		{
			name: "scp query and fragment omitted",
			raw:  "git@example.com:acme/app.git?token=secret#ref",
			want: "example.com/acme/app",
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
			name: "scp query credential",
			raw:  "git@example.com:Org/Repo.git?token=secret",
			want: "example.com:Org/Repo.git",
		},
		{
			name: "scp fragment credential",
			raw:  "git@example.com:Org/Repo.git#token=secret",
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

func TestSafeProjectDisplayLabelRejectsURLSchemes(t *testing.T) {
	tests := []string{
		"mailto:alice@example.com",
		"urn:example:private-project",
		"s3:private-bucket/project",
		"custom+ssh:credential-bearing-value",
	}

	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			assert.Empty(t, SafeProjectDisplayLabel(value))
		})
	}
	assert.Equal(t, "release notes", SafeProjectDisplayLabel("release notes"))
	assert.Equal(t, "Fix: project identity",
		SafeProjectDisplayLabel("Fix: project identity"))
}

func TestProjectIdentitySelectRemotePrefersOriginAndRejectsAmbiguity(t *testing.T) {
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
	assert.False(t, ok)
	assert.Empty(t, name)
	assert.Empty(t, raw)
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

func TestProjectIdentityStoredRootPathUsesLexicalSymlinkPath(t *testing.T) {
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
	assert.NotEqual(t, live.Key, stored.Key)
	assert.Equal(t, filepath.ToSlash(filepath.Clean(linkRoot)), stored.RootPath)
	expectedLive, ok, err := NormalizeRootPath(realRoot)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, expectedLive, live.RootPath)
}

func TestStoredProjectReferenceDoesNotFollowRetargetedSymlink(t *testing.T) {
	base := t.TempDir()
	firstRoot := filepath.Join(base, "first")
	secondRoot := filepath.Join(base, "second")
	linkRoot := filepath.Join(base, "link")
	require.NoError(t, mkdirAll(firstRoot))
	require.NoError(t, mkdirAll(secondRoot))
	if err := symlink(firstRoot, linkRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	obs := ProjectIdentityObservation{
		Project: "fixture", Machine: "fixture-machine",
		RootPath: linkRoot, RepositoryPath: linkRoot,
		WorktreeRootPath: linkRoot, WorktreeRelationship: WorktreeMain,
	}
	scope := IdentityScope{ArchiveID: "archive", ArchiveSalt: "salt"}
	before := ResolveProjectReferenceFromObservation(obs, scope)

	require.NoError(t, os.Remove(linkRoot))
	require.NoError(t, symlink(secondRoot, linkRoot))
	after := ResolveProjectReferenceFromObservation(obs, scope)

	assert.Equal(t, before.Identity, after.Identity)
	assert.Equal(t, before.Worktree, after.Worktree)
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
	reference := ResolveProjectReference(ProjectIdentityInput{
		RootPath:         "/work/repo",
		WorktreeRootPath: "/work/repo",
		WorktreeKind:     WorktreeMain,
		GitRemote:        "git@github.com:Org/Repo.git",
	}, IdentityScope{ArchiveID: "archive", ArchiveSalt: "salt", MachineID: "machine"})
	require.NotNil(t, reference.Identity)

	data, err := json.Marshal(reference.Identity)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, "github.com/Org/Repo", got["normalized_remote"])
	assert.NotContains(t, got, "git_remote")
	assert.NotContains(t, got, "root_path")
	assert.NotContains(t, got, "key_source")
	assert.NotContains(t, got, "machine_local")
	assert.Equal(t, string(ProjectKindGitRemote), got["kind"])
	assert.Contains(t, got, "root_key")
	assert.Contains(t, got, "repository_key")
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
	assert.Equal(t, ProjectKindGitRemote, got["app"].Identity.Kind)
	assert.NotEmpty(t, got["app"].Identity.Key)

	assert.Equal(t, ProjectResolutionUnknown, got["missing"].Resolution)
	assert.Nil(t, got["missing"].Identity)

	assert.Equal(t, ProjectResolutionAmbiguous, got["ambiguous"].Resolution)
	assert.Nil(t, got["ambiguous"].Identity)
}

func TestProjectIdentityBuildProjectsMapUsesObservationArchiveScope(t *testing.T) {
	got := BuildProjectsMapWithScope([]string{"app"}, []ProjectIdentityObservation{{
		SourceArchiveID:   "archive-a",
		SourceArchiveSalt: "0123456789abcdef",
		Project:           "app",
		Machine:           "laptop",
		RootPath:          "/workspace/app",
		GitRemote:         "https://github.com/acme/app.git",
	}}, IdentityScope{})

	require.NotNil(t, got["app"].Identity)
	assert.Empty(t, got["app"].Identity.RootKey,
		"remote-backed aggregates must not select one machine-local root")
}

func TestProjectMapForWireUsesOpaqueKeysAndSafeLabels(t *testing.T) {
	projects := BuildProjectsMapWithScope(
		[]string{
			"/Users/alice/private/app",
			`\\server\share\private-app`,
			`\rooted\private-app`,
		}, nil,
		IdentityScope{ArchiveID: "archive-a", ArchiveSalt: "salt-a"},
	)
	got := ProjectMapForWire(projects)

	require.Len(t, got, 3)
	for key, entry := range got {
		assert.NotContains(t, key, "/Users/alice")
		assert.Empty(t, entry.DisplayLabel)
	}
}

func TestProjectLabelKeyIsStableOnlyWithinArchive(t *testing.T) {
	build := func(archiveID, salt string) string {
		projects := BuildProjectsMapWithScope(
			[]string{"/Users/alice/private/app"}, nil,
			IdentityScope{ArchiveID: archiveID, ArchiveSalt: salt},
		)
		for key := range ProjectMapForWire(projects) {
			return key
		}
		return ""
	}

	first := build("archive-a", "salt-a")
	assert.Equal(t, first, build("archive-a", "salt-a"))
	assert.NotEqual(t, first, build("archive-b", "salt-b"))
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
		{"darwin data volume home", "darwin", "/System/Volumes/Data/home/user/repo", true},
		{"darwin data volume net", "darwin", "/System/Volumes/Data/net/host", true},
		{"darwin mixed case home", "darwin", "/HOME/user/repo", true},
		{"darwin mixed case network servers", "darwin", "/network/SERVERS/x", true},
		{"darwin mixed case data volume", "darwin", "/system/Volumes/DATA/home/user", true},
		{"darwin data volume users is real", "darwin", "/System/Volumes/Data/Users/user/repo", false},
		{"darwin data volume root is real", "darwin", "/System/Volumes/Data", false},
		{"linux home is real", "linux", "/home/user/repo", false},
		{"linux data volume ignored", "linux", "/System/Volumes/Data/home/user", false},
		{"windows never matches", "windows", "/home/user", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsAutomountNamespacePath(tt.goos, tt.path))
		})
	}
}

func TestIsProtectedUserDataPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the predicate compares POSIX home paths built with filepath")
	}
	const home = "/Users/tester"
	tests := []struct {
		name string
		goos string
		home string
		path string
		want bool
	}{
		{"documents child", "darwin", home, home + "/Documents/proj", true},
		{"documents root", "darwin", home, home + "/Documents", true},
		{"downloads child", "darwin", home, home + "/Downloads/repo", true},
		{"desktop child", "darwin", home, home + "/Desktop/repo", true},
		{"pictures child", "darwin", home, home + "/Pictures/scan", true},
		{"dropbox child", "darwin", home, home + "/Dropbox/code/app", true},
		{
			"cloud storage child", "darwin", home,
			home + "/Library/CloudStorage/Dropbox-Personal/app", true,
		},
		{
			"icloud drive child", "darwin", home,
			home + "/Library/Mobile Documents/com~apple~CloudDocs/app", true,
		},
		{
			"case folded child", "darwin", home,
			home + "/documents/proj", true,
		},
		{
			"data volume documents", "darwin", home,
			"/System/Volumes/Data" + home + "/Documents/proj", true,
		},
		{
			"data volume home form", "darwin",
			"/System/Volumes/Data" + home, home + "/Documents/proj", true,
		},
		{
			"data volume unprotected", "darwin", home,
			"/System/Volumes/Data" + home + "/src/app", false,
		},
		{
			"mixed case data volume documents", "darwin", home,
			"/system/volumes/data" + home + "/Documents/proj", true,
		},
		{"unprotected home child", "darwin", home, home + "/src/app", false},
		{"library sibling", "darwin", home, home + "/Library/Caches/x", false},
		{
			"prefix collision documentation", "darwin", home,
			home + "/Documentation/proj", false,
		},
		{"another user documents", "darwin", home, "/Users/other/Documents", false},
		{"outside home", "darwin", home, "/opt/src/app", false},
		{"relative path", "darwin", home, "Documents/proj", false},
		{"empty home", "darwin", "", home + "/Documents/proj", false},
		{"linux never matches", "linux", home, home + "/Documents/proj", false},
		{"windows never matches", "windows", home, home + "/Documents/proj", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				IsProtectedUserDataPath(tt.goos, tt.home, tt.path))
		})
	}
}

// TestIsCanonicalAutomountNamespacePath pins that the canonical predicate
// accepts only the exact spellings the parser's resolved-autofs probe
// examines: alternate spellings the broad predicate recognizes must not
// inherit that vetting.
func TestIsCanonicalAutomountNamespacePath(t *testing.T) {
	tests := []struct {
		name string
		goos string
		path string
		want bool
	}{
		{"canonical home", "darwin", "/home/user/repo", true},
		{"canonical net", "darwin", "/net/host/share", true},
		{"canonical network servers", "darwin", "/Network/Servers/x", true},
		{"data volume spelling", "darwin", "/System/Volumes/Data/home/user", false},
		{"case folded spelling", "darwin", "/HOME/user", false},
		{"lowercase network servers", "darwin", "/network/servers/x", false},
		{"regular path", "darwin", "/Users/user/repo", false},
		{"linux never matches", "linux", "/home/user/repo", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				IsCanonicalAutomountNamespacePath(tt.goos, tt.path))
		})
	}
}

// TestRegisteredAutomountPrefixes pins that autofs mounts discovered from
// the host's mount table classify as automount alongside the fixed
// namespaces: the broad predicate accepts alternate spellings, the canonical
// predicate only the mount table's exact form, and the classification walk
// refuses a symlink into a custom mount without Lstat-ing inside it.
func TestRegisteredAutomountPrefixes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the predicate compares POSIX home paths built with filepath")
	}
	orig := RegisteredAutomountPrefixes()
	t.Cleanup(func() { RegisterAutomountPrefixes(orig) })
	RegisterAutomountPrefixes([]string{"/corp/home/"})

	assert.True(t, IsAutomountNamespacePath("darwin", "/corp/home/user"),
		"a custom autofs mount must classify as automount")
	assert.True(t, IsAutomountNamespacePath("darwin", "/corp/home"),
		"the custom mount root itself must classify as automount")
	assert.True(t, IsAutomountNamespacePath("darwin", "/CORP/Home/user"),
		"case-folded spellings of a custom mount must match")
	assert.True(t, IsAutomountNamespacePath(
		"darwin", "/System/Volumes/Data/corp/home/user",
	), "the data-volume spelling of a custom mount must match")
	assert.False(t, IsAutomountNamespacePath("darwin", "/corp/homework"),
		"prefix matching must respect component boundaries")
	assert.False(t, IsAutomountNamespacePath("linux", "/corp/home/user"),
		"custom mounts are darwin-only like the fixed namespaces")

	assert.True(t, IsCanonicalAutomountNamespacePath(
		"darwin", "/corp/home/user",
	), "the canonical predicate accepts the mount table's exact form")
	assert.False(t, IsCanonicalAutomountNamespacePath(
		"darwin", "/CORP/home/user",
	), "the canonical predicate must not accept case-folded forms")

	home := t.TempDir()
	require.NoError(t, os.Symlink(
		"/corp/home/user", filepath.Join(home, "tocorp"),
	))
	origLstat := osLstat
	t.Cleanup(func() { osLstat = origLstat })
	osLstat = func(path string) (os.FileInfo, error) {
		if path == "/corp" || strings.HasPrefix(path, "/corp/") {
			assert.Fail(t, "custom automount must not be walked", path)
		}
		return origLstat(path)
	}
	assert.Equal(t, LocalPathProbeAutomountNamespace, ClassifyLocalPathProbe(
		"darwin", home, filepath.Join(home, "tocorp", "repo"), false,
	), "a symlink into a custom mount must stop the walk")
}

// TestClassifyLocalPathProbe pins the tri-state classification, including
// working directories that only reach a protected folder through a symlink,
// which the lexical predicates alone cannot see.
func TestClassifyLocalPathProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the predicate compares POSIX home paths built with filepath")
	}
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(home, "Documents", "proj"), 0o755,
	))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "src", "app"), 0o755))
	require.NoError(t, os.Symlink(
		filepath.Join(home, "Documents"), filepath.Join(home, "code"),
	))
	require.NoError(t, os.Symlink("./Documents", filepath.Join(home, "rel")))
	require.NoError(t, os.Symlink(
		filepath.Join(home, "loop"), filepath.Join(home, "loop"),
	))
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(home, "out")))

	tests := []struct {
		name string
		goos string
		home string
		path string
		want LocalPathProbeClass
	}{
		{"lexically protected", "darwin", home, filepath.Join(home, "Documents", "proj"), LocalPathProbeProtectedUserData},
		{"absolute symlink into protected", "darwin", home, filepath.Join(home, "code", "proj"), LocalPathProbeProtectedUserData},
		{"relative symlink into protected", "darwin", home, filepath.Join(home, "rel", "proj"), LocalPathProbeProtectedUserData},
		{"link loop fails protected", "darwin", home, filepath.Join(home, "loop", "proj"), LocalPathProbeProtectedUserData},
		{"automount namespace", "darwin", home, "/home/user/repo", LocalPathProbeAutomountNamespace},
		{"automount without home", "darwin", "", "/net/host/share", LocalPathProbeAutomountNamespace},
		{"data volume protected", "darwin", home, "/System/Volumes/Data" + home + "/Documents/proj", LocalPathProbeProtectedUserData},
		{"data volume automount", "darwin", home, "/System/Volumes/Data/home/user/repo", LocalPathProbeAutomountNamespace},
		{"plain unprotected", "darwin", home, filepath.Join(home, "src", "app"), LocalPathProbeSafe},
		{"symlink to unprotected", "darwin", home, filepath.Join(home, "out", "app"), LocalPathProbeSafe},
		{"missing unprotected path", "darwin", home, filepath.Join(home, "src", "gone", "deep"), LocalPathProbeSafe},
		{"empty home protected miss", "darwin", "", filepath.Join(home, "Documents", "proj"), LocalPathProbeSafe},
		{"linux never restricts", "linux", home, filepath.Join(home, "code", "proj"), LocalPathProbeSafe},
		{"relative path", "darwin", home, "code/proj", LocalPathProbeSafe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				ClassifyLocalPathProbe(tt.goos, tt.home, tt.path, false))
		})
	}
}

// TestClassifyLocalPathProbeSkipsAutomountNamespace pins that classification
// never Lstats inside a macOS automounter namespace, whether the input names
// it directly or a symlink hops into it mid-walk, and that the namespace is
// reported as its own class rather than as protected user data — the
// protected-path opt-in must not become permission to wake automountd.
func TestClassifyLocalPathProbeSkipsAutomountNamespace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the predicate compares POSIX home paths built with filepath")
	}
	home := t.TempDir()
	require.NoError(t, os.Symlink("/home", filepath.Join(home, "tohome")))
	orig := osLstat
	t.Cleanup(func() { osLstat = orig })
	osLstat = func(path string) (os.FileInfo, error) {
		if path == "/home" || strings.HasPrefix(path, "/home/") {
			assert.Fail(t, "automount namespace must not be walked", path)
		}
		return orig(path)
	}

	assert.Equal(t, LocalPathProbeAutomountNamespace, ClassifyLocalPathProbe(
		"darwin", home, "/home/user/repo", false), "a direct automount path must classify as automount")
	assert.Equal(t, LocalPathProbeAutomountNamespace, ClassifyLocalPathProbe(
		"darwin", home, filepath.Join(home, "tohome", "user", "repo"), false), "a symlink into the automount namespace must stop the walk")
}

// TestClassifyLocalPathProbeDotDotTraversalOrder pins that ".." resolves in
// traversal order, after the components before it: collapsing it lexically
// would drop an unresolved symlink component and classify a path as safe
// while real filesystem resolution reaches a protected location through it.
func TestClassifyLocalPathProbeDotDotTraversalOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the predicate compares POSIX home paths built with filepath")
	}
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(
		filepath.Join(home, "Documents", "sub"), 0o755,
	))
	require.NoError(t, os.MkdirAll(filepath.Join(home, "src", "app"), 0o755))
	// q links into Documents; home/q/../x resolves through Documents.
	require.NoError(t, os.Symlink(
		filepath.Join(home, "Documents", "sub"), filepath.Join(home, "q"),
	))
	// Chained relative target: l1 -> "l2/../safe" where l2 links into
	// Documents; the kernel resolves l2 before applying "..".
	require.NoError(t, os.Symlink(
		filepath.Join(home, "Documents", "sub"), filepath.Join(home, "l2"),
	))
	require.NoError(t, os.Symlink("l2/../safe", filepath.Join(home, "l1")))

	tests := []struct {
		name string
		path string
		want LocalPathProbeClass
	}{
		{"dotdot after symlink into protected", filepath.Join(home, "q") + "/../x", LocalPathProbeProtectedUserData},
		{"chained relative target with dotdot", filepath.Join(home, "l1"), LocalPathProbeProtectedUserData},
		{"dotdot through protected midpoint", home + "/Documents/../src/app", LocalPathProbeProtectedUserData},
		{"dotdot through automount", "/home/../tmp/x", LocalPathProbeAutomountNamespace},
		{"dotdot through safe components", home + "/src/app/../app", LocalPathProbeSafe},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want,
				ClassifyLocalPathProbe("darwin", home, tt.path, false))
		})
	}
}

// TestClassifyLocalPathProbeOptInStillRefusesAutomount pins that the
// protected-folder opt-in continues resolution through protected prefixes:
// a symlink hidden inside ~/Documents that leads into an automounter
// namespace must classify as automount, because the opt-in lifts consent
// prompts, never automountd wakeups. Without the opt-in the walk still
// stops at the protected prefix untouched.
func TestClassifyLocalPathProbeOptInStillRefusesAutomount(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the predicate compares POSIX home paths built with filepath")
	}
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(home, "Documents"), 0o755))
	require.NoError(t, os.Symlink(
		"/home/user", filepath.Join(home, "Documents", "tohome"),
	))
	hidden := filepath.Join(home, "Documents", "tohome", "repo")

	assert.Equal(t, LocalPathProbeAutomountNamespace,
		ClassifyLocalPathProbe("darwin", home, hidden, true),
		"opt-in must keep resolving and find the automount target")
	assert.Equal(t, LocalPathProbeProtectedUserData,
		ClassifyLocalPathProbe("darwin", home,
			filepath.Join(home, "Documents", "proj"), true),
		"a protected-only path stays protected under the opt-in")

	orig := osLstat
	t.Cleanup(func() { osLstat = orig })
	osLstat = func(path string) (os.FileInfo, error) {
		if strings.HasPrefix(path, filepath.Join(home, "Documents")) {
			assert.Fail(t, "opt-out must not Lstat inside protected", path)
		}
		return orig(path)
	}
	assert.Equal(t, LocalPathProbeProtectedUserData,
		ClassifyLocalPathProbe("darwin", home, hidden, false),
		"without the opt-in the walk stops at the protected prefix")
}

// TestClassifyLocalPathProbeAutomountHomeNeverResolved pins that resolving
// the home directory for lexical comparison never touches an automounter
// namespace: a home under /home (autofs user homes) or reached through a
// /net link must not wake automountd on every classification call.
func TestClassifyLocalPathProbeAutomountHomeNeverResolved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the predicate compares POSIX home paths built with filepath")
	}
	outside := t.TempDir()
	linkedHome := filepath.Join(outside, "nethome")
	require.NoError(t, os.Symlink("/net/host/users/x", linkedHome))
	orig := osLstat
	t.Cleanup(func() { osLstat = orig })
	osLstat = func(path string) (os.FileInfo, error) {
		for _, ns := range []string{"/home", "/net", "/Network/Servers"} {
			if path == ns || strings.HasPrefix(path, ns+"/") {
				assert.Fail(t, "automount namespace must not be walked", path)
			}
		}
		return orig(path)
	}

	assert.Equal(t, LocalPathProbeSafe, ClassifyLocalPathProbe(
		"darwin", "/home/user", filepath.Join(outside, "elsewhere"), false), "an automount home must not derail classifying an unrelated path")
	assert.Equal(t, LocalPathProbeSafe, ClassifyLocalPathProbe(
		"darwin", linkedHome, filepath.Join(outside, "elsewhere"), false), "resolving a home linked through /net must abort, not follow")
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

func TestResolveProjectReferenceScrubsPrivateLocalIdentity(t *testing.T) {
	got := ResolveProjectReference(ProjectIdentityInput{
		DisplayLabel:     "app-copy",
		RootPath:         "/private/work/app-copy",
		GitRemote:        "https://user:token@example.com/acme/app.git?x=1#frag",
		RepositoryPath:   "/private/work/app-copy/.git",
		WorktreeRootPath: "/private/work/app-copy",
		WorktreeKind:     WorktreeLinked,
		GitBranch:        "feature/a",
	}, IdentityScope{
		ArchiveID:   "archive-fixture",
		ArchiveSalt: "salt-fixture",
		MachineID:   "machine-fixture",
	})

	require.NotNil(t, got.Identity)
	assert.Equal(t, "app-copy", got.DisplayLabel)
	assert.Equal(t, ProjectResolutionResolved, got.Resolution)
	assert.Equal(t, ProjectKindGitRemote, got.Identity.Kind)
	assert.Equal(t, "example.com/acme/app", got.Identity.NormalizedRemote)
	assert.NotEmpty(t, got.Identity.Key)
	assert.NotEmpty(t, got.Identity.RootKey)
	assert.NotEmpty(t, got.Identity.RepositoryKey)
	assert.Equal(t, WorktreeLinked, got.Worktree.Relationship)
	assert.NotEmpty(t, got.Worktree.WorktreeKey)
	assert.Equal(t, got.Identity.RepositoryKey, got.Worktree.RepositoryKey)
	assert.Equal(t, CheckoutBranch, got.Checkout.State)
	assert.Equal(t, "feature/a", got.Checkout.Branch)

	encoded := marshalProjectJSON(t, got)
	assert.NotContains(t, encoded, "/private/work")
	assert.NotContains(t, encoded, "user:token")
}

func TestResolveProjectReferenceUsesMachineScopedRootWithoutRemote(t *testing.T) {
	input := ProjectIdentityInput{
		DisplayLabel:     "local-app",
		RootPath:         "/work/local-app",
		WorktreeRootPath: "/work/local-app",
		WorktreeKind:     WorktreeMain,
	}

	first := ResolveProjectReference(input, IdentityScope{
		ArchiveID: "archive", ArchiveSalt: "salt", MachineID: "machine-a",
	})
	second := ResolveProjectReference(input, IdentityScope{
		ArchiveID: "archive", ArchiveSalt: "salt", MachineID: "machine-b",
	})

	require.NotNil(t, first.Identity)
	require.NotNil(t, second.Identity)
	assert.Equal(t, ProjectKindMachineRoot, first.Identity.Kind)
	assert.NotEqual(t, first.Identity.Key, second.Identity.Key)
	assert.Empty(t, first.Identity.NormalizedRemote)
	assert.Equal(t, CheckoutUnknown, first.Checkout.State)
	assert.NotContains(t, marshalProjectJSON(t, first), "/work/local-app")
}

func TestResolveRemoteSelectionFailsClosedWithoutOrigin(t *testing.T) {
	selection := ResolveRemoteSelection(map[string]string{
		"beta": "https://example.com/acme/beta.git",
		"zeta": "https://example.com/acme/zeta.git",
	})

	assert.Equal(t, ProjectResolutionAmbiguous, selection.Resolution)
	assert.Empty(t, selection.Name)
	assert.Empty(t, selection.Raw)
	assert.Empty(t, selection.Normalized)
}

func TestResolveProjectReferencePropagatesAmbiguousRemoteSelection(t *testing.T) {
	got := ResolveProjectReference(ProjectIdentityInput{
		DisplayLabel:     "app",
		RootPath:         "/work/app-worktree",
		RepositoryPath:   "/work/app/.git",
		WorktreeRootPath: "/work/app-worktree",
		WorktreeKind:     WorktreeLinked,
		Detached:         true,
		RemoteSelection: ResolveRemoteSelection(map[string]string{
			"one": "https://example.com/acme/one.git",
			"two": "https://example.com/acme/two.git",
		}),
	}, IdentityScope{ArchiveID: "archive", ArchiveSalt: "salt", MachineID: "machine"})

	assert.Equal(t, ProjectResolutionAmbiguous, got.Resolution)
	assert.Nil(t, got.Identity)
	assert.Equal(t, WorktreeLinked, got.Worktree.Relationship)
	assert.NotEmpty(t, got.Worktree.WorktreeKey)
	assert.NotEmpty(t, got.Worktree.RepositoryKey)
	assert.Equal(t, CheckoutDetached, got.Checkout.State)
	assert.NotContains(t, marshalProjectJSON(t, got), `"identity"`)
}

func TestBuildProjectsMapPreservesObservedRemoteAmbiguity(t *testing.T) {
	got := BuildProjectsMap([]string{"app"}, []ProjectIdentityObservation{{
		Project:          "app",
		Machine:          "machine",
		RootPath:         "/work/app",
		RemoteResolution: ProjectResolutionAmbiguous,
	}})

	assert.Equal(t, ProjectResolutionAmbiguous, got["app"].Resolution)
	assert.Nil(t, got["app"].Identity)
}

func TestAggregateIdentityScopeIsOrderIndependentAndArchiveSensitive(t *testing.T) {
	first := AggregateIdentityScope([]IdentityScope{
		{ArchiveID: "archive-b", ArchiveSalt: "salt-b"},
		{ArchiveID: "archive-a", ArchiveSalt: "salt-a"},
	})
	reordered := AggregateIdentityScope([]IdentityScope{
		{ArchiveID: "archive-a", ArchiveSalt: "salt-a"},
		{ArchiveID: "archive-b", ArchiveSalt: "salt-b"},
	})
	changed := AggregateIdentityScope([]IdentityScope{
		{ArchiveID: "archive-a", ArchiveSalt: "salt-a"},
		{ArchiveID: "archive-c", ArchiveSalt: "salt-c"},
	})

	assert.Equal(t, first, reordered)
	assert.NotEmpty(t, first.ArchiveID)
	assert.NotEmpty(t, first.ArchiveSalt)
	assert.NotEqual(t, first, changed)
}

func TestResolveProjectReferenceSharesNoRemoteRepositoryAcrossWorktrees(t *testing.T) {
	scope := IdentityScope{ArchiveID: "archive", ArchiveSalt: "salt", MachineID: "machine"}
	main := ResolveProjectReference(ProjectIdentityInput{
		RootPath:         "/work/app",
		RepositoryPath:   "/work/app/.git",
		WorktreeRootPath: "/work/app",
		WorktreeKind:     WorktreeMain,
	}, scope)
	linked := ResolveProjectReference(ProjectIdentityInput{
		RootPath:         "/work/app-feature",
		RepositoryPath:   "/work/app/.git",
		WorktreeRootPath: "/work/app-feature",
		WorktreeKind:     WorktreeLinked,
	}, scope)

	require.NotNil(t, main.Identity)
	require.NotNil(t, linked.Identity)
	assert.Equal(t, main.Identity.Key, linked.Identity.Key)
	assert.Equal(t, main.Identity.RepositoryKey, linked.Identity.RepositoryKey)
	assert.NotEqual(t, main.Identity.RootKey, linked.Identity.RootKey)
	assert.NotEqual(t, main.Worktree.WorktreeKey, linked.Worktree.WorktreeKey)
}

func TestResolveProjectReferenceRequiresCompleteLocalScope(t *testing.T) {
	input := ProjectIdentityInput{
		RootPath:         "/work/app",
		RepositoryPath:   "/work/app/.git",
		WorktreeRootPath: "/work/app",
		WorktreeKind:     WorktreeMain,
	}

	for name, scope := range map[string]IdentityScope{
		"missing archive": {ArchiveSalt: "salt", MachineID: "machine"},
		"missing salt":    {ArchiveID: "archive", MachineID: "machine"},
		"missing machine": {ArchiveID: "archive", ArchiveSalt: "salt"},
	} {
		t.Run(name, func(t *testing.T) {
			got := ResolveProjectReference(input, scope)
			assert.Equal(t, ProjectResolutionUnknown, got.Resolution)
			assert.Nil(t, got.Identity)
			assert.Empty(t, got.Worktree.WorktreeKey)
		})
	}
}

func TestResolveProjectReferenceRemoteRenameAndSelectionMatrix(t *testing.T) {
	scope := IdentityScope{ArchiveID: "archive", ArchiveSalt: "salt", MachineID: "machine"}
	tests := []struct {
		name       string
		remotes    map[string]string
		resolution ProjectResolution
	}{
		{
			name: "origin wins",
			remotes: map[string]string{
				"origin":   "git@example.com:acme/app.git",
				"upstream": "https://example.com/acme/upstream.git",
			},
			resolution: ProjectResolutionResolved,
		},
		{
			name:       "single non-origin",
			remotes:    map[string]string{"upstream": "https://example.com/acme/app.git"},
			resolution: ProjectResolutionResolved,
		},
		{
			name: "ambiguous non-origin",
			remotes: map[string]string{
				"one": "https://example.com/acme/one.git",
				"two": "https://example.com/acme/two.git",
			},
			resolution: ProjectResolutionAmbiguous,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selection := ResolveRemoteSelection(tt.remotes)
			got := ResolveProjectReference(ProjectIdentityInput{
				RootPath:         "/old/app",
				RepositoryPath:   "/old/app/.git",
				WorktreeRootPath: "/old/app",
				WorktreeKind:     WorktreeMain,
				RemoteSelection:  selection,
			}, scope)
			assert.Equal(t, tt.resolution, got.Resolution)
			if tt.resolution != ProjectResolutionResolved {
				assert.Nil(t, got.Identity)
				return
			}
			require.NotNil(t, got.Identity)
			renamed := ResolveProjectReference(ProjectIdentityInput{
				RootPath:         "/renamed/app",
				RepositoryPath:   "/renamed/app/.git",
				WorktreeRootPath: "/renamed/app",
				WorktreeKind:     WorktreeMain,
				RemoteSelection:  selection,
			}, scope)
			require.NotNil(t, renamed.Identity)
			assert.Equal(t, got.Identity.Key, renamed.Identity.Key)
			assert.Equal(t, got.Identity.RepositoryKey, renamed.Identity.RepositoryKey)
			assert.NotEqual(t, got.Identity.RootKey, renamed.Identity.RootKey)
		})
	}
}

func TestResolveProjectReferenceRejectsContradictoryOrPrivateContext(t *testing.T) {
	got := ResolveProjectReference(ProjectIdentityInput{
		DisplayLabel:     "/private/app",
		RootPath:         "/work/app",
		RepositoryPath:   "/work/app/.git",
		WorktreeRootPath: "/work/app",
		WorktreeKind:     WorktreeNone,
		GitBranch:        "https://user:token@example.com/private",
	}, IdentityScope{ArchiveID: "archive", ArchiveSalt: "salt", MachineID: "machine"})

	assert.Empty(t, got.DisplayLabel)
	assert.Empty(t, got.Worktree.WorktreeKey)
	assert.Equal(t, CheckoutUnknown, got.Checkout.State)
	assert.Empty(t, got.Checkout.Branch)
	encoded := marshalProjectJSON(t, got)
	assert.NotContains(t, encoded, "/private/app")
	assert.NotContains(t, encoded, "token")
}

func TestResolveProjectReferenceMarksDetachedCheckout(t *testing.T) {
	got := ResolveProjectReference(ProjectIdentityInput{
		DisplayLabel: "app",
		RootPath:     "/work/app",
		GitRemote:    "git@example.com:acme/app.git",
		Detached:     true,
		WorktreeKind: WorktreeLinked,
	}, IdentityScope{ArchiveID: "archive", ArchiveSalt: "salt", MachineID: "machine"})

	assert.Equal(t, CheckoutDetached, got.Checkout.State)
	assert.Empty(t, got.Checkout.Branch)
	assert.Equal(t, WorktreeLinked, got.Worktree.Relationship)
}

func marshalProjectJSON(t *testing.T, value any) string {
	t.Helper()

	data, err := json.Marshal(value)
	require.NoError(t, err)
	return string(data)
}
