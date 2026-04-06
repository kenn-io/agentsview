package server

import (
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestLaunchResumeDarwinGhosttyDirectCli(t *testing.T) {
	cwd := t.TempDir()
	proc := launchResumeDarwin(
		Opener{
			ID:   "ghostty",
			Name: "Ghostty",
			Kind: "terminal",
			Bin:  "/usr/local/bin/ghostty",
		},
		"cursor agent --resume chat-1",
		cwd,
	)
	if proc == nil {
		t.Fatal("launchResumeDarwin returned nil")
	}
	if strings.HasSuffix(proc.Args[0], "osascript") {
		t.Fatalf("expected direct CLI, got osascript: %v",
			proc.Args)
	}
	wantWD := "--working-directory=" + cwd
	if !sliceContains(proc.Args, wantWD) {
		t.Fatalf("missing %q in args: %v", wantWD, proc.Args)
	}
}

func TestLaunchResumeDarwinGhosttyAppBundle(t *testing.T) {
	cwd := t.TempDir()
	proc := launchResumeDarwin(
		Opener{
			ID:   "ghostty",
			Name: "Ghostty",
			Kind: "terminal",
			Bin:  "/Applications/Ghostty.app",
		},
		"cursor agent --resume chat-1",
		cwd,
	)
	if proc == nil {
		t.Fatal("launchResumeDarwin returned nil")
	}
	// App bundle wraps with `open -na`.
	if !strings.HasSuffix(proc.Args[0], "open") {
		t.Fatalf("expected open for app bundle, got %q",
			proc.Args[0])
	}
	if !sliceContains(proc.Args, "-na") {
		t.Fatalf("missing -na flag: %v", proc.Args)
	}
	wantWD := "--working-directory=" + cwd
	if !sliceContains(proc.Args, wantWD) {
		t.Fatalf("missing %q in args: %v", wantWD, proc.Args)
	}
}

func TestLaunchResumeDarwinGhosttyNoCwd(t *testing.T) {
	proc := launchResumeDarwin(
		Opener{
			ID:   "ghostty",
			Name: "Ghostty",
			Kind: "terminal",
			Bin:  "/usr/local/bin/ghostty",
		},
		"cursor agent --resume chat-1",
		"",
	)
	if proc == nil {
		t.Fatal("launchResumeDarwin returned nil")
	}
	for _, arg := range proc.Args {
		if strings.HasPrefix(arg, "--working-directory") {
			t.Fatalf("unexpected --working-directory with empty cwd: %v",
				proc.Args)
		}
	}
}

func TestLaunchTerminalInDirGhosttyDirectCliOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-specific Ghostty launch path")
	}
	dir := t.TempDir()
	proc := launchTerminalInDir(
		Opener{
			ID:   "ghostty",
			Name: "Ghostty",
			Kind: "terminal",
			Bin:  "/Applications/Ghostty.app",
		},
		dir,
	)
	if proc == nil {
		t.Fatal("launchTerminalInDir returned nil")
	}
	if strings.HasSuffix(proc.Args[0], "osascript") {
		t.Fatalf("expected direct launch, got osascript: %v",
			proc.Args)
	}
	wantWD := "--working-directory=" + dir
	if !sliceContains(proc.Args, wantWD) {
		t.Fatalf("missing %q in args: %v", wantWD, proc.Args)
	}
}

func sliceContains(ss []string, s string) bool {
	return slices.Contains(ss, s)
}
