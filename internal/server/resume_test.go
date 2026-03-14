package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wesm/agentsview/internal/db"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple uuid", "abc-123-def", "abc-123-def"},
		{"alphanumeric", "session42", "session42"},
		{"with colon", "a:b", "'a:b'"},
		{"with spaces", "has space", "'has space'"},
		{"with single quote", "it's", `'it'"'"'s'`},
		{"command injection attempt", "$(whoami)", "'$(whoami)'"},
		{"backtick injection", "`rm -rf /`", "'`rm -rf /`'"},
		{"semicolon", "id;rm -rf /", "'id;rm -rf /'"},
		{"pipe", "id|cat", "'id|cat'"},
		{"empty passthrough", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellQuote(tt.in)
			if got != tt.want {
				t.Errorf(
					"shellQuote(%q) = %q, want %q",
					tt.in, got, tt.want,
				)
			}
		})
	}
}

func TestDetectTerminalLinux_NoTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only terminal detection")
	}
	// Empty PATH and no $TERMINAL — no terminal should be found.
	t.Setenv("PATH", t.TempDir())
	t.Setenv("TERMINAL", "")
	_, _, _, err := detectTerminalLinux("echo test")
	if err == nil {
		t.Error("expected error with empty PATH, got nil")
	}
}

func TestDetectTerminalLinux_EnvTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only terminal detection")
	}
	// Create a fake terminal binary on PATH.
	binDir := t.TempDir()
	fakeBin := filepath.Join(binDir, "myterm")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("TERMINAL", "myterm")

	bin, args, name, err := detectTerminalLinux("echo hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bin != fakeBin {
		t.Errorf("bin = %q, want %q", bin, fakeBin)
	}
	if name != "myterm" {
		t.Errorf("name = %q, want %q", name, "myterm")
	}
	if len(args) == 0 {
		t.Error("expected non-empty args")
	}
}

func TestDetectTerminalLinux_EnvTerminalWithArgs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only terminal detection")
	}
	binDir := t.TempDir()
	fakeBin := filepath.Join(binDir, "kitty")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv("TERMINAL", "kitty --single-instance")

	bin, args, name, err := detectTerminalLinux("echo hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bin != fakeBin {
		t.Errorf("bin = %q, want %q", bin, fakeBin)
	}
	if name != "kitty" {
		t.Errorf("name = %q, want %q", name, "kitty")
	}
	// Should have --single-instance prepended before template args.
	if len(args) < 2 || args[0] != "--single-instance" {
		t.Errorf("args = %v, want --single-instance as first arg", args)
	}
}

func TestReadSessionCwd_LargeLine(t *testing.T) {
	// Verify that readSessionCwd handles lines larger than the
	// old 2MB scanner limit without losing the cwd field.
	dir := t.TempDir()
	cwdDir := filepath.Join(dir, "project")
	if err := os.Mkdir(cwdDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cwdJSON, _ := json.Marshal(cwdDir)
	// Build a 3MB padding string to exceed the old scanner limit.
	padding := strings.Repeat("x", 3*1024*1024)
	line := `{"cwd":` + string(cwdJSON) +
		`,"big":"` + padding + `"}` + "\n"

	sessionFile := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sessionFile, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readSessionCwd(sessionFile)
	if got != cwdDir {
		t.Errorf("readSessionCwd() = %q, want %q", got, cwdDir)
	}
}

func TestReadSessionCwd_CopilotFormat(t *testing.T) {
	dir := t.TempDir()
	cwdDir := filepath.Join(dir, "project")
	if err := os.Mkdir(cwdDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cwdJSON, _ := json.Marshal(cwdDir)
	line := `{"type":"session.start","data":{"sessionId":"abc","context":{"cwd":` +
		string(cwdJSON) + `}}}` + "\n"

	sessionFile := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(sessionFile, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readSessionCwd(sessionFile)
	if got != cwdDir {
		t.Errorf("readSessionCwd() = %q, want %q", got, cwdDir)
	}
}

func TestResolveSessionDir(t *testing.T) {
	// Create a real temp directory for the "absolute path" cases.
	tmpDir := t.TempDir()

	// Create a session file with a cwd field.
	sessionFile := filepath.Join(tmpDir, "session.jsonl")
	cwdDir := filepath.Join(tmpDir, "project")
	if err := os.Mkdir(cwdDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cwdJSON, _ := json.Marshal(cwdDir)
	content := `{"cwd":` + string(cwdJSON) + `}` + "\n"
	if err := os.WriteFile(sessionFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		session *db.Session
		want    string
	}{
		{
			name: "absolute project path",
			session: &db.Session{
				Project: tmpDir,
			},
			want: tmpDir,
		},
		{
			name: "relative project name returns empty",
			session: &db.Session{
				Project: "my-repo",
			},
			want: "",
		},
		{
			name: "nil file_path with relative project",
			session: &db.Session{
				Project:  "my-repo",
				FilePath: nil,
			},
			want: "",
		},
		{
			name: "file_path with cwd in session file",
			session: &db.Session{
				Project:  "my-repo",
				FilePath: &sessionFile,
			},
			want: cwdDir,
		},
		{
			name: "file_path takes precedence over project",
			session: &db.Session{
				Project:  tmpDir,
				FilePath: &sessionFile,
			},
			want: cwdDir,
		},
		{
			name: "nonexistent file_path falls back to project",
			session: func() *db.Session {
				bad := "/nonexistent/session.jsonl"
				return &db.Session{
					Project:  tmpDir,
					FilePath: &bad,
				}
			}(),
			want: tmpDir,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveSessionDir(tt.session)
			if got != tt.want {
				t.Errorf(
					"resolveSessionDir() = %q, want %q",
					got, tt.want,
				)
			}
		})
	}
}
