package remotesync

import (
	"context"
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
		{Path: unchanged, Size: 2, MtimeNS: base.UnixNano()},
		{Path: changedMtime, Size: 2, MtimeNS: base.Add(3 * time.Second).UnixNano()},
		{Path: changedSize, Size: 9, MtimeNS: base.UnixNano()},
		{Path: "/home/u/.claude/projects/p/new.jsonl", Size: 1, MtimeNS: base.UnixNano()},
	}}
	delta, err := MirrorDiff(root, m)
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
	delta, err := MirrorDiff(root, m)
	require.NoError(t, err)
	assert.Empty(t, delta.Fetch)
	assert.Empty(t, delta.Deletions)
}

func TestMirrorDiffEmptyMirrorFetchesEverything(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	m := Manifest{Files: []ManifestEntry{
		{Path: "/home/u/.claude/projects/p/s.jsonl", Size: 1, MtimeNS: 1},
	}}
	delta, err := MirrorDiff(root, m)
	require.NoError(t, err)
	assert.Len(t, delta.Fetch, 1)
	assert.Empty(t, delta.Deletions)
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
	root := filepath.Join(t.TempDir(), "mirror")
	lock, err := AcquireMirrorLock(context.Background(), root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = AcquireMirrorLock(ctx, root)
	require.Error(t, err)

	require.NoError(t, lock.Close())
	second, err := AcquireMirrorLock(context.Background(), root)
	require.NoError(t, err)
	require.NoError(t, second.Close())
}

func TestAcquireMirrorLockCanonicalizesSymlinkedParent(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real")
	require.NoError(t, os.MkdirAll(realParent, 0o755))
	linkParent := filepath.Join(base, "link")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	realRoot := filepath.Join(realParent, "data", "remote-mirrors", "shared")
	aliasRoot := filepath.Join(linkParent, "data", "remote-mirrors", "shared")
	lock, err := AcquireMirrorLock(context.Background(), realRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	aliasLock, err := AcquireMirrorLock(ctx, aliasRoot)
	cancel()
	if aliasLock != nil {
		require.NoError(t, aliasLock.Close())
	}
	assert.Error(t, err, "symlink aliases must contend on one canonical lock")

	require.NoError(t, lock.Close())
	aliasLock, err = AcquireMirrorLock(context.Background(), aliasRoot)
	require.NoError(t, err)
	require.NoError(t, aliasLock.Close())
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
