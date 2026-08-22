//go:build !windows

package capture

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInvalidateResultRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.json")
	require.NoError(t, os.WriteFile(targetPath, []byte("old result"), 0o600))
	resultPath := filepath.Join(dir, "usage.json")
	require.NoError(t, os.Symlink(targetPath, resultPath))

	err := invalidateResult(resultPath)

	require.ErrorContains(t, err, "not a regular file")
	info, statErr := os.Lstat(resultPath)
	require.NoError(t, statErr)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
	data, readErr := os.ReadFile(targetPath)
	require.NoError(t, readErr)
	assert.Equal(t, "old result", string(data))
}

func TestRunRollsBackNewStateWhenResultCannotBeRemoved(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove files from a read-only directory")
	}
	root := t.TempDir()
	resultParent := filepath.Join(t.TempDir(), "read-only")
	require.NoError(t, os.Mkdir(resultParent, 0o700))
	resultPath := filepath.Join(resultParent, "usage.json")
	require.NoError(t, os.WriteFile(resultPath, []byte("old result"), 0o600))
	require.NoError(t, os.Chmod(resultParent, 0o500))
	t.Cleanup(func() { require.NoError(t, os.Chmod(resultParent, 0o700)) })
	captureDir := filepath.Join(t.TempDir(), "capture")
	producer := copyCaptureHelper(t, "claude")

	_, err := Run(context.Background(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "unlink-failed",
		CaptureDir: captureDir, ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-final", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(),
	})

	require.ErrorContains(t, err, "invalidating existing result")
	assert.NoDirExists(t, captureDir)
	data, readErr := os.ReadFile(resultPath)
	require.NoError(t, readErr)
	assert.Equal(t, "old result", string(data))
}
