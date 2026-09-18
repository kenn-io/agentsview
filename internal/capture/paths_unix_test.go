//go:build !windows

package capture

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInvalidateResultRejectsSymlink(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.json")
	require.NoError(os.WriteFile(targetPath, []byte("old result"), 0o600))
	resultPath := filepath.Join(dir, "usage.json")
	require.NoError(os.Symlink(targetPath, resultPath))

	err := invalidateResult(resultPath)

	require.ErrorContains(err, "not a regular file")
	info, statErr := os.Lstat(resultPath)
	require.NoError(statErr)
	assert.NotZero(info.Mode() & os.ModeSymlink)
	data, readErr := os.ReadFile(targetPath)
	require.NoError(readErr)
	assert.Equal("old result", string(data))
}

func TestRunRollsBackNewStateWhenResultCannotBeRemoved(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	if os.Geteuid() == 0 {
		t.Skip("root can remove files from a read-only directory")
	}
	root := t.TempDir()
	resultParent := filepath.Join(t.TempDir(), "read-only")
	require.NoError(os.Mkdir(resultParent, 0o700))
	resultPath := filepath.Join(resultParent, "usage.json")
	require.NoError(os.WriteFile(resultPath, []byte("old result"), 0o600))
	require.NoError(os.Chmod(resultParent, 0o500))
	t.Cleanup(func() { require.NoError(os.Chmod(resultParent, 0o700)) })
	captureDir := filepath.Join(t.TempDir(), "capture")
	producer := copyCaptureHelper(t, "claude")

	_, err := Run(t.Context(), RunOptions{
		Provider: ProviderClaude, OccurrenceID: "unlink-failed",
		CaptureDir: captureDir, ResultPath: resultPath,
		ProviderRoot: root, WorkDir: t.TempDir(),
		Command:     []string{producer, "-p", "prompt"},
		Environment: helperEnvironment(root, "claude-final", 0),
		Streams:     Streams{Stdout: io.Discard, Stderr: io.Discard},
		Limits:      testLimits(),
	})

	require.ErrorContains(err, "invalidating existing result")
	assert.NoDirExists(captureDir)
	data, readErr := os.ReadFile(resultPath)
	require.NoError(readErr)
	assert.Equal("old result", string(data))
}
