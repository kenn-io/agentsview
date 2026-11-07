package main

import (
	"errors"
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
	scissors    = "------------------------ >8 ------------------------"
	// rewriteSignoff is a prepare-commit-msg step standing in for another hook.
	rewriteSignoff = `tmp="$1.tmp" && sed 's/^Signed-off-by: .*/Signed-off-by: Other <other@example.com>/' "$1" > "$tmp" && mv "$tmp" "$1"`
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
		staged        bool
		prepareSuffix string
		comment       [2]string
		templateText  string
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
		{name: "template with comment from another hook", template: true, prepareSuffix: `printf '\n# note from another hook\n\n' >> "$1"`, abort: true, abortContains: "you did not edit the message"},
		{name: "generated signoff rewritten by another hook", args: []string{"-s"}, template: true, prepareSuffix: rewriteSignoff, abort: true, abortContains: "you did not edit the message"},
		{name: "verbose diff below scissors", args: []string{"-v"}, staged: true, abort: true},
		{name: "verbose diff below scissors with spaced comment string", args: []string{"-v"}, staged: true, comment: [2]string{"core.commentString", "// x"}, abort: true},
		{name: "verbose diff below scissors with automatic comment character", args: []string{"-v"}, staged: true, comment: [2]string{"core.commentChar", "auto"}, abort: true},
		{name: "scissors-like line with unselected automatic comment character", args: []string{"-m", "; " + scissors}, comment: [2]string{"core.commentChar", "auto"}, want: "; " + scissors + "\n\n" + trailerLine},
		{name: "scissors-like lines with unselected automatic comment character", args: []string{"-m", "; " + scissors + "\n; explanation"}, comment: [2]string{"core.commentChar", "auto"}, want: "; " + scissors + "\n; explanation\n\n" + trailerLine},
		{name: "template signoff changed by another hook", template: true, templateText: "Template subject\n\nSigned-off-by: Template Author <author@example.com>\n", prepareSuffix: rewriteSignoff, want: "Template subject\n\nSigned-off-by: Other <other@example.com>\n" + trailerLine},
		{name: "scissors-like message line", args: []string{"-m", "foo " + scissors}, want: "foo " + scissors + "\n\n" + trailerLine},
		{name: "editor", editor: true, want: "from editor"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newGitTrailerFixture(t)
			f.hook(t, "install")
			f.setenv("GIT_EDITOR", ":")
			if tt.prepareSuffix != "" {
				prepare := filepath.Join(f.repo, ".git", "hooks", "prepare-commit-msg")
				data, err := os.ReadFile(prepare)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(prepare, append(data, tt.prepareSuffix+"\n"...), 0o755))
			}
			if tt.comment[0] != "" {
				f.git(t, "config", tt.comment[0], tt.comment[1])
			}
			if tt.staged {
				require.NoError(t, os.WriteFile(filepath.Join(f.repo, "staged.txt"), []byte("staged change\n"), 0o600))
				f.git(t, "add", "staged.txt")
			}
			if tt.name == "file" {
				f.setenv("TMPDIR", filepath.Join(f.repo, "missing-temp"))
			}
			require.NoError(t, os.WriteFile(filepath.Join(f.repo, "message"), []byte("from file\n"), 0o600))
			if tt.template {
				template := filepath.Join(f.repo, "template")
				text := tt.templateText
				if text == "" {
					text = "Template subject\n\nTemplate body\n"
				}
				require.NoError(t, os.WriteFile(template, []byte(text), 0o600))
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
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "#!/bin/sh\n# existing hook\n", string(data))
	steps := hookInstructions(t, out)
	require.Len(t, steps, 1)
	require.Equal(t, path, steps[0].path)
	require.Equal(t, "add", steps[0].action)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+steps[0].text+"\nprintf 'later check\\n' >&2\nexit 42\n"), 0o755))
	require.NoError(t, os.Remove(f.bin))
	f.setenv("PATH", filepath.Dir(f.bin))
	out, err = f.run(t, "git", "commit", "--allow-empty", "-m", "rejected by later check")
	require.Error(t, err, out)
	assert.Contains(t, out, "later check")
}

// A custom hook blocks installation of both hooks, and the printed steps must
// produce a working pair, including Git's unchanged-template rejection.
func TestGitHookInstallInstructions(t *testing.T) {
	const custom = "#!/bin/sh\n# existing hook\n"
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, f *gitTrailerFixture, hooks string)
		steps int
	}{
		{name: "custom prepare-commit-msg", steps: 2, setup: func(t *testing.T, _ *gitTrailerFixture, hooks string) {
			require.NoError(t, os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"), []byte(custom), 0o755))
		}},
		{name: "custom commit-msg", steps: 2, setup: func(t *testing.T, _ *gitTrailerFixture, hooks string) {
			require.NoError(t, os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte(custom), 0o755))
		}},
		{name: "custom non-executable prepare-commit-msg", steps: 3, setup: func(t *testing.T, _ *gitTrailerFixture, hooks string) {
			require.NoError(t, os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"), []byte(custom), 0o644))
		}},
		// Git checks execute access for the current user, so only the outcome is fixed.
		{name: "prepare-commit-msg executable only by others", setup: func(t *testing.T, f *gitTrailerFixture, hooks string) {
			f.hook(t, "install")
			require.NoError(t, os.Chmod(filepath.Join(hooks, "prepare-commit-msg"), 0o655))
			require.NoError(t, os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte(custom), 0o755))
		}},
		{name: "non-executable prepare-commit-msg", steps: 2, setup: func(t *testing.T, f *gitTrailerFixture, hooks string) {
			f.hook(t, "install")
			require.NoError(t, os.Chmod(filepath.Join(hooks, "prepare-commit-msg"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte(custom), 0o755))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newGitTrailerFixture(t)
			hooks := filepath.Join(f.repo, ".git", "hooks")
			tt.setup(t, f, hooks)
			before := hookFiles(t, hooks)
			out, err := f.run(t, f.bin, "git-hook", "install")
			require.Error(t, err)
			assert.Equal(t, before, hookFiles(t, hooks), "install must not write half of the pair")
			steps := hookInstructions(t, out)
			if tt.steps > 0 {
				require.Len(t, steps, tt.steps, out)
			}
			for _, step := range steps {
				switch step.action {
				case "create":
					require.NoError(t, os.WriteFile(step.path, []byte(step.text+"\n"), 0o755))
				case "add":
					data, err := os.ReadFile(step.path)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(step.path, append(data, step.text+"\n"...), 0o755))
				case "chmod":
					require.NoError(t, os.Chmod(step.path, 0o755))
				}
			}
			f.git(t, "commit", "--allow-empty", "-m", "integrated hooks")
			assert.Equal(t, "integrated hooks\n\n"+trailerLine, f.git(t, "log", "-1", "--format=%B"))
			template := filepath.Join(f.repo, "template")
			require.NoError(t, os.WriteFile(template, []byte("Template subject\n"), 0o600))
			f.git(t, "config", "commit.template", template)
			f.setenv("GIT_EDITOR", ":")
			out, err = f.run(t, "git", "commit", "--allow-empty")
			require.Error(t, err, out)
			assert.Contains(t, out, "you did not edit the message")
		})
	}
}

// hookFiles records each hook's content and mode, so tests can prove install wrote nothing.
func hookFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, name := range []string{"prepare-commit-msg", "commit-msg"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		require.NoError(t, err)
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		files[name] = info.Mode().String() + "\n" + string(data)
	}
	return files
}

type hookStep struct{ action, path, text string }

// hookInstructions reads the integration steps that git-hook install prints.
func hookInstructions(t *testing.T, out string) []hookStep {
	t.Helper()
	var steps []hookStep
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if path, ok := strings.CutPrefix(line, "add this line to "); ok {
			require.Greater(t, len(lines), i+1, out)
			steps = append(steps, hookStep{action: "add", path: strings.TrimSuffix(path, ":"), text: lines[i+1]})
		}
		if rest, ok := strings.CutPrefix(line, "create executable "); ok {
			require.Greater(t, len(lines), i+2, out)
			steps = append(steps, hookStep{action: "create", path: strings.TrimSuffix(rest, " containing:"), text: lines[i+1] + "\n" + lines[i+2]})
		}
		if rest, ok := strings.CutPrefix(line, "make "); ok {
			steps = append(steps, hookStep{action: "chmod", path: strings.TrimSuffix(rest, " executable")})
		}
	}
	return steps
}
