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
		return export.ClassifyLocalPathProbe("darwin", home, cleaned) ==
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
	assert.True(t, defaultProbeGitRootForCwd("/home/user/repo"),
		"a literal automount cwd defers to isForeignOSPath's autofs probe")
}
