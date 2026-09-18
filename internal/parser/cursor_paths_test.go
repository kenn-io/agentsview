package parser

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

func TestResolveCursorWorkspaceDirInWindowsCaseInsensitive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Cursor workspace matching is case-insensitive on Windows")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "Users", "helix", "Code", "work-area")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	got, ambiguous := ResolveCursorWorkspaceDirIn(
		root, "users-HELIX-code-WORK-AREA",
	)
	assert.False(t, ambiguous)
	assert.Equal(t, normalizeCursorDir(workspace), got)
}

func TestResolveCursorWorkspaceDirDarwinPrivateVarContainment(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Cursor private-var containment is macOS-specific")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "Users", "helix", "Code", "work-area")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	got, ambiguous := ResolveCursorWorkspaceDirIn(
		root, "Users-helix-Code-work-area",
	)
	assert.False(t, ambiguous)
	assert.Equal(t, normalizeCursorDir(workspace), got)
}

func TestResolveCursorWorkspaceDirInUniqueMissingAndAmbiguous(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	workspace := filepath.Join(root, "Users", "helix", "Code", "work-area")
	require.NoError(os.MkdirAll(workspace, 0o755))

	got, ambiguous := ResolveCursorWorkspaceDirIn(root, "Users-helix-Code-work-area")
	assert.False(ambiguous)
	assert.Equal(normalizeCursorDir(workspace), got)

	missing, ambiguous := ResolveCursorWorkspaceDirIn(root, "Users-helix-Code-missing")
	assert.False(ambiguous)
	assert.Empty(missing)

	other := filepath.Join(root, "Users", "helix", "Code-work-area")
	require.NoError(os.MkdirAll(other, 0o755))
	ambiguousPath, ambiguous := ResolveCursorWorkspaceDirIn(root, "Users-helix-Code-work-area")
	assert.True(ambiguous)
	assert.NotEmpty(ambiguousPath)
}

func TestResolveCursorWorkspaceDirInIssue1418Token(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "Users", "helix", "Code", "work-area")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	// The external example cannot be created at the OS root, so resolve its
	// exact token sequence under a test root instead.
	got, ambiguous := ResolveCursorWorkspaceDirIn(root, "Users-helix-Code-work-area")
	assert.False(t, ambiguous)
	assert.Equal(t, normalizeCursorDir(workspace), got)
}

func TestResolveCursorWorkspaceDirHintOnlyDisambiguates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	workspace := filepath.Join(root, "Users", "helix", "Code", "work-area")
	outside := filepath.Join(root, "outside")
	require.NoError(os.MkdirAll(filepath.Join(workspace, "frontend"), 0o755))
	require.NoError(os.MkdirAll(outside, 0o755))

	assert.Equal(normalizeCursorDir(workspace), ResolveCursorWorkspaceDirHint(root,
		"Users-helix-Code-work-area", filepath.Join(workspace, "frontend")))
	assert.Equal(normalizeCursorDir(workspace), ResolveCursorWorkspaceDirHint(root,
		"Users-helix-Code-work-area", outside),
		"a stale hint cannot reject a unique real workspace")
	other := filepath.Join(root, "Users", "helix", "Code-work-area")
	require.NoError(os.MkdirAll(other, 0o755))
	assert.Empty(ResolveCursorWorkspaceDirHint(root,
		"Users-helix-Code-work-area", outside),
		"an outside hint cannot choose among ambiguous matches")
}

func TestResolveCursorWorkspaceDirFiltersNamesBeforeStat(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	workspace := filepath.Join(root, "Users", "helix", "Code", "app")
	unmatched := filepath.Join(root, "unmatched-directory")
	require.NoError(os.MkdirAll(workspace, 0o755))
	require.NoError(os.MkdirAll(unmatched, 0o755))

	originalStat := osStat
	t.Cleanup(func() { osStat = originalStat })
	var statPaths []string
	osStat = func(path string) (os.FileInfo, error) {
		statPaths = append(statPaths, filepath.Clean(path))
		return os.Stat(path)
	}

	originalProbe := probeGitRootForCwd
	t.Cleanup(func() { probeGitRootForCwd = originalProbe })
	var policyPaths []string
	probeGitRootForCwd = func(path string) bool {
		policyPaths = append(policyPaths, filepath.Clean(path))
		return true
	}

	got, ambiguous := ResolveCursorWorkspaceDirIn(root, "Users-helix-Code-app")
	assert.False(ambiguous)
	assert.Equal(normalizeCursorDir(workspace), got)
	assert.NotContains(policyPaths, filepath.Clean(unmatched))
	assert.NotContains(statPaths, filepath.Clean(unmatched))
}

func TestResolveCursorWorkspaceDirHonorsProbePolicy(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "Users", "helix", "Code", "app")
	require.NoError(t, os.MkdirAll(workspace, 0o755))

	originalProbe := probeGitRootForCwd
	t.Cleanup(func() { probeGitRootForCwd = originalProbe })
	blocked := filepath.Join(root, "Users", "helix", "Code")
	probeGitRootForCwd = func(path string) bool {
		return filepath.Clean(path) != filepath.Clean(blocked)
	}

	got, ambiguous := ResolveCursorWorkspaceDirIn(root, "Users-helix-Code-app")
	assert.False(t, ambiguous)
	assert.Empty(t, got)
}

func TestResolveCursorWorkspaceDirAppliesProtectedAndAutomountPolicy(t *testing.T) {
	t.Run("protected candidate", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		root := t.TempDir()
		workspace := filepath.Join(root, "Documents", "app")
		nonmatching := filepath.Join(root, "unmatched-directory")
		require.NoError(os.MkdirAll(workspace, 0o755))
		require.NoError(os.MkdirAll(nonmatching, 0o755))

		originalProbe := probeGitRootForCwd
		t.Cleanup(func() { probeGitRootForCwd = originalProbe })
		var policyPaths []string
		probeGitRootForCwd = func(path string) bool {
			policyPaths = append(policyPaths, filepath.Clean(path))
			return export.ClassifyLocalPathProbe("darwin", root, path, false) == export.LocalPathProbeSafe
		}

		got, ambiguous := ResolveCursorWorkspaceDirIn(root, "Documents-app")
		assert.False(ambiguous)
		assert.Empty(got)
		assert.Contains(policyPaths, filepath.Clean(filepath.Join(root, "Documents")))
		assert.NotContains(policyPaths, filepath.Clean(nonmatching))
	})

	t.Run("automount candidate", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		root := t.TempDir()
		workspace := filepath.Join(root, "app")
		nonmatching := filepath.Join(root, "unmatched-directory")
		require.NoError(os.MkdirAll(workspace, 0o755))
		require.NoError(os.MkdirAll(nonmatching, 0o755))

		originalPrefixes := export.RegisteredAutomountPrefixes()
		t.Cleanup(func() { export.RegisterAutomountPrefixes(originalPrefixes) })
		export.RegisterAutomountPrefixes([]string{root + string(filepath.Separator)})

		originalProbe := probeGitRootForCwd
		t.Cleanup(func() { probeGitRootForCwd = originalProbe })
		var policyPaths []string
		probeGitRootForCwd = func(path string) bool {
			policyPaths = append(policyPaths, filepath.Clean(path))
			return export.ClassifyLocalPathProbe("darwin", root, path, false) == export.LocalPathProbeSafe
		}

		got, ambiguous := ResolveCursorWorkspaceDirIn(root, "app")
		assert.False(ambiguous)
		assert.Empty(got)
		assert.Contains(policyPaths, filepath.Clean(workspace))
		assert.NotContains(policyPaths, filepath.Clean(nonmatching))
	})
}

func TestResolveCursorWorkspaceDirModesAndStates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	workspace := filepath.Join(root, "Users", "helix", "Code", "app")
	require.NoError(os.MkdirAll(workspace, 0o755))

	originalProbe := probeGitRootForCwd
	t.Cleanup(func() { probeGitRootForCwd = originalProbe })
	probeGitRootForCwd = func(string) bool { return false }

	passive := ResolveCursorWorkspaceDirResolution(
		root, "Users-helix-Code-app", "", CursorResolvePassiveDiscovery,
	)
	assert.Equal(SourceCwdUnavailable, passive.State)

	explicit := ResolveCursorWorkspaceDirExplicit(
		root, "Users-helix-Code-app", "",
	)
	assert.Equal(SourceCwdResolved, explicit.State)
	assert.Equal(normalizeCursorDir(workspace), explicit.Path)
	probeGitRootForCwd = func(string) bool { return true }

	missing := ResolveCursorWorkspaceDirResolution(
		root, "Users-helix-Code-missing", "", CursorResolvePassiveDiscovery,
	)
	assert.Equal(SourceCwdNone, missing.State)
	require.NoError(os.MkdirAll(filepath.Join(root, "Users", "helix", "Code-app"), 0o755))
	ambiguous := ResolveCursorWorkspaceDirResolution(
		root, "Users-helix-Code-app", "", CursorResolvePassiveDiscovery,
	)
	assert.Equal(SourceCwdAmbiguous, ambiguous.State)
	require.NoError(os.MkdirAll(filepath.Join(workspace, "src"), 0o755))
	hinted := ResolveCursorWorkspaceDirExplicit(
		root, "Users-helix-Code-app", filepath.Join(workspace, "src"),
	)
	assert.Equal(SourceCwdResolved, hinted.State)
	assert.Equal(normalizeCursorDir(workspace), hinted.Path)
}

func TestResolveCursorWorkspaceDirClassifiesIncompleteTraversal(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	workspace := filepath.Join(root, "Users", "helix", "Code", "app")
	other := filepath.Join(root, "Users", "helix", "Code-app")
	require.NoError(os.MkdirAll(workspace, 0o755))
	require.NoError(os.MkdirAll(other, 0o755))

	originalReadDir := cursorReadDir
	t.Cleanup(func() { cursorReadDir = originalReadDir })
	cursorReadDir = func(path string) ([]os.DirEntry, error) {
		if filepath.Clean(path) == filepath.Clean(filepath.Join(root, "Users", "helix", "Code")) {
			return nil, errors.New("permission denied")
		}
		return os.ReadDir(path)
	}

	resolution := ResolveCursorWorkspaceDirResolution(
		root, "Users-helix-Code-app", "", CursorResolvePassiveDiscovery,
	)
	assert.Equal(SourceCwdUnavailable, resolution.State)
	explicit := ResolveCursorWorkspaceDirExplicit(
		root, "Users-helix-Code-app", workspace,
	)
	assert.Equal(SourceCwdUnavailable, explicit.State,
		"an explicit hint cannot establish uniqueness through an unreadable branch")
	legacy, ambiguous := ResolveCursorWorkspaceDirIn(
		root, "Users-helix-Code-app",
	)
	assert.Empty(legacy)
	assert.False(ambiguous)
	assert.Empty(ResolveCursorWorkspaceDirMatchesIn(
		root, "Users-helix-Code-app", "", 2,
	))
}

func TestResolveCursorWorkspaceDirLimitCountsCanonicalTargets(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "Users", "helix", "Code", "app")
	other := filepath.Join(root, "Users", "helix", "Code_app")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	require.NoError(t, os.MkdirAll(other, 0o755))
	aliasOne := filepath.Join(root, "Users", "helix", "Code-app")
	aliasTwo := filepath.Join(root, "Users", "helix", "Code.app")
	if err := os.Symlink(workspace, aliasOne); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if err := os.Symlink(workspace, aliasTwo); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}

	resolution := ResolveCursorWorkspaceDirResolution(
		root, "Users-helix-Code-app", "", CursorResolvePassiveDiscovery,
	)
	assert.Equal(t, SourceCwdAmbiguous, resolution.State)
}

func TestResolveCursorWorkspaceDirPassiveCachesAcrossCalls(t *testing.T) {
	assert := assert.New(t)

	cursorPassiveResolutionsEnabled = true
	t.Cleanup(func() { cursorPassiveResolutionsEnabled = false })
	resetCursorPassiveResolutions()
	t.Cleanup(resetCursorPassiveResolutions)
	originalReadDir := cursorReadDir
	t.Cleanup(func() { cursorReadDir = originalReadDir })
	readDirCalls := 0
	cursorReadDir = func(string) ([]os.DirEntry, error) {
		readDirCalls++
		return nil, nil
	}

	// On Windows a root-relative walk needs a drive component: without one the
	// separator-only start path is not absolute and resolution reports
	// Unavailable before ever reading a directory.
	dirName := "no-such-cursor-cache-workspace"
	if runtime.GOOS == "windows" {
		dirName = "C-no-such-cursor-cache-workspace"
	}
	first := ResolveCursorWorkspaceDirPassive(dirName)
	assert.Equal(SourceCwdNone, first.State)
	require.Equal(t, 1, readDirCalls)

	second := ResolveCursorWorkspaceDirPassive(dirName)
	assert.Equal(first, second)
	assert.Equal(1, readDirCalls,
		"a cached passive resolution must not walk the filesystem again")

	cursorPassiveResolutions.Lock()
	entry := cursorPassiveResolutions.entries[dirName]
	entry.expiresAt = time.Now().Add(-time.Second)
	cursorPassiveResolutions.entries[dirName] = entry
	cursorPassiveResolutions.Unlock()
	expired := ResolveCursorWorkspaceDirPassive(dirName)
	assert.Equal(first, expired)
	assert.Equal(2, readDirCalls,
		"an expired entry must re-resolve against the filesystem")

	resetCursorPassiveResolutions()
	reset := ResolveCursorWorkspaceDirPassive(dirName)
	assert.Equal(first, reset)
	assert.Equal(3, readDirCalls)
}

func TestResolveCursorWorkspaceDirRootedCallsBypassCache(t *testing.T) {
	require := require.New(t)

	cursorPassiveResolutionsEnabled = true
	t.Cleanup(func() { cursorPassiveResolutionsEnabled = false })
	resetCursorPassiveResolutions()
	t.Cleanup(resetCursorPassiveResolutions)
	root := t.TempDir()
	workspace := filepath.Join(root, "Users", "helix", "Code", "app")
	require.NoError(os.MkdirAll(workspace, 0o755))

	first := ResolveCursorWorkspaceDirResolution(
		root, "Users-helix-Code-app", "", CursorResolvePassiveDiscovery,
	)
	require.Equal(SourceCwdResolved, first.State)
	require.NoError(os.RemoveAll(workspace))
	second := ResolveCursorWorkspaceDirResolution(
		root, "Users-helix-Code-app", "", CursorResolvePassiveDiscovery,
	)
	assert.Equal(t, SourceCwdNone, second.State,
		"rooted resolutions must observe the live filesystem")
}

func TestResolveCursorWorkspaceDirIncompleteDominatesAmbiguity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	first := filepath.Join(root, "Users", "helix", "Code", "app")
	second := filepath.Join(root, "Users", "helix", "Code-app")
	blocked := filepath.Join(root, "Users", "helix", "Code_app")
	require.NoError(os.MkdirAll(first, 0o755))
	require.NoError(os.MkdirAll(second, 0o755))
	require.NoError(os.MkdirAll(blocked, 0o755))

	originalStat := osStat
	t.Cleanup(func() { osStat = originalStat })
	osStat = func(path string) (os.FileInfo, error) {
		if filepath.Clean(path) == filepath.Clean(blocked) {
			return nil, errors.New("permission denied")
		}
		return os.Stat(path)
	}

	resolution := ResolveCursorWorkspaceDirResolution(
		root, "Users-helix-Code-app", "", CursorResolvePassiveDiscovery,
	)
	assert.Equal(SourceCwdUnavailable, resolution.State,
		"an unreadable branch must not let plural matches clear a preserved Cwd")

	explicit := ResolveCursorWorkspaceDirExplicit(
		root, "Users-helix-Code-app", first,
	)
	assert.Equal(SourceCwdUnavailable, explicit.State,
		"a hint cannot pick among matches while traversal is incomplete")
}
