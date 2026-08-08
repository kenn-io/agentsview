package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
