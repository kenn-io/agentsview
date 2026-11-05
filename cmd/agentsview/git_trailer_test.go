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
	"go.kenn.io/agentsview/internal/shellquote"
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
	require.NoError(t, os.WriteFile(f.bin, []byte("#!/bin/sh\nexec "+shellquote.POSIX(exe)+" -test.run=^TestGitTrailerCLIProcess$ -- \"$@\"\n"), 0o755))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "AGENTSVIEW_") || slices.Contains(sessionEnvKeys, key) {
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

// sessionEnvKeys keeps the developer's own agent session out of fixture commits.
var sessionEnvKeys = []string{
	"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID", "CURSOR_CONVERSATION_ID",
	"OPENCODE_SESSION_ID", "PI_SESSION_ID", "OMPCODE", "QWEN_CODE_SESSION_ID", "AGENT_SESSION_ID", "HERMES_SESSION_ID",
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
		name          string
		args          []string
		editor        bool
		template      bool
		templateFlag  bool
		abort         bool
		abortContains string
		want          string
	}{
		{name: "hash-leading subject is stripped as a comment", args: []string{"-m", "#123 fix the parser"}, want: "#123 fix the parser"},
		{name: "file", args: []string{"-F", "message"}, want: "from file\n\n" + trailerLine},
		{name: "empty", args: []string{"-m", ""}, abort: true},
		{name: "empty signoff", args: []string{"-s", "-m", ""}, abort: true},
		{name: "command-line stripped comment", args: []string{"--cleanup=strip", "-m", "# wip"}, abort: true},
		{name: "comment-only message", abort: true},
		{name: "unchanged configured template", template: true, abort: true, abortContains: "you did not edit the message"},
		{name: "unchanged command-line template", template: true, templateFlag: true, abort: true, abortContains: "you did not edit the message"},
		{name: "editor", editor: true, want: "from editor"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newGitTrailerFixture(t)
			f.hook(t, "install")
			f.setenv("GIT_EDITOR", ":")
			if tt.name == "file" {
				f.setenv("TMPDIR", filepath.Join(f.repo, "missing-temp"))
			}
			require.NoError(t, os.WriteFile(filepath.Join(f.repo, "message"), []byte("from file\n"), 0o600))
			if tt.template {
				template := filepath.Join(f.repo, "template")
				require.NoError(t, os.WriteFile(template, []byte("Template subject\n\nTemplate body\n"), 0o600))
				if tt.templateFlag {
					tt.args = append(tt.args, "-t", "template")
				} else {
					f.git(t, "config", "commit.template", template)
				}
			}
			if tt.editor {
				editor := filepath.Join(filepath.Dir(f.bin), "editor")
				require.NoError(t, os.WriteFile(editor, []byte("#!/bin/sh\nprintf 'from editor\\n' > \"$1\"\n"), 0o755))
				f.setenv("GIT_EDITOR", editor)
			}
			out, err := f.run(t, "git", append([]string{"commit", "--allow-empty"}, tt.args...)...)
			if tt.abort {
				require.Error(t, err, out)
				if tt.abortContains == "" {
					assert.Contains(t, out, "empty commit message")
				} else {
					assert.Contains(t, out, tt.abortContains)
				}
				message, readErr := os.ReadFile(filepath.Join(f.repo, ".git", "COMMIT_EDITMSG"))
				require.NoError(t, readErr)
				assert.NotContains(t, string(message), "Agent-Session:")
				return
			}
			require.NoError(t, err, out)
			assert.Equal(t, tt.want, f.git(t, "log", "-1", "--format=%B"))
			if tt.name == "file" {
				assert.NoDirExists(t, filepath.Join(filepath.Dir(f.repo), "data"))
			}
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
}

func TestGitTrailerUnavailable(t *testing.T) {
	for _, state := range []string{"missing", "failure", "no session", "ambiguous"} {
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

func TestGitHookRejectsUnknownAction(t *testing.T) {
	_, err := executeCommand(newRootCommand(), "git-hook", "instal")

	require.Error(t, err)
	assert.ErrorContains(t, err, `unknown command "instal"`)
}

func TestGitHookInstall(t *testing.T) {
	for _, tt := range []struct{ hooksDir, subdir string }{
		{".git/hooks", ""}, {"custom-hooks", ""}, {"custom-hooks ", ""}, {"custom-hooks", "nested/dir"},
	} {
		t.Run(filepath.Join(tt.subdir, tt.hooksDir), func(t *testing.T) {
			f := newGitTrailerFixture(t)
			repo := f.repo
			if tt.hooksDir != ".git/hooks" {
				f.git(t, "config", "core.hooksPath", tt.hooksDir)
			}
			f.repo = filepath.Join(f.repo, tt.subdir)
			require.NoError(t, os.MkdirAll(f.repo, 0o700))
			f.hook(t, "install")
			f.repo = repo
			f.git(t, "commit", "--allow-empty", "-m", "installed hook")
			assert.Equal(t, "installed hook\n\n"+trailerLine, f.git(t, "log", "-1", "--format=%B"))
		})
	}
}

func TestGitHookReinstall(t *testing.T) {
	f := newGitTrailerFixture(t)
	f.hook(t, "install")
	path := filepath.Join(f.repo, ".git", "hooks", "commit-msg")
	preparePath := filepath.Join(f.repo, ".git", "hooks", "prepare-commit-msg")
	require.NoError(t, os.Remove(preparePath))
	f.hook(t, "install")
	assert.FileExists(t, preparePath)
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0o644))
	before, err := os.Stat(path)
	require.NoError(t, err)
	f.hook(t, "install")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, original, data)
	assert.Equal(t, before.ModTime(), after.ModTime())
	assert.Equal(t, os.FileMode(0o755), after.Mode().Perm())
	f.git(t, "commit", "--allow-empty", "-m", "reinstalled hook")
	assert.Equal(t, "reinstalled hook\n\n"+trailerLine, f.git(t, "log", "-1", "--format=%B"))
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n# existing hook\n"), 0o755))
	out, err := f.run(t, f.bin, "git-hook", "install")
	require.Error(t, err)
	assert.Contains(t, out, `if command -v agentsview >/dev/null 2>&1; then agentsview git-trailer "$1" || :; fi`)
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\n# existing hook\n", string(data))
	_, line, ok := strings.Cut(out, "add this line to it:\n")
	require.True(t, ok)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+strings.TrimSpace(line)+"\nprintf 'later check\\n' >&2\nexit 42\n"), 0o755))
	require.NoError(t, os.Remove(f.bin))
	f.setenv("PATH", filepath.Dir(f.bin))
	out, err = f.run(t, "git", "commit", "--allow-empty", "-m", "rejected by later check")
	require.Error(t, err, out)
	assert.Contains(t, out, "later check")
}

func TestGitHookInstallPreservesExistingPrepareHook(t *testing.T) {
	f := newGitTrailerFixture(t)
	path := filepath.Join(f.repo, ".git", "hooks", "prepare-commit-msg")
	const existing = "#!/bin/sh\n# existing hook\n"
	require.NoError(t, os.WriteFile(path, []byte(existing), 0o755))
	out, err := f.run(t, f.bin, "git-hook", "install")
	require.Error(t, err)
	assert.Contains(t, out, "prepare-commit-msg already exists; add this line to it:")
	assert.Contains(t, out, "agentsview git-trailer-prepare")
	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, existing, string(data))
	_, statErr := os.Stat(filepath.Join(f.repo, ".git", "hooks", "commit-msg"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}
