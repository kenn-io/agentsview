package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/export"
)

// TestExtractProjectFromCwdGuardedCwdSkipsGitWalk pins that when the
// protected-path guard refuses a cwd, project extraction falls back to the
// path basename without touching the cwd's subtree on disk. The fixture is a
// real git repo whose root name differs from the cwd basename, so a missing
// guard is caught twice: the stat spy fires and the extracted name flips to
// the repo root's.
func TestExtractProjectFromCwdGuardedCwdSkipsGitWalk(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "guarded-repo")
	cwd := filepath.Join(repo, "nested")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(cwd, 0o755))

	origGuard := probeGitRootForCwd
	t.Cleanup(func() { probeGitRootForCwd = origGuard })
	probeGitRootForCwd = func(cleaned string) bool {
		return cleaned != filepath.Clean(cwd)
	}
	origStat := osStat
	t.Cleanup(func() { osStat = origStat })
	osStat = func(path string) (os.FileInfo, error) {
		if strings.HasPrefix(path, repo) {
			assert.Fail(t, "guarded cwd must not be stat-ed", path)
		}
		return origStat(path)
	}

	assert.Equal(t, "nested", ExtractProjectFromCwd(cwd),
		"a guarded cwd must fall back to its basename")
}

func TestExtractProjectFromCwdContextCanDisableFilesystemDiscovery(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repository")
	cwd := filepath.Join(repo, "recorded-cwd")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	require.NoError(t, os.MkdirAll(cwd, 0o755))

	origGuard := probeGitRootForCwd
	t.Cleanup(func() { probeGitRootForCwd = origGuard })
	probeGitRootForCwd = func(string) bool {
		assert.Fail(t, "lexical project discovery must not probe the filesystem")
		return true
	}

	ctx := WithoutFilesystemProjectDiscovery(t.Context())
	assert.Equal(t, "recorded_cwd",
		ExtractProjectFromCwdWithBranchContext(ctx, cwd, ""))
}

// TestExtractProjectFromCwdProtectedGitdirTargetFallsBack pins that a linked
// worktree in an unguarded directory whose .git file targets a refused gitdir
// stops at the worktree itself: following the target would read commondir and
// config inside the refused location, and escalating to gitMainRoot would
// exec git, which reads the same target. The main repository name must not
// leak into the result.
func TestExtractProjectFromCwdProtectedGitdirTargetFallsBack(t *testing.T) {
	root := t.TempDir()
	guarded := filepath.Join(root, "guarded")
	mainGitDir := filepath.Join(guarded, "main", ".git")
	worktreeGitDir := filepath.Join(mainGitDir, "worktrees", "wt")
	require.NoError(t, os.MkdirAll(worktreeGitDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(worktreeGitDir, "commondir"), []byte("../..\n"), 0o644,
	))
	worktree := filepath.Join(root, "work", "wt-checkout")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(worktree, ".git"),
		[]byte("gitdir: "+worktreeGitDir+"\n"), 0o644,
	))

	origGuard := probeGitfileTarget
	t.Cleanup(func() { probeGitfileTarget = origGuard })
	probeGitfileTarget = func(cleaned string) bool {
		return !strings.HasPrefix(cleaned, guarded)
	}

	assert.Equal(t, "wt_checkout", ExtractProjectFromCwd(worktree),
		"a refused gitdir target must fall back to the worktree basename")
}

// TestExtractProjectFromCwdSymlinkedGitFileFallsBack pins that a .git entry
// which is itself a symlink into a guarded location is refused before being
// read: the walker's type probe sees a regular file through the link, and
// reading it would traverse into the guarded folder. The link target is a
// valid gitfile pointing at a safe main repository, so a missing vet is
// caught by the main repository's name leaking into the result.
func TestExtractProjectFromCwdSymlinkedGitFileFallsBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe classifier walks POSIX paths and symlinks")
	}
	home := t.TempDir()
	mainGitDir := filepath.Join(home, "src", "main-repo", ".git")
	worktreeGitDir := filepath.Join(mainGitDir, "worktrees", "wt")
	require.NoError(t, os.MkdirAll(worktreeGitDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(worktreeGitDir, "commondir"), []byte("../..\n"), 0o644,
	))
	guardedFile := filepath.Join(home, "Documents", "redirect.git")
	require.NoError(t, os.MkdirAll(filepath.Dir(guardedFile), 0o755))
	require.NoError(t, os.WriteFile(
		guardedFile, []byte("gitdir: "+worktreeGitDir+"\n"), 0o644,
	))
	worktree := filepath.Join(home, "work", "wt-link")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.Symlink(
		guardedFile, filepath.Join(worktree, ".git"),
	))

	// Exercise the real classifier with an injected darwin home so the
	// symlink is resolved and refused on any host platform.
	origGuard := probeGitfileTarget
	t.Cleanup(func() { probeGitfileTarget = origGuard })
	probeGitfileTarget = func(cleaned string) bool {
		return export.ClassifyLocalPathProbe("darwin", home, cleaned, false) ==
			export.LocalPathProbeSafe
	}

	assert.Equal(t, "wt_link", ExtractProjectFromCwd(worktree),
		"a .git symlink into a guarded location must not be read")
}

// TestDefaultProbeGitRootForCwdHonorsProtectedHome pins the default guard's
// wiring on macOS: a cwd under $HOME/Documents is refused until
// SetAllowProtectedPathProbes opts in. Darwin-only because the guard reads
// runtime.GOOS; the resolution logic itself is covered cross-platform in
// internal/export.
func TestDefaultProbeGitRootForCwdHonorsProtectedHome(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the default guard only restricts paths on darwin")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	protected := filepath.Join(home, "Documents", "proj")
	require.NoError(t, os.MkdirAll(protected, 0o755))
	plain := filepath.Join(home, "src", "proj")
	require.NoError(t, os.MkdirAll(plain, 0o755))

	assert.False(t, defaultProbeGitRootForCwd(protected),
		"protected cwd must be refused by default")
	assert.True(t, defaultProbeGitRootForCwd(plain),
		"unprotected cwd must stay probeable")

	SetAllowProtectedPathProbes(true)
	t.Cleanup(func() { SetAllowProtectedPathProbes(false) })
	assert.True(t, defaultProbeGitRootForCwd(protected),
		"opting in must allow protected cwd probes")
}

// TestRepoRootFromSiblingsSkipsRefusedGitfileTargets pins that missing-cwd
// recovery applies the same gitfile-target vetting as the upward walk: a
// sibling worktree whose gitfile targets a refused location must not have
// its commondir read or its main repository's name recovered. The deleted
// cwd falls back to its own basename instead.
func TestRepoRootFromSiblingsSkipsRefusedGitfileTargets(t *testing.T) {
	root := t.TempDir()
	guarded := filepath.Join(root, "guarded")
	mainGitDir := filepath.Join(guarded, "main-docs", ".git")
	worktreeGitDir := filepath.Join(mainGitDir, "worktrees", "wt1")
	require.NoError(t, os.MkdirAll(worktreeGitDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(worktreeGitDir, "commondir"), []byte("../..\n"), 0o644,
	))
	parent := filepath.Join(root, "worktrees")
	sibling := filepath.Join(parent, "wt1")
	require.NoError(t, os.MkdirAll(sibling, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sibling, ".git"),
		[]byte("gitdir: "+worktreeGitDir+"\n"), 0o644,
	))

	origGuard := probeGitfileTarget
	t.Cleanup(func() { probeGitfileTarget = origGuard })
	probeGitfileTarget = func(cleaned string) bool {
		return !strings.HasPrefix(cleaned, guarded)
	}

	// The recorded cwd is a deleted child of parent, so recovery scans
	// parent's siblings for linked-worktree gitfiles.
	deleted := filepath.Join(parent, "wt2", "src")
	assert.Equal(t, "src", ExtractProjectFromCwd(deleted),
		"a refused sibling gitfile target must not name the main repository")
}

// TestRepoRootFromSiblingsBoundaryCheckDoesNotFollowRefusedSymlink pins that
// the ancestor boundary check in missing-cwd recovery types a refused .git
// symlink without following it: the old following stat traversed the link
// into the guarded target before any vet ran.
func TestRepoRootFromSiblingsBoundaryCheckDoesNotFollowRefusedSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe classifier walks POSIX paths and symlinks")
	}
	home := t.TempDir()
	guarded := filepath.Join(home, "Documents", "main", ".git")
	require.NoError(t, os.MkdirAll(guarded, 0o755))
	ancestor := filepath.Join(home, "work")
	require.NoError(t, os.MkdirAll(ancestor, 0o755))
	gitLink := filepath.Join(ancestor, ".git")
	require.NoError(t, os.Symlink(guarded, gitLink))

	// Exercise the real classifier with an injected darwin home: the guard
	// receives the symlink's own path and must refuse it by resolving to
	// the guarded target.
	origGuard := probeGitfileTarget
	t.Cleanup(func() { probeGitfileTarget = origGuard })
	probeGitfileTarget = func(cleaned string) bool {
		return export.ClassifyLocalPathProbe("darwin", home, cleaned, false) ==
			export.LocalPathProbeSafe
	}
	origStat := osStat
	t.Cleanup(func() { osStat = origStat })
	osStat = func(path string) (os.FileInfo, error) {
		if path == gitLink {
			assert.Fail(t, "a refused .git symlink must not be stat-followed", path)
		}
		return origStat(path)
	}

	// The recorded cwd is a deleted child of ancestor, so recovery reaches
	// the boundary check with ancestor's refused .git symlink. The refused
	// entry marks ancestor as the repo boundary, so its basename becomes
	// the path-only name; the stat spy above is what catches a regression
	// back to a following stat.
	deleted := filepath.Join(ancestor, "gone", "sub")
	assert.Equal(t, "work", ExtractProjectFromCwd(deleted),
		"a refused boundary must name the boundary directory, unread")
}

// TestExtractProjectFromCwdSymlinkedCommondirFallsBack pins the exact-file
// vet on the parser side: a vetted gitdir whose commondir file is a symlink
// into a guarded location must not be read through, so the main repository
// it names cannot be recovered.
func TestExtractProjectFromCwdSymlinkedCommondirFallsBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe classifier walks POSIX paths and symlinks")
	}
	home := t.TempDir()
	mainRepo := filepath.Join(home, "src", "main-repo")
	require.NoError(t, os.MkdirAll(filepath.Join(mainRepo, ".git"), 0o755))
	gitStore := filepath.Join(home, "gitstore", "wt-git")
	require.NoError(t, os.MkdirAll(gitStore, 0o755))
	target := filepath.Join(home, "Documents", "commondir-target")
	require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
	require.NoError(t, os.WriteFile(
		target, []byte(filepath.Join(mainRepo, ".git")+"\n"), 0o644,
	))
	require.NoError(t, os.Symlink(
		target, filepath.Join(gitStore, "commondir"),
	))
	worktree := filepath.Join(home, "work", "wt-x")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(worktree, ".git"),
		[]byte("gitdir: "+gitStore+"\n"), 0o644,
	))

	origGuard := probeGitfileTarget
	t.Cleanup(func() { probeGitfileTarget = origGuard })
	probeGitfileTarget = func(cleaned string) bool {
		return export.ClassifyLocalPathProbe("darwin", home, cleaned, false) ==
			export.LocalPathProbeSafe
	}

	assert.Equal(t, "wt_x", ExtractProjectFromCwd(worktree),
		"a commondir symlink into a guarded location must not be read")
}

// TestRepoRootFromSiblingsSkipsGuardedSiblings pins that missing-cwd
// recovery vets each sibling before typing its .git entry: when the first
// existing ancestor is the home directory, the siblings include Documents
// and the other guarded folders, and typing them would Lstat inside — while
// a guarded sibling holding a real .git directory would flow into
// deletedChildIsWorktree's ReadDir of its worktrees list. The fixture makes
// Documents exactly that sibling, with the deleted child registered as its
// worktree, so a missing vet is caught by "Documents" becoming the name.
func TestRepoRootFromSiblingsSkipsGuardedSiblings(t *testing.T) {
	home := t.TempDir()
	worktreesDir := filepath.Join(home, "Documents", ".git", "worktrees")
	require.NoError(t, os.MkdirAll(
		filepath.Join(worktreesDir, "gone-child"), 0o755,
	))

	origGuard := probeGitfileTarget
	t.Cleanup(func() { probeGitfileTarget = origGuard })
	probeGitfileTarget = func(cleaned string) bool {
		return export.ClassifyLocalPathProbe("darwin", home, cleaned, false) ==
			export.LocalPathProbeSafe
	}

	deleted := filepath.Join(home, "gone-child", "sub")
	assert.Equal(t, "sub", ExtractProjectFromCwd(deleted),
		"a guarded sibling must not be typed or recovered as the root")
}

// TestDeletedChildIsWorktreeSkipsSymlinkedWorktreesDir pins that verifying a
// deleted worktree vets the exact worktrees path before enumerating it: the
// sibling's .git directory is vetted and real, but worktrees inside it can
// be a symlink into a guarded folder, and ReadDir through it is exactly the
// enumeration macOS gates behind a consent prompt. The guarded store lists
// the deleted child, so a missing vet is caught by the sibling's name being
// recovered.
func TestDeletedChildIsWorktreeSkipsSymlinkedWorktreesDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe classifier walks POSIX paths and symlinks")
	}
	home := t.TempDir()
	store := filepath.Join(home, "Documents", "wt-store")
	require.NoError(t, os.MkdirAll(
		filepath.Join(store, "gone-child"), 0o755,
	))
	parent := filepath.Join(home, "work")
	mainRepo := filepath.Join(parent, "mainrepo")
	require.NoError(t, os.MkdirAll(filepath.Join(mainRepo, ".git"), 0o755))
	require.NoError(t, os.Symlink(
		store, filepath.Join(mainRepo, ".git", "worktrees"),
	))

	origGuard := probeGitfileTarget
	t.Cleanup(func() { probeGitfileTarget = origGuard })
	probeGitfileTarget = func(cleaned string) bool {
		return export.ClassifyLocalPathProbe("darwin", home, cleaned, false) ==
			export.LocalPathProbeSafe
	}

	deleted := filepath.Join(parent, "gone-child", "sub")
	assert.Equal(t, "sub", ExtractProjectFromCwd(deleted),
		"a symlinked worktrees directory must not be enumerated")
}

// TestGitFileTargetsProbeableVetsSubmoduleConfig pins that a gitfile target
// without a commondir — the submodule layout — vets the gitdir's own config
// and HEAD: the conservative result would otherwise escalate to gitMainRoot,
// whose git exec reads them, so a config symlink into a guarded folder would
// be read through despite the vetted gitdir.
func TestGitFileTargetsProbeableVetsSubmoduleConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe classifier walks POSIX paths and symlinks")
	}
	home := t.TempDir()
	gitDir := filepath.Join(home, "src", "parent", ".git", "modules", "sub")
	require.NoError(t, os.MkdirAll(gitDir, 0o755))
	guardedConfig := filepath.Join(home, "Documents", "config-target")
	require.NoError(t, os.MkdirAll(filepath.Dir(guardedConfig), 0o755))
	require.NoError(t, os.WriteFile(guardedConfig, []byte("[core]\n"), 0o644))
	worktree := filepath.Join(home, "src", "parent", "sub")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	gitPath := filepath.Join(worktree, ".git")
	require.NoError(t, os.WriteFile(
		gitPath, []byte("gitdir: "+gitDir+"\n"), 0o644,
	))

	origGuard := probeGitfileTarget
	t.Cleanup(func() { probeGitfileTarget = origGuard })
	probeGitfileTarget = func(cleaned string) bool {
		return export.ClassifyLocalPathProbe("darwin", home, cleaned, false) ==
			export.LocalPathProbeSafe
	}

	require.NoError(t, os.Symlink(
		guardedConfig, filepath.Join(gitDir, "config"),
	))
	assert.False(t, gitFileTargetsProbeable(worktree, gitPath),
		"a guarded config symlink must refuse the submodule gitdir")

	require.NoError(t, os.Remove(filepath.Join(gitDir, "config")))
	require.NoError(t, os.WriteFile(
		filepath.Join(gitDir, "config"), []byte("[core]\n"), 0o644,
	))
	assert.True(t, gitFileTargetsProbeable(worktree, gitPath),
		"a plain submodule gitdir stays probeable")
}

// TestDefaultProbeGitfileTargetRefusesAutomount pins the asymmetry between
// the two default guards on macOS: a literal automount cwd stays probeable
// because isForeignOSPath vetted it with the resolved-autofs probe before
// the guard runs, but a gitfile target under the same namespace was never
// autofs-vetted and must be refused so reading it cannot wake automountd.
func TestDefaultProbeGitfileTargetRefusesAutomount(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the default guards only restrict paths on darwin")
	}
	assert.False(t, defaultProbeGitfileTarget("/home/user/repo/.git"),
		"an automount gitfile target must be refused")
}

// TestAutomountCwdProbeAllowed pins the clearance rules for automount cwds:
// spelling alone is never enough. Clearance requires the canonical spelling,
// an actually resolving autofs first-level probe (or no autofs management at
// all), a non-root path, and — without the opt-in — a path outside the
// network home's own guarded folders.
func TestAutomountCwdProbeAllowed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the guard reads runtime.GOOS")
	}
	origPrefixes := autofsPrefixes
	t.Cleanup(func() { autofsPrefixes = origPrefixes; resetAutofsProbes() })
	autofsPrefixes = []string{"/home/"}
	resetAutofsProbes()
	origStat := osStat
	t.Cleanup(func() { osStat = origStat })
	realInfo, err := os.Stat(t.TempDir())
	require.NoError(t, err)
	osStat = func(path string) (os.FileInfo, error) {
		if path == "/home/user" {
			return realInfo, nil
		}
		return nil, os.ErrNotExist
	}
	t.Setenv("HOME", "/home/user")

	assert.True(t, automountCwdProbeAllowed("/home/user/repo"),
		"a resolving first-level entry clears the walk")
	assert.False(t, automountCwdProbeAllowed("/home/ghost/repo"),
		"an unresolved first-level entry must stay refused")
	assert.False(t, automountCwdProbeAllowed("/home"),
		"the namespace root has no first-level entry to vet")
	assert.False(t, automountCwdProbeAllowed("/System/Volumes/Data/home/user"),
		"the data-volume spelling was never autofs-examined")
	assert.False(t, automountCwdProbeAllowed("/HOME/user/repo"),
		"a case-folded spelling was never autofs-examined")
	assert.False(t, automountCwdProbeAllowed("/home/user/Documents/proj"),
		"a network home's guarded folders stay refused")
	SetAllowProtectedPathProbes(true)
	t.Cleanup(func() { SetAllowProtectedPathProbes(false) })
	assert.True(t, automountCwdProbeAllowed("/home/user/Documents/proj"),
		"the opt-in lifts the guarded-folder refusal for network homes")

	autofsPrefixes = nil
	assert.True(t, automountCwdProbeAllowed("/home/user/repo"),
		"an unmanaged namespace has no automountd to wake")
}

// TestAutomountCwdProbeAllowedCustomPrefix pins the same clearance rules for
// a custom autofs mount discovered from the mount table: registration makes
// it classify as automount, and clearance still requires a resolving
// first-level probe and refuses the mount root.
func TestAutomountCwdProbeAllowedCustomPrefix(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the guard reads runtime.GOOS")
	}
	origRegistered := export.RegisteredAutomountPrefixes()
	t.Cleanup(func() { export.RegisterAutomountPrefixes(origRegistered) })
	export.RegisterAutomountPrefixes([]string{"/corp/home/"})
	origPrefixes := autofsPrefixes
	t.Cleanup(func() { autofsPrefixes = origPrefixes; resetAutofsProbes() })
	autofsPrefixes = []string{"/corp/home/"}
	resetAutofsProbes()
	origStat := osStat
	t.Cleanup(func() { osStat = origStat })
	realInfo, err := os.Stat(t.TempDir())
	require.NoError(t, err)
	osStat = func(path string) (os.FileInfo, error) {
		if path == "/corp/home/user" {
			return realInfo, nil
		}
		return nil, os.ErrNotExist
	}

	assert.True(t, automountCwdProbeAllowed("/corp/home/user/repo"),
		"a resolving custom-mount entry clears the walk")
	assert.False(t, automountCwdProbeAllowed("/corp/home/ghost/repo"),
		"an unresolved custom-mount entry must stay refused")
	assert.False(t, automountCwdProbeAllowed("/corp/home"),
		"the custom mount root has no first-level entry to vet")
}
