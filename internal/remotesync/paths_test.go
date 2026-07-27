package remotesync

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forbiddenRootCase is the shared edge-case table used to characterize the
// pre-unification divergence between the two forbidden-root predicates and
// to drive the unified replacement, PathWithinForbiddenRoots.
type forbiddenRootCase struct {
	name  string
	roots []string
	path  string
	want  bool
}

// forbiddenRootCases covers the edge cases called out in the task brief:
// root "/", root == path, trailing-slash roots, prefix-but-not-boundary
// (/a/bc vs root /a/b), and relative traversal (..). Cases are written with
// '/' separators and translated to '\' by the backslash-separator test
// below, so both the local (OS filepath) and remote (POSIX) call sites are
// exercised against the same table.
var forbiddenRootCases = []forbiddenRootCase{
	{"root_slash_matches_deep_path", []string{"/"}, "/etc/passwd", true},
	{"root_slash_matches_itself", []string{"/"}, "/", true},
	{"root_slash_does_not_match_relative_path", []string{"/"}, "relative/x", false},
	{"root_equal_path", []string{"/a/b"}, "/a/b", true},
	{"root_child_matches", []string{"/a/b"}, "/a/b/c", true},
	{"trailing_slash_root_matches_child", []string{"/a/b/"}, "/a/b/c", true},
	{"trailing_slash_root_matches_itself", []string{"/a/b/"}, "/a/b", true},
	{"prefix_not_boundary_bc", []string{"/a/b"}, "/a/bc", false},
	{"prefix_not_boundary_more", []string{"/a/b"}, "/a/bmore", false},
	{"relative_traversal_escapes_root", []string{"/a/b"}, "/a/b/../../etc", false},
	{"relative_traversal_stays_under_root", []string{"/a"}, "/a/b/../../a/c", true},
	{"relative_traversal_resolves_to_sibling", []string{"/a/b"}, "/a/b/../c", false},
	{"relative_root_and_path_match", []string{"secret"}, "secret/x", true},
	{"relative_path_against_absolute_root", []string{"/a/b"}, "relative/x", false},
	{"empty_root_never_matches", []string{""}, "/a/b", false},
	{"second_root_in_list_matches", []string{"/x", "/a/b"}, "/a/b/c", true},
	{"no_roots_never_matches", nil, "/a/b", false},
	{"path_is_bare_dotdot", []string{"/a/b"}, "..", false},
}

// TestPathWithinForbiddenRootsSlashSeparator exercises PathWithinForbiddenRoots
// with '/', the separator internal/ssh uses for remote POSIX paths, and the
// separator internal/remotesync's local wrapper also uses on non-Windows
// hosts (filepath.Separator == '/').
func TestPathWithinForbiddenRootsSlashSeparator(t *testing.T) {
	for _, tc := range forbiddenRootCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PathWithinForbiddenRoots(tc.roots, tc.path, '/'))
		})
	}
}

// TestPathWithinForbiddenRootsBackslashSeparator exercises the same table
// through the Windows-style separator ('\'). Neither pre-unification donor
// implementation could be exercised against this separator on a non-Windows
// runner: the old internal/remotesync predicate depended on
// filepath.Separator, which is only '\' on an actual Windows GOOS build, and
// the old internal/ssh predicate was hardcoded to '/'. Because the unified
// predicate takes sep as an explicit parameter, local-filepath behavior is
// now testable on any host OS.
func TestPathWithinForbiddenRootsBackslashSeparator(t *testing.T) {
	for _, tc := range forbiddenRootCases {
		t.Run(tc.name, func(t *testing.T) {
			roots := toBackslash(tc.roots)
			path := strings.ReplaceAll(tc.path, "/", `\`)
			assert.Equal(t, tc.want, PathWithinForbiddenRoots(roots, path, '\\'))
		})
	}
}

func toBackslash(roots []string) []string {
	if roots == nil {
		return nil
	}
	out := make([]string, len(roots))
	for i, r := range roots {
		out[i] = strings.ReplaceAll(r, "/", `\`)
	}
	return out
}

// TestPathWithinForbiddenRootsWindowsStyleFixtures uses literal
// backslash/UNC/drive-letter strings (not derived from forbiddenRootCases via
// ReplaceAll) to document PathWithinForbiddenRoots' actual behavior on
// Windows-shaped paths, including the volume-awareness gap called out on
// PathWithinForbiddenRoots and cleanPathWithSeparator: normalization here is
// not volume-aware the way real filepath.Clean/filepath.Rel are on Windows.
// The UNC and drive-letter cases below happen to resolve correctly because
// root and path pass through the same lossy transform (the leading "\\" of a
// UNC path collapses to a single separator for both, and a differing drive
// letter is still rejected because it's literally a different leading
// string, not because this function understands volumes) — this is the
// "collapse cancels out" property the doc comments describe, not general
// volume awareness.
func TestPathWithinForbiddenRootsWindowsStyleFixtures(t *testing.T) {
	tests := []forbiddenRootCase{
		{
			"unc_root_matches_child",
			[]string{`\\server\share\Secret`}, `\\server\share\Secret\file.txt`, true,
		},
		{
			"unc_root_matches_itself",
			[]string{`\\server\share\Secret`}, `\\server\share\Secret`, true,
		},
		{
			"unc_prefix_not_boundary",
			[]string{`\\server\share\Secret`}, `\\server\share\Secret2\file.txt`, false,
		},
		{
			"drive_letter_root_matches_child",
			[]string{`C:\Users\foo\Secret`}, `C:\Users\foo\Secret\file.txt`, true,
		},
		{
			"drive_letter_mismatch_different_volume_rejected",
			[]string{`C:\Users\foo\Secret`}, `D:\Users\foo\Secret\file.txt`, false,
		},
		{
			"drive_letter_relative_traversal_resolves_to_sibling",
			[]string{`C:\a\b`}, `C:\a\b\..\c`, false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PathWithinForbiddenRoots(tc.roots, tc.path, '\\'))
		})
	}
}

// TestPathWithinForbiddenRootsLocalWrapper confirms the unexported
// pathWithinForbiddenRoots wrapper used by this package's other files
// (archive.go, manifest.go, resolve.go, types.go) matches the exported
// predicate for local OS paths, except where its canonicalization
// deliberately strengthens matching: a relative path is anchored to the
// working directory before comparison, so root "/" now guards it too.
func TestPathWithinForbiddenRootsLocalWrapper(t *testing.T) {
	localWants := map[string]bool{
		// filepath.Abs anchors "relative/x" under cwd, which lies
		// beneath root "/" — the wrapper intentionally diverges from
		// the pure lexical predicate here.
		"root_slash_does_not_match_relative_path": true,
	}
	for _, tc := range forbiddenRootCases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if localWant, ok := localWants[tc.name]; ok {
				want = localWant
			}
			assert.Equal(t, want, pathWithinForbiddenRoots(tc.roots, tc.path))
		})
	}
}

// TestPathWithinForbiddenRootsLocalWrapperCanonicalizesRelativeSpellings
// pins the fix for filesystem-equivalent alias spellings: a forbidden root
// configured relatively must still guard its absolute alias's children,
// and vice versa, and a root of "." must guard relative children.
func TestPathWithinForbiddenRootsLocalWrapperCanonicalizesRelativeSpellings(
	t *testing.T,
) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Chdir(base)
	absRoot := filepath.Join(base, "sessions")

	assert.True(t, pathWithinForbiddenRoots(
		[]string{"sessions"}, filepath.Join(absRoot, "chat.db"),
	), "relative root spelling must guard absolute children")
	assert.True(t, pathWithinForbiddenRoots(
		[]string{absRoot}, filepath.Join("sessions", "chat.db"),
	), "absolute root must guard relatively spelled children")
	assert.True(t, pathWithinForbiddenRoots([]string{"."}, "sessions"),
		"root \".\" must guard relative children of the working directory")
	assert.False(t, pathWithinForbiddenRoots(
		[]string{"sessions"}, filepath.Join(base, "sessions2"),
	), "canonicalization must preserve component-boundary matching")
}

// TestPathWithinForbiddenRootsLocalWrapperResolvesSymlinkAliases pins the
// symlink half of alias handling: a path reached through a symlinked
// ancestor must compare by its physical location, in both directions —
// alias-spelled path against physical forbidden root, and alias-spelled
// forbidden root against physical path.
func TestPathWithinForbiddenRootsLocalWrapperResolvesSymlinkAliases(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses POSIX symlinks")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	forbidden := filepath.Join(base, "trae")
	require.NoError(t, os.MkdirAll(filepath.Join(forbidden, "sub"), 0o755))
	alias := filepath.Join(base, "alias")
	require.NoError(t, os.Symlink(forbidden, alias))

	assert.True(t, pathWithinForbiddenRoots(
		[]string{forbidden}, filepath.Join(alias, "sub", "chat.db"),
	), "alias-spelled path must resolve into the physical forbidden root")
	assert.True(t, pathWithinForbiddenRoots(
		[]string{alias}, filepath.Join(forbidden, "sub", "chat.db"),
	), "alias-spelled forbidden root must guard its physical children")
	assert.True(t, pathWithinForbiddenRoots(
		[]string{forbidden}, filepath.Join(alias, "sub", "missing", "x"),
	), "missing tails resolve through their longest existing ancestor")
	assert.False(t, pathWithinForbiddenRoots(
		[]string{forbidden}, filepath.Join(base, "other"),
	), "unrelated siblings stay outside the boundary")
}

// TestPathWithinForbiddenRootsLocalWrapperCaseFolding asserts the local
// wrapper compares case-insensitively exactly on hosts whose default
// filesystems are case-insensitive (Windows, macOS) and case-sensitively
// everywhere else.
func TestPathWithinForbiddenRootsLocalWrapperCaseFolding(t *testing.T) {
	got := pathWithinForbiddenRoots([]string{"/a/Secret"}, "/a/sECRET/file")
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		assert.True(t, got,
			"case-variant alias of a forbidden root must match on a case-insensitive host")
	} else {
		assert.False(t, got,
			"case-sensitive hosts must not conflate case-variant paths")
	}
}
