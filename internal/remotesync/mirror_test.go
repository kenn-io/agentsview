package remotesync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMirrorDirDisambiguatesSanitizedCollisions(t *testing.T) {
	a := MirrorDir("/data", "host:8080")
	b := MirrorDir("/data", "host_8080")
	assert.NotEqual(t, a, b)
	assert.True(t, strings.HasPrefix(a, filepath.Join("/data", "remote-mirrors")+string(filepath.Separator)))
	assert.NotContains(t, filepath.Base(a), ":")
}

// diffResult is a MirrorDelta read back into memory.
type diffResult struct {
	Fetch     []string
	Deletions []string
	Total     int
}

// diffManifest compares a whole manifest against the mirror, as a sync does
// while the manifest streams in.
func diffManifest(root string, m Manifest) (result diffResult, err error) {
	diff := newMirrorDiff(root)
	defer func() { err = errors.Join(err, diff.close()) }()
	for _, entry := range m.Files {
		if err := diff.add(entry); err != nil {
			return diffResult{}, err
		}
	}
	delta, err := diff.finish()
	if err != nil {
		return diffResult{}, err
	}
	defer func() { err = errors.Join(err, delta.Close()) }()
	result.Total = delta.Total
	if result.Fetch, err = collectPaths(delta.Fetch); err != nil {
		return diffResult{}, err
	}
	if result.Deletions, err = collectPaths(delta.Deletions); err != nil {
		return diffResult{}, err
	}
	return result, nil
}

func writeMirrorFile(t *testing.T, root, remotePath, content string, mtime time.Time) string {
	t.Helper()

	local, err := safeRemappedRemotePath(root, remotePath)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(local), 0o755))
	require.NoError(t, os.WriteFile(local, []byte(content), 0o644))
	require.NoError(t, os.Chtimes(local, mtime, mtime))
	return local
}

func TestMirrorDiffFetchesNewChangedAndDeletesStale(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 7, 8, 10, 0, 0, 111222333, time.UTC)
	unchanged := "/home/u/.claude/projects/p/unchanged.jsonl"
	changedMtime := "/home/u/.claude/projects/p/newer.jsonl"
	changedSize := "/home/u/.claude/projects/p/grew.jsonl"
	writeMirrorFile(t, root, unchanged, "aa", base)
	writeMirrorFile(t, root, changedMtime, "bb", base)
	writeMirrorFile(t, root, changedSize, "cc", base)
	staleLocal := writeMirrorFile(t, root, "/home/u/.claude/projects/p/stale.jsonl", "dd", base)

	m := Manifest{Files: []ManifestEntry{
		{Path: changedSize, Size: 9, MtimeNS: base.UnixNano()},
		{Path: "/home/u/.claude/projects/p/new.jsonl", Size: 1, MtimeNS: base.UnixNano()},
		{Path: changedMtime, Size: 2, MtimeNS: base.Add(3 * time.Second).UnixNano()},
		{Path: unchanged, Size: 2, MtimeNS: base.UnixNano()},
	}}
	delta, err := diffManifest(root, m)
	require.NoError(t, err)
	assert.Equal(t, []string{
		changedSize,
		"/home/u/.claude/projects/p/new.jsonl",
		changedMtime,
	}, delta.Fetch)
	assert.Equal(t, []string{staleLocal}, delta.Deletions)
	assert.Equal(t, 4, delta.Total)
}

func TestMirrorDiffTruncatesMtimeToMicroseconds(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 7, 8, 10, 0, 0, 111222000, time.UTC)
	path := "/home/u/.claude/projects/p/s.jsonl"
	writeMirrorFile(t, root, path, "aa", base)
	m := Manifest{Files: []ManifestEntry{
		// Same microsecond, different nanosecond remainder: unchanged.
		{Path: path, Size: 2, MtimeNS: base.UnixNano() + 999},
	}}
	delta, err := diffManifest(root, m)
	require.NoError(t, err)
	assert.Empty(t, delta.Fetch)
	assert.Empty(t, delta.Deletions)
}

func TestMirrorDiffEmptyMirrorFetchesEverything(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	m := Manifest{Files: []ManifestEntry{
		{Path: "/home/u/.claude/projects/p/s.jsonl", Size: 1, MtimeNS: 1},
	}}
	delta, err := diffManifest(root, m)
	require.NoError(t, err)
	assert.Len(t, delta.Fetch, 1)
	assert.Empty(t, delta.Deletions)
}

func TestMirrorDiffWalksTheMirrorInManifestOrder(t *testing.T) {
	root := t.TempDir()
	mtime := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	// "proj-b" sorts before "proj/" byte by byte, but a directory walk
	// visits "proj" first. Every file here is unchanged.
	paths := []string{
		"/home/u/.claude/projects/proj-b/s.jsonl",
		"/home/u/.claude/projects/proj/s.jsonl",
		"/home/u/.claude/projects/proj/sub/t.jsonl",
	}
	m := Manifest{}
	for _, path := range paths {
		writeMirrorFile(t, root, path, "x", mtime)
		m.Files = append(m.Files, ManifestEntry{Path: path, Size: 1, MtimeNS: mtime.UnixNano()})
	}

	delta, err := diffManifest(root, m)

	require.NoError(t, err)
	assert.Empty(t, delta.Fetch)
	assert.Empty(t, delta.Deletions)
	assert.Equal(t, 3, delta.Total)
}

func TestMirrorDiffRejectsAManifestOutOfOrder(t *testing.T) {
	m := Manifest{Files: []ManifestEntry{
		{Path: "/home/u/.claude/projects/proj/s.jsonl", Size: 1, MtimeNS: 1},
		{Path: "/home/u/.claude/projects/proj-b/s.jsonl", Size: 1, MtimeNS: 1},
	}}

	_, err := diffManifest(t.TempDir(), m)

	require.ErrorContains(t, err, "not sorted by mirror path")
	assert.Equal(t, "remote sync failed: the remote host runs an older "+
		"agentsview that lists its files in a different order; upgrade "+
		"agentsview on the remote host", FailureSummary(err))
}

func TestMirrorDiffNamesTheDataDirectoryWhenItsListCannotSpill(t *testing.T) {
	// The mirror's parent does not exist, so the list's temporary file cannot
	// be created once the list passes its memory limit.
	root := filepath.Join(t.TempDir(), "missing", "mirror")
	m := Manifest{}
	for i := range mirrorListMemoryBytes/4096 + 1 {
		name := fmt.Sprintf("/home/u/new/%08d-", i)
		m.Files = append(m.Files, ManifestEntry{
			Path: name + strings.Repeat("a", 4096-len(name)), Size: 1, MtimeNS: 1,
		})
	}

	_, err := diffManifest(root, m)

	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, "remote sync failed: could not write a temporary change "+
		"list next to the local mirror; check that the data directory is "+
		"writable and has free space", FailureSummary(err))
}

func TestApplyMirrorDeletionsConfinedToRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.txt")
	require.NoError(t, os.WriteFile(outside, []byte("x"), 0o644))
	err := ApplyMirrorDeletions(root, []string{outside})
	require.Error(t, err)
	assert.FileExists(t, outside)

	inside := writeMirrorFile(t, root, "/home/u/.claude/projects/p/s.jsonl", "x",
		time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC))
	require.NoError(t, ApplyMirrorDeletions(root, []string{inside}))
	assert.NoFileExists(t, inside)
}

func TestApplyMirrorDeletionsPrunesEmptyDirs(t *testing.T) {
	root := t.TempDir()
	mtime := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	inside := writeMirrorFile(t,
		root, "/home/u/.claude/projects/p/nested/s.jsonl", "x", mtime)
	sibling := writeMirrorFile(t,
		root, "/home/u/.claude/projects/other.jsonl", "y", mtime)

	require.NoError(t, ApplyMirrorDeletions(root, []string{inside}))

	assert.NoDirExists(t, filepath.Dir(inside))
	assert.NoDirExists(t, filepath.Dir(filepath.Dir(inside)))
	assert.FileExists(t, sibling)
	assert.DirExists(t, filepath.Dir(sibling))
}

func TestAcquireMirrorLockIsExclusive(t *testing.T) {
	tests := map[string]func(t *testing.T, base string) (root, contender string){
		"same path": func(_ *testing.T, base string) (string, string) {
			root := filepath.Join(base, "mirror")
			return root, root
		},
		// Symlink aliases must contend on one canonical lock.
		"symlinked parent": func(t *testing.T, base string) (string, string) {
			t.Helper()
			realParent := filepath.Join(base, "real")
			require.NoError(t, os.MkdirAll(realParent, 0o755))
			linkParent := filepath.Join(base, "link")
			if err := os.Symlink(realParent, linkParent); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return filepath.Join(realParent, "data", "remote-mirrors", "shared"),
				filepath.Join(linkParent, "data", "remote-mirrors", "shared")
		},
	}
	for name, paths := range tests {
		t.Run(name, func(t *testing.T) {
			root, contender := paths(t, t.TempDir())
			lock, err := AcquireMirrorLock(t.Context(), root)
			require.NoError(t, err)
			t.Cleanup(func() { _ = lock.Close() })

			assertMirrorLocked(t, contender)

			require.NoError(t, lock.Close())
			second, err := AcquireMirrorLock(t.Context(), contender)
			require.NoError(t, err)
			require.NoError(t, second.Close())
		})
	}
}

func TestRemoveMirrorTypeConflictsRemovesDirAtFetchPath(t *testing.T) {
	root := t.TempDir()
	wedged := "/archives/claude/projects/p/s.jsonl"
	local, err := safeRemappedRemotePath(root, wedged)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(local, 0o755))

	kept := writeMirrorFile(t, root, "/home/u/.claude/projects/p/other.jsonl",
		"x", time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC))

	require.NoError(t, RemoveMirrorTypeConflicts(root, []string{
		wedged,
		"/home/u/.claude/projects/p/other.jsonl",
		"/home/u/.claude/projects/p/absent.jsonl",
	}))
	assert.NoDirExists(t, local)
	assert.FileExists(t, kept)
}
