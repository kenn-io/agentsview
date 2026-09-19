package server

import (
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunchResumeDarwinGhosttyDirectCli(t *testing.T) {
	cwd := t.TempDir()
	proc := launchResumeDarwin(t.Context(),
		Opener{
			ID:   "ghostty",
			Name: "Ghostty",
			Kind: "terminal",
			Bin:  "/usr/local/bin/ghostty",
		},
		"cursor agent --resume chat-1",
		cwd,
	)
	require.NotNil(t, proc, "launchResumeDarwin returned nil")
	assert.False(t, strings.HasSuffix(proc.Args[0], "osascript"),
		"expected direct CLI, got osascript: %v", proc.Args)
	wantWD := "--working-directory=" + cwd
	assert.True(t, sliceContains(proc.Args, wantWD),
		"missing %q in args: %v", wantWD, proc.Args)
}

func TestLaunchResumeDarwinGhosttyAppBundle(t *testing.T) {
	cwd := t.TempDir()
	proc := launchResumeDarwin(t.Context(),
		Opener{
			ID:   "ghostty",
			Name: "Ghostty",
			Kind: "terminal",
			Bin:  "/Applications/Ghostty.app",
		},
		"cursor agent --resume chat-1",
		cwd,
	)
	require.NotNil(t, proc, "launchResumeDarwin returned nil")
	// App bundle wraps with `open -na`.
	assert.True(t, strings.HasSuffix(proc.Args[0], "open"),
		"expected open for app bundle, got %q", proc.Args[0])
	assert.True(t, sliceContains(proc.Args, "-na"),
		"missing -na flag: %v", proc.Args)
	wantWD := "--working-directory=" + cwd
	assert.True(t, sliceContains(proc.Args, wantWD),
		"missing %q in args: %v", wantWD, proc.Args)
}

func TestLaunchResumeDarwinGhosttyNoCwd(t *testing.T) {
	proc := launchResumeDarwin(t.Context(),
		Opener{
			ID:   "ghostty",
			Name: "Ghostty",
			Kind: "terminal",
			Bin:  "/usr/local/bin/ghostty",
		},
		"cursor agent --resume chat-1",
		"",
	)
	require.NotNil(t, proc, "launchResumeDarwin returned nil")
	for _, arg := range proc.Args {
		assert.False(t, strings.HasPrefix(arg, "--working-directory"),
			"unexpected --working-directory with empty cwd: %v", proc.Args)
	}
}

func TestLaunchTerminalInDirGhosttyDirectCliOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-specific Ghostty launch path")
	}
	dir := t.TempDir()
	proc := launchTerminalInDir(t.Context(),
		Opener{
			ID:   "ghostty",
			Name: "Ghostty",
			Kind: "terminal",
			Bin:  "/Applications/Ghostty.app",
		},
		dir,
	)
	require.NotNil(t, proc, "launchTerminalInDir returned nil")
	assert.False(t, strings.HasSuffix(proc.Args[0], "osascript"),
		"expected direct launch, got osascript: %v", proc.Args)
	wantWD := "--working-directory=" + dir
	assert.True(t, sliceContains(proc.Args, wantWD),
		"missing %q in args: %v", wantWD, proc.Args)
}

func sliceContains(ss []string, s string) bool {
	return slices.Contains(ss, s)
}
