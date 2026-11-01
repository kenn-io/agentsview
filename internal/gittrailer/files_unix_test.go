//go:build unix

package gittrailer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStampRejectsSymlinksAndFIFOs(t *testing.T) {
	repo := trailerRepo(t)
	target := messageFile(t, repo, "Subject\n")
	link := filepath.Join(repo, "link")
	require.NoError(t, os.Symlink(target, link))
	require.Error(t, Stamp(t.Context(), link, nil, Session{"pi", "one"}))
	assert.Equal(t, "Subject\n", messageText(t, target))
	fifo := filepath.Join(repo, "fifo")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	require.Error(t, Stamp(t.Context(), fifo, nil, Session{"pi", "one"}))
	hookDir := t.TempDir()
	require.NoError(t, os.Symlink(target, filepath.Join(hookDir, "commit-msg")))
	require.Error(t, Install(hookDir))
	assert.NoFileExists(t, filepath.Join(hookDir, "prepare-commit-msg"))
	assert.Equal(t, "Subject\n", messageText(t, target))
}

func TestStampCancellationPreservesMessage(t *testing.T) {
	repo := trailerRepo(t)
	path := messageFile(t, repo, "Subject\n")
	bin := t.TempDir()
	record := filepath.Join(repo, "pid")
	script := "#!/bin/sh\nprintf '%s' \"$$\" > \"$PID_RECORD\"\n/bin/sleep 30 &\nwait\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755))
	t.Setenv("PATH", bin)
	t.Setenv("PID_RECORD", record)
	// A real hung Git process exercises the production timeout and group kill.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.Error(t, Stamp(ctx, path, nil, Session{"pi", "one"}))
	assert.Equal(t, "Subject\n", messageText(t, path))
	pidBytes, err := os.ReadFile(record)
	require.NoError(t, err)
	assert.NotEmpty(t, strings.TrimSpace(string(pidBytes)))
}

func TestTrailerWritesPreserveSpecialModeBits(t *testing.T) {
	repo := trailerRepo(t)
	path := messageFile(t, repo, "Subject\n")
	mode := os.FileMode(0o751) | os.ModeSticky
	require.NoError(t, os.Chmod(path, mode))
	require.NoError(t, Stamp(t.Context(), path, nil, Session{"pi", "one"}))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, mode, info.Mode())
	hooks := t.TempDir()
	hook := filepath.Join(hooks, "commit-msg")
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o751))
	require.NoError(t, os.Chmod(hook, mode))
	require.NoError(t, Install(hooks))
	info, err = os.Stat(hook)
	require.NoError(t, err)
	assert.Equal(t, mode, info.Mode())
	require.NoError(t, Uninstall(hooks))
	info, err = os.Stat(hook)
	require.NoError(t, err)
	assert.Equal(t, mode, info.Mode())
}

func TestFailedAtomicMessageWritePreservesOriginal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	repo := trailerRepo(t)
	path := messageFile(t, repo, "Subject\n")
	require.NoError(t, os.Chmod(repo, 0o500))
	t.Cleanup(func() { require.NoError(t, os.Chmod(repo, 0o700)) })
	require.Error(t, Stamp(t.Context(), path, nil, Session{"pi", "one"}))
	assert.Equal(t, "Subject\n", messageText(t, path))
}

func TestFailedPrepareStateWriteCannotReuseEarlierContext(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	repo, hooks, path := managedHookFixture(t)
	t.Setenv("AV_PREVIOUS_HOOK", filepath.Join(hooks, "prepare-commit-msg"))
	t.Setenv("AV_PREVIOUS_MESSAGE", path)
	previous := exec.CommandContext(t.Context(), "/bin/sh", "-c", "\"$AV_PREVIOUS_HOOK\" \"$AV_PREVIOUS_MESSAGE\" editor; :")
	out, err := previous.CombinedOutput()
	require.NoError(t, err, string(out))
	require.NoError(t, os.Chmod(repo, 0o500))
	t.Cleanup(func() { require.NoError(t, os.Chmod(repo, 0o700)) })
	runManagedHook(t, hooks, "prepare-commit-msg", path, "template")
	require.NoError(t, os.Chmod(repo, 0o700))
	runManagedHook(t, hooks, "commit-msg", path)
	assert.Equal(t, "Describe the change\n", messageText(t, path))
}
