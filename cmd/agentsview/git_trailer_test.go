package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	trailerID   = "12345678-1234-1234-1234-1234567890ab"
	trailerLine = "Agent-Session: codex:" + trailerID
)

// Reuse the test executable, as main_test.go does, rather than building in tests.
func TestGitTrailerCLIProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_GIT_TEST_CHILD") != "1" {
		return
	}
	index := slices.Index(os.Args, "--")
	if index < 0 {
		os.Exit(2)
	}
	os.Args = append([]string{"agentsview"}, os.Args[index+1:]...)
	main()
	os.Exit(0)
}

type gitTrailerFixture struct {
	repo, bin string
	env       []string
}

func newGitTrailerFixture(t *testing.T) *gitTrailerFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test executable shell wrapper requires Unix")
	}
	root := t.TempDir()
	f := &gitTrailerFixture{repo: filepath.Join(root, "repo"), bin: filepath.Join(root, "bin", "agentsview")}
	require.NoError(t, os.Mkdir(f.repo, 0o700))
	require.NoError(t, os.Mkdir(filepath.Dir(f.bin), 0o700))
	exe, err := os.Executable()
	require.NoError(t, err)
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	require.NoError(t, os.WriteFile(f.bin, []byte("#!/bin/sh\nexec "+quote(exe)+" -test.run=^TestGitTrailerCLIProcess$ -- \"$@\"\n"), 0o755))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "AGENTSVIEW_") || key == "CLAUDE_CODE_SESSION_ID" || key == "CODEX_THREAD_ID" || key == "CODEX_SESSION_ID" {
			continue
		}
		f.env = append(f.env, entry)
	}
	for key, value := range map[string]string{
		"PATH":                filepath.Dir(f.bin) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": filepath.Join(root, "gitconfig"),
		"AGENTSVIEW_DATA_DIR": filepath.Join(root, "data"), "AGENTSVIEW_GIT_TEST_CHILD": "1",
		"CODEX_THREAD_ID": trailerID,
	} {
		f.setenv(key, value)
	}
	f.git(t, "init", "-q", "-b", "main")
	f.git(t, "config", "user.name", "Example")
	f.git(t, "config", "user.email", "example@example.com")
	f.git(t, "config", "commit.gpgsign", "false")
	return f
}

func (f *gitTrailerFixture) setenv(key, value string) {
	f.env = slices.DeleteFunc(f.env, func(s string) bool { return strings.HasPrefix(s, key+"=") })
	f.env = append(f.env, key+"="+value)
}

func (f *gitTrailerFixture) run(t *testing.T, exe string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), exe, args...)
	cmd.Dir, cmd.Env = f.repo, f.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f *gitTrailerFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	out, err := f.run(t, "git", args...)
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(out)
}

func (f *gitTrailerFixture) hook(t *testing.T, action string) {
	t.Helper()
	out, err := f.run(t, f.bin, "git-hook", action)
	require.NoError(t, err, out)
}

func TestGitTrailerCommits(t *testing.T) {
	for _, tt := range []struct {
		name    string
		args    []string
		cleanup string
		editor  bool
		abort   bool
		want    string
	}{
		{name: "issue subject", args: []string{"-m", "#123 fix the parser"}, want: "#123 fix the parser"},
		{name: "amend subject", args: []string{"-m", "amend! fix the parser"}, want: "amend! fix the parser\n\n" + trailerLine},
		{name: "file", args: []string{"-F", "message"}, want: "from file\n\n" + trailerLine},
		{name: "empty", args: []string{"-m", ""}, abort: true},
		{name: "empty signoff", args: []string{"-s", "-m", ""}, abort: true},
		{name: "stripped comment", args: []string{"-m", "# wip"}, cleanup: "strip", abort: true},
		{name: "command-line stripped comment", args: []string{"--cleanup=strip", "-m", "# wip"}, abort: true},
		{name: "unedited template", abort: true},
		{name: "editor", editor: true, want: "from editor"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newGitTrailerFixture(t)
			f.hook(t, "install")
			f.setenv("GIT_EDITOR", ":")
			require.NoError(t, os.WriteFile(filepath.Join(f.repo, "message"), []byte("from file\n"), 0o600))
			if tt.cleanup != "" {
				f.git(t, "config", "commit.cleanup", tt.cleanup)
			}
			if tt.editor {
				editor := filepath.Join(filepath.Dir(f.bin), "editor")
				require.NoError(t, os.WriteFile(editor, []byte("#!/bin/sh\nprintf 'from editor\\n' > \"$1\"\n"), 0o755))
				f.setenv("GIT_EDITOR", editor)
			}
			out, err := f.run(t, "git", append([]string{"commit", "--allow-empty"}, tt.args...)...)
			if tt.abort {
				require.Error(t, err, out)
				assert.Contains(t, out, "empty commit message")
				message, readErr := os.ReadFile(filepath.Join(f.repo, ".git", "COMMIT_EDITMSG"))
				require.NoError(t, readErr)
				assert.NotContains(t, string(message), "Agent-Session:")
				return
			}
			require.NoError(t, err, out)
			assert.Equal(t, tt.want, f.git(t, "log", "-1", "--format=%B"))
		})
	}
}

func TestGitTrailerAmend(t *testing.T) {
	f := newGitTrailerFixture(t)
	f.hook(t, "install")
	f.git(t, "commit", "--allow-empty", "-m", "first")
	f.git(t, "commit", "--amend", "--no-edit", "--allow-empty")
	assert.Equal(t, "first\n\n"+trailerLine, f.git(t, "log", "-1", "--format=%B"))
	f.setenv("CODEX_THREAD_ID", "abcdefab-1234-1234-1234-1234567890ab")
	f.git(t, "commit", "--amend", "--no-edit", "--allow-empty")
	assert.Equal(t, "first\n\n"+trailerLine+"\nAgent-Session: codex:abcdefab-1234-1234-1234-1234567890ab", f.git(t, "log", "-1", "--format=%B"))
	f.git(t, "commit", "--allow-empty", "-C", "HEAD")
	assert.Equal(t, 2, strings.Count(f.git(t, "log", "-1", "--format=%B"), "Agent-Session:"))
}

func TestGitTrailerUnavailable(t *testing.T) {
	for _, state := range []string{"missing", "failure", "no session", "invalid", "ambiguous"} {
		t.Run(state, func(t *testing.T) {
			f := newGitTrailerFixture(t)
			f.hook(t, "install")
			switch state {
			case "missing":
				require.NoError(t, os.Remove(f.bin))
				// Restrict PATH to the fixture and Git so an installed agentsview cannot run.
				git, err := exec.LookPath("git")
				require.NoError(t, err)
				require.NoError(t, os.Symlink(git, filepath.Join(filepath.Dir(f.bin), "git")))
				f.setenv("PATH", filepath.Dir(f.bin))
			case "failure":
				require.NoError(t, os.WriteFile(f.bin, []byte("#!/bin/sh\nexit 42\n"), 0o755))
			case "no session":
				f.setenv("CODEX_THREAD_ID", "")
			case "invalid":
				f.setenv("CODEX_THREAD_ID", "bad")
			case "ambiguous":
				f.setenv("CLAUDE_CODE_SESSION_ID", trailerID)
			}
			out, err := f.run(t, "git", "commit", "--allow-empty", "-m", "unchanged")
			require.NoError(t, err, out)
			if state == "ambiguous" {
				assert.Contains(t, out, "warning: ambiguous current session")
			}
			assert.Equal(t, "unchanged", f.git(t, "log", "-1", "--format=%B"))
		})
	}
}

func TestGitHookInstall(t *testing.T) {
	for _, hooksDir := range []string{".git/hooks", "custom-hooks", "custom-hooks "} {
		t.Run(hooksDir, func(t *testing.T) {
			f := newGitTrailerFixture(t)
			if hooksDir != ".git/hooks" {
				f.git(t, "config", "core.hooksPath", hooksDir)
			}
			path := filepath.Join(f.repo, hooksDir, "commit-msg")
			f.hook(t, "install")
			f.git(t, "commit", "--allow-empty", "-m", "installed hook")
			assert.Equal(t, "installed hook\n\n"+trailerLine, f.git(t, "log", "-1", "--format=%B"))
			original, err := os.ReadFile(path)
			require.NoError(t, err)
			out, err := f.run(t, f.bin, "git-hook", "install")
			require.Error(t, err)
			assert.Contains(t, out, `if command -v agentsview >/dev/null 2>&1; then agentsview git-trailer "$1" || :; fi`)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, original, data)
			_, line, ok := strings.Cut(out, "add this line to it:\n")
			require.True(t, ok)
			require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+strings.TrimSpace(line)+"\nprintf 'later check\\n' >&2\nexit 42\n"), 0o755))
			require.NoError(t, os.Remove(f.bin))
			f.setenv("PATH", filepath.Dir(f.bin))
			out, err = f.run(t, "git", "commit", "--allow-empty", "-m", "rejected by later check")
			require.Error(t, err, out)
			assert.Contains(t, out, "later check")
		})
	}
}

func TestGitTrailerRebasePick(t *testing.T) {
	f := newGitTrailerFixture(t)
	f.git(t, "commit", "--allow-empty", "-m", "base")
	f.git(t, "checkout", "-b", "topic")
	require.NoError(t, os.WriteFile(filepath.Join(f.repo, "topic"), []byte("topic"), 0o600))
	f.git(t, "add", "topic")
	f.git(t, "commit", "-m", "picked")
	f.git(t, "checkout", "main")
	require.NoError(t, os.WriteFile(filepath.Join(f.repo, "main"), []byte("main"), 0o600))
	f.git(t, "add", "main")
	f.git(t, "commit", "-m", "advance")
	f.hook(t, "install")
	f.git(t, "rebase", "main", "topic")
	assert.Equal(t, "picked", f.git(t, "log", "-1", "--format=%B"))
}

func TestGitTrailerAvoidsStartup(t *testing.T) {
	f := newGitTrailerFixture(t)
	f.setenv("TMPDIR", filepath.Join(f.repo, "missing-temp"))
	f.setenv("GIT_EDITOR", ":")
	out, err := f.run(t, f.bin, "git-trailer", "missing-message")
	require.NoError(t, err, out)
	assert.Contains(t, out, "warning:")
	assert.NoDirExists(t, filepath.Join(filepath.Dir(f.repo), "data"))
}
