//go:build !windows

package remotesync

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestWriteArchiveSkipsFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))

	done := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		done <- WriteArchive(t.Context(), &buf, TargetSet{ExtraFiles: []string{fifo}})
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(backgroundWaitTimeout):
		require.FailNow(t, "WriteArchive blocked on FIFO")
	}
}

func TestUnreadableArchiveMembersAndChildDirsAreSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}

	root := t.TempDir()
	healthyDir := filepath.Join(root, "-proj-a")
	unreadableFileDir := filepath.Join(root, "-proj-b", "memory")
	lockedSubdir := filepath.Join(root, "-proj-locked")
	for _, dir := range []string{healthyDir, unreadableFileDir, lockedSubdir} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	healthy := filepath.Join(healthyDir, "session.jsonl")
	secretFile := filepath.Join(unreadableFileDir, "dysync-troubleshooting.md")
	lockedChild := filepath.Join(lockedSubdir, "secret.jsonl")
	extraUnreadable := filepath.Join(root, "extra-unreadable.jsonl")
	require.NoError(t, os.WriteFile(healthy, []byte("healthy session"), 0o644))
	require.NoError(t, os.WriteFile(secretFile, []byte("root-only note"), 0o644))
	require.NoError(t, os.WriteFile(lockedChild, []byte("locked child"), 0o644))
	require.NoError(t, os.WriteFile(extraUnreadable, []byte("locked extra"), 0o644))

	require.NoError(t, os.Chmod(secretFile, 0o000))
	t.Cleanup(func() { _ = os.Chmod(secretFile, 0o644) })
	require.NoError(t, os.Chmod(extraUnreadable, 0o000))
	t.Cleanup(func() { _ = os.Chmod(extraUnreadable, 0o644) })
	require.NoError(t, os.Chmod(lockedSubdir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(lockedSubdir, 0o755) })

	targets := TargetSet{
		Dirs:       map[parser.AgentType][]string{parser.AgentClaude: {root}},
		ExtraFiles: []string{extraUnreadable},
	}

	manifest, err := BuildManifest(t.Context(), targets)
	require.NoError(t, err)
	assert.Equal(t, []string{healthy}, manifestPaths(manifest))

	var fullBuf bytes.Buffer
	require.NoError(t, WriteArchive(t.Context(), &fullBuf, targets))
	fullEntries := archiveEntries(t, fullBuf.Bytes())
	assert.Equal(t, []byte("healthy session"), fullEntries[archiveNameForTest(t, healthy)])
	assert.NotContains(t, fullEntries, archiveNameForTest(t, secretFile))
	assert.NotContains(t, fullEntries, archiveNameForTest(t, lockedChild))
	assert.NotContains(t, fullEntries, archiveNameForTest(t, extraUnreadable))

	var mixedDelta bytes.Buffer
	require.NoError(t, WriteArchiveFiles(t.Context(), &mixedDelta, targets, []string{
		secretFile, lockedChild, extraUnreadable, healthy,
	}))
	assert.Equal(t, map[string][]byte{
		archiveNameForTest(t, healthy): []byte("healthy session"),
	}, archiveEntries(t, mixedDelta.Bytes()))

	var onlyUnreadableDelta bytes.Buffer
	require.NoError(t, WriteArchiveFiles(t.Context(), &onlyUnreadableDelta, targets, []string{secretFile}))
	assert.Empty(t, archiveEntries(t, onlyUnreadableDelta.Bytes()))

	// Restoring readability surfaces the file again in manifest and delta archives.
	require.NoError(t, os.Chmod(secretFile, 0o644))
	recoveredManifest, err := BuildManifest(t.Context(), targets)
	require.NoError(t, err)
	assert.Equal(t, []string{healthy, secretFile}, manifestPaths(recoveredManifest))

	var recoveredDelta bytes.Buffer
	require.NoError(t, WriteArchiveFiles(t.Context(), &recoveredDelta, targets, []string{secretFile}))
	assert.Equal(t, map[string][]byte{
		archiveNameForTest(t, secretFile): []byte("root-only note"),
	}, archiveEntries(t, recoveredDelta.Bytes()))
}

func TestBuildManifestAndWriteArchiveFailOnUnreadableRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "session.jsonl"), []byte("x"), 0o644))
	require.NoError(t, os.Chmod(root, 0o000))
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	targets := TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}}

	_, err := BuildManifest(t.Context(), targets)
	require.ErrorIs(t, err, os.ErrPermission)

	var buf bytes.Buffer
	err = WriteArchive(t.Context(), &buf, targets)
	require.ErrorIs(t, err, os.ErrPermission)
}
