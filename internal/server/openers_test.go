package server

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunchOpenerSurvivesRequestCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses a POSIX executable script")
	}
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	completed := filepath.Join(dir, "completed")
	bin := filepath.Join(dir, "editor")
	require.NoError(t, exec.CommandContext(t.Context(), "mkfifo", release).Run())
	require.NoError(t, os.WriteFile(bin, []byte(`#!/bin/sh
IFS= read -r _ < "$1/release"
printf completed > "$1/completed"
`), 0o755))
	releaseFile, err := os.OpenFile(release, os.O_RDWR, 0o600)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = releaseFile.WriteString("\n")
		_ = releaseFile.Close()
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.NoError(t, launchOpener(ctx, Opener{Kind: "editor", Bin: bin}, dir))
	cancel()
	_, err = releaseFile.WriteString("release\n")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := os.Stat(completed)
		return err == nil
	}, 5*time.Second, 5*time.Millisecond)
}

func TestLaunchOpenerRejectsCancelledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(t, launchOpener(ctx, Opener{Kind: "editor", Bin: "unused"}, t.TempDir()), context.Canceled)
}
