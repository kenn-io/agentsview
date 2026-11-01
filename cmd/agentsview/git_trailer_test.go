package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildGitTrailerCLI(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agentsview")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	cmd := exec.CommandContext(t.Context(), "go", "build", "-tags", "fts5", "-o", path, ".")
	cmd.Env = append(os.Environ(), "GOMAXPROCS=2", "GOFLAGS=-p=2 -trimpath")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build: %s", out)
	return path
}

type gitTrailerFixture struct {
	repo, bin string
	env       []string
}

func newGitTrailerFixture(t *testing.T, bin string) *gitTrailerFixture {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.Mkdir(repo, 0o700))
	f := &gitTrailerFixture{repo: repo, bin: bin, env: []string{
		"PATH=" + filepath.Dir(bin) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + root, "USERPROFILE=" + root, "XDG_CONFIG_HOME=" + filepath.Join(root, "xdg"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + filepath.Join(root, "gitconfig"),
		"GIT_AUTHOR_NAME=Example", "GIT_AUTHOR_EMAIL=example@example.com",
		"GIT_COMMITTER_NAME=Example", "GIT_COMMITTER_EMAIL=example@example.com",
		"AGENTSVIEW_DATA_DIR=" + filepath.Join(root, "data"),
		"AGENTSVIEW_SESSION_AGENT=pi", "AGENTSVIEW_SESSION_ID=first",
	}}
	f.git(t, "init", "-q")
	return f
}

func (f *gitTrailerFixture) run(t *testing.T, exe string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), exe, args...)
	cmd.Dir = f.repo
	cmd.Env = f.env
	b, e := cmd.CombinedOutput()
	return string(b), e
}

func (f *gitTrailerFixture) git(t *testing.T, args ...string) string {
	t.Helper()
	b, e := f.run(t, "git", args...)
	require.NoError(t, e, "git %v: %s", args, b)
	return strings.TrimSpace(b)
}

func (f *gitTrailerFixture) stage(t *testing.T, text string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.repo, "sample"), []byte(text), 0o600))
	f.git(t, "add", "sample")
}

func (f *gitTrailerFixture) setenv(key, value string) {
	prefix := key + "="
	for i, s := range f.env {
		if strings.HasPrefix(s, prefix) {
			f.env[i] = prefix + value
			return
		}
	}
	f.env = append(f.env, prefix+value)
}

func (f *gitTrailerFixture) install(t *testing.T) {
	t.Helper()
	out, err := f.run(t, f.bin, "git-hook", "install", "--repo", f.repo)
	require.NoError(t, err, out)
	assert.Contains(t, out, "prepare-commit-msg: installed")
	assert.Contains(t, out, "commit-msg: installed")
}

func shellQuotePath(path string) string { return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'" }

func TestGitTrailerExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-hook executable coverage runs on Unix")
	}
	bin := buildGitTrailerCLI(t)
	t.Run("managed hook commands", func(t *testing.T) { testManagedGitHookExecutable(t, bin) })
	t.Run("replay operations", func(t *testing.T) { testGitTrailerReplayExecutable(t, bin) })
	t.Run("message and amend", func(t *testing.T) {
		f := newGitTrailerFixture(t, bin)
		f.install(t)
		f.stage(t, "first\n")
		f.git(t, "commit", "-qm", "First message")
		assert.Equal(t, "First message\n\nAgent-Session: pi:first", f.git(t, "log", "-1", "--format=%B"))
		f.git(t, "commit", "--amend", "--no-edit", "-q")
		assert.Equal(t, 1, strings.Count(f.git(t, "log", "-1", "--format=%B"), "Agent-Session: pi:first"))
		f.setenv("AGENTSVIEW_SESSION_ID", "second")
		f.git(t, "commit", "--amend", "--no-edit", "-q")
		assert.Equal(t, "First message\n\nAgent-Session: pi:first\nAgent-Session: pi:second", f.git(t, "log", "-1", "--format=%B"))
	})
	t.Run("amend fixup with removed body still aborts", func(t *testing.T) {
		f := newGitTrailerFixture(t, bin)
		f.install(t)
		f.stage(t, "first\n")
		f.git(t, "commit", "-qm", "Subject\n\nbody")
		before := f.git(t, "rev-parse", "HEAD")
		f.stage(t, "change\n")
		editor := filepath.Join(t.TempDir(), "editor")
		require.NoError(t, os.WriteFile(editor, []byte("#!/bin/sh\nprintf '%s\\n' 'amend! Subject' > \"$1\"\n"), 0o755))
		f.setenv("GIT_EDITOR", shellQuotePath(editor))
		out, err := f.run(t, "git", "commit", "--fixup=amend:HEAD")
		require.Error(t, err, out)
		assert.Contains(t, out, "empty commit message body")
		assert.Equal(t, before, f.git(t, "rev-parse", "HEAD"))
	})
	t.Run("editor", func(t *testing.T) {
		f := newGitTrailerFixture(t, bin)
		f.install(t)
		f.stage(t, "first\n")
		editor := filepath.Join(t.TempDir(), "message-editor")
		require.NoError(t, os.WriteFile(editor, []byte("#!/bin/sh\nprintf '%s\\n' 'Edited message' > \"$1\"\n"), 0o755))
		f.setenv("GIT_EDITOR", shellQuotePath(editor))
		f.git(t, "commit", "-q")
		assert.Equal(t, "Edited message\n\nAgent-Session: pi:first", f.git(t, "log", "-1", "--format=%B"))
	})
	t.Run("abandoned verbose editor aborts", func(t *testing.T) {
		f := newGitTrailerFixture(t, bin)
		f.install(t)
		f.stage(t, "first\n")
		f.git(t, "commit", "-qm", "Base")
		before := f.git(t, "rev-parse", "HEAD")
		f.stage(t, "next\n")
		f.setenv("GIT_EDITOR", ":")
		out, err := f.run(t, "git", "commit", "-v")
		require.Error(t, err, out)
		assert.Equal(t, before, f.git(t, "rev-parse", "HEAD"))
		assert.Contains(t, out, "empty commit message")
	})
	t.Run("template unchanged and edited", func(t *testing.T) {
		f := newGitTrailerFixture(t, bin)
		f.install(t)
		f.stage(t, "first\n")
		f.git(t, "commit", "-qm", "Base")
		before := f.git(t, "rev-parse", "HEAD")
		f.stage(t, "next\n")
		f.setenv("GIT_EDITOR", ":")
		template := filepath.Join(t.TempDir(), "template")
		require.NoError(t, os.WriteFile(template, []byte("Describe the change here\n"), 0o600))
		out, err := f.run(t, "git", "commit", "-t", template)
		require.Error(t, err, out)
		assert.Equal(t, before, f.git(t, "rev-parse", "HEAD"))
		assert.Contains(t, out, "did not edit the message")
		editor := filepath.Join(t.TempDir(), "template-editor")
		require.NoError(t, os.WriteFile(editor, []byte("#!/bin/sh\nprintf '%s\\n' 'Template was edited' > \"$1\"\n"), 0o755))
		f.setenv("GIT_EDITOR", shellQuotePath(editor))
		f.git(t, "commit", "-q", "-t", template)
		assert.Equal(t, "Template was edited\n\nAgent-Session: pi:first", f.git(t, "log", "-1", "--format=%B"))
	})

	t.Run("signed abandoned messages abort", func(t *testing.T) {
		for _, args := range [][]string{{"commit", "-s"}, {"commit", "-s", "-v"}, {"commit", "-s", "-m", ""}} {
			f := newGitTrailerFixture(t, bin)
			f.install(t)
			f.stage(t, "base\n")
			f.git(t, "commit", "-qm", "Base")
			before := f.git(t, "rev-parse", "HEAD")
			f.stage(t, "change\n")
			f.setenv("GIT_EDITOR", ":")
			out, err := f.run(t, "git", args...)
			require.Error(t, err, out)
			assert.Equal(t, before, f.git(t, "rev-parse", "HEAD"))
		}
	})
	t.Run("signoff-only template edit aborts", func(t *testing.T) {
		f := newGitTrailerFixture(t, bin)
		f.install(t)
		f.stage(t, "base\n")
		f.git(t, "commit", "-qm", "Base")
		before := f.git(t, "rev-parse", "HEAD")
		f.stage(t, "change\n")
		template := filepath.Join(t.TempDir(), "template")
		require.NoError(t, os.WriteFile(template, []byte("Describe this change\n"), 0o600))
		editor := filepath.Join(t.TempDir(), "editor")
		require.NoError(t, os.WriteFile(editor, []byte("#!/bin/sh\nprintf '\\nSigned-off-by: Example <example@example.com>\\n' >> \"$1\"\n"), 0o755))
		f.setenv("GIT_EDITOR", shellQuotePath(editor))
		out, err := f.run(t, "git", "commit", "-t", template)
		require.Error(t, err, out)
		assert.Equal(t, before, f.git(t, "rev-parse", "HEAD"))
	})
	t.Run("edited message can be abandoned", func(t *testing.T) {
		f := newGitTrailerFixture(t, bin)
		f.install(t)
		f.stage(t, "base\n")
		f.git(t, "commit", "-qm", "Base")
		before := f.git(t, "rev-parse", "HEAD")
		f.stage(t, "change\n")
		editor := filepath.Join(t.TempDir(), "editor")
		require.NoError(t, os.WriteFile(editor, []byte("#!/bin/sh\n: > \"$1\"\n"), 0o755))
		f.setenv("GIT_EDITOR", shellQuotePath(editor))
		out, err := f.run(t, "git", "commit", "-m", "Discard this", "-e")
		require.Error(t, err, out)
		assert.Equal(t, before, f.git(t, "rev-parse", "HEAD"))
	})
	t.Run("comment aliases retain verbose trailer", func(t *testing.T) {
		for _, settings := range [][]string{
			{"core.commentString", "//", "core.commentChar", "#"},
			{"core.commentChar", "#", "core.commentString", "//"},
		} {
			f := newGitTrailerFixture(t, bin)
			f.install(t)
			f.git(t, "config", "--global", settings[0], settings[1])
			f.git(t, "config", settings[2], settings[3])
			f.stage(t, "base\n")
			f.git(t, "commit", "-qm", "Base")
			f.stage(t, "change\n")
			editor := filepath.Join(t.TempDir(), "editor")
			script := "#!/bin/sh\n{ printf 'Edited subject\\n'; cat \"$1\"; } > \"$1.edited\"\nmv \"$1.edited\" \"$1\"\n"
			require.NoError(t, os.WriteFile(editor, []byte(script), 0o755))
			f.setenv("GIT_EDITOR", shellQuotePath(editor))
			f.git(t, "commit", "-v")
			assert.Equal(t, "Edited subject\n\nAgent-Session: pi:first", f.git(t, "log", "-1", "--format=%B"))
		}
	})
	t.Run("comment prefix collision warns and skips", func(t *testing.T) {
		f := newGitTrailerFixture(t, bin)
		f.install(t)
		f.git(t, "config", "core.commentChar", "A")
		f.stage(t, "change\n")
		editor := filepath.Join(t.TempDir(), "editor")
		require.NoError(t, os.WriteFile(editor, []byte("#!/bin/sh\nexit 0\n"), 0o755))
		f.setenv("GIT_EDITOR", shellQuotePath(editor))
		out, err := f.run(t, "git", "commit", "-m", "Subject", "-e")
		require.NoError(t, err, out)
		assert.Contains(t, out, "would remove the Agent-Session trailer")
		assert.Equal(t, "Subject", f.git(t, "log", "-1", "--format=%B"))
	})
	t.Run("failed preparation leaves template abort intact", func(t *testing.T) {
		realGit, err := exec.LookPath("git")
		require.NoError(t, err)
		for _, fault := range []string{"exit 1", "sleep 4"} {
			f := newGitTrailerFixture(t, bin)
			f.install(t)
			f.stage(t, "base\n")
			f.git(t, "commit", "-qm", "Base")
			before := f.git(t, "rev-parse", "HEAD")
			f.stage(t, "change\n")
			template := filepath.Join(t.TempDir(), "template")
			require.NoError(t, os.WriteFile(template, []byte("Describe this change\n"), 0o600))
			shimDir := t.TempDir()
			marker := filepath.Join(shimDir, "failed-once")
			shim := "#!/bin/sh\nif [ \"${1:-}\" = rev-parse ] && [ ! -f " + shellQuotePath(marker) + " ]; then\n: > " + shellQuotePath(marker) + "\n" + fault + "\nfi\nexec " + shellQuotePath(realGit) + " \"$@\"\n"
			require.NoError(t, os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755))
			// Git prepends its exec path when launching hooks. Put the fault
			// shim first inside this fixture's prepare hook, after that change.
			hook := filepath.Join(f.repo, ".git", "hooks", "prepare-commit-msg")
			script, err := os.ReadFile(hook)
			require.NoError(t, err)
			injected := "#!/bin/sh\nPATH=" + shellQuotePath(shimDir) + ":$PATH\nexport PATH\n"
			script = []byte(strings.Replace(string(script), "#!/bin/sh\n", injected, 1))
			require.NoError(t, os.WriteFile(hook, script, 0o755))
			f.setenv("PATH", shimDir+string(os.PathListSeparator)+filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
			f.setenv("GIT_EDITOR", ":")
			out, err := f.run(t, realGit, "commit", "-t", template)
			require.Error(t, err, out)
			assert.FileExists(t, marker)
			assert.Contains(t, out, "did not edit the message")
			assert.Equal(t, before, f.git(t, "rev-parse", "HEAD"))
		}
	})
	t.Run("entry fails open without startup writes", func(t *testing.T) {
		f := newGitTrailerFixture(t, bin)
		absent := filepath.Join(t.TempDir(), "absent-data")
		f.setenv("AGENTSVIEW_DATA_DIR", absent)
		f.setenv("TMPDIR", filepath.Join(t.TempDir(), "absent-temp"))
		f.setenv("AGENTSVIEW_MODE", "invalid")
		for _, args := range [][]string{{"git-trailer"}, {"git-trailer", "--unknown"}, {"git-trailer", "-port"}, {"git-trailer", "a", "b", "c"}} {
			out, err := f.run(t, bin, args...)
			require.NoError(t, err, out)
			assert.LessOrEqual(t, strings.Count(strings.TrimSuffix(out, "\n"), "\n"), 0)
		}
		assert.NoDirExists(t, absent)
	})
	t.Run("unrelated semantic config is ignored", func(t *testing.T) {
		f := newGitTrailerFixture(t, bin)
		f.install(t)
		data := filepath.Join(t.TempDir(), "data")
		require.NoError(t, os.Mkdir(data, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(data, "config.toml"), []byte("port=-500\n"), 0o600))
		f.setenv("AGENTSVIEW_DATA_DIR", data)
		f.stage(t, "first\n")
		f.git(t, "commit", "-qm", "Subject")
		assert.Contains(t, f.git(t, "log", "-1", "--format=%B"), "Agent-Session: pi:first")
	})
	t.Run("disabled and invalid config permit commits", func(t *testing.T) {
		f := newGitTrailerFixture(t, bin)
		f.install(t)
		data := filepath.Join(t.TempDir(), "data")
		require.NoError(t, os.Mkdir(data, 0o700))
		f.setenv("AGENTSVIEW_DATA_DIR", data)
		for _, config := range []string{"[git_trailer]\nenabled=false\n", "[malformed"} {
			require.NoError(t, os.WriteFile(filepath.Join(data, "config.toml"), []byte(config), 0o600))
			f.stage(t, config)
			f.git(t, "commit", "-qm", "Subject")
			assert.Equal(t, "Subject", f.git(t, "log", "-1", "--format=%B"))
		}
	})
}

func testManagedGitHookExecutable(t *testing.T, bin string) {
	t.Helper()
	f := newGitTrailerFixture(t, bin)
	dir := filepath.Join(f.repo, "custom hooks")
	f.git(t, "config", "core.hooksPath", dir)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	original := "#!/bin/sh -eu\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "commit-msg"), []byte(original), 0o751))
	f.install(t)
	f.install(t)
	f.stage(t, "first\n")
	f.git(t, "commit", "-qm", "Managed hooks")
	assert.Equal(t, "Managed hooks\n\nAgent-Session: pi:first", f.git(t, "log", "-1", "--format=%B"))
	absent := filepath.Join(t.TempDir(), "absent-data")
	f.setenv("AGENTSVIEW_DATA_DIR", absent)
	f.setenv("TMPDIR", filepath.Join(t.TempDir(), "absent-temp"))
	out, err := f.run(t, bin, "git-hook", "status", "--repo", f.repo)
	require.NoError(t, err, out)
	assert.NoDirExists(t, absent)
	out, err = f.run(t, bin, "git-hook", "uninstall", "--repo", f.repo)
	require.NoError(t, err, out)
	got, err := os.ReadFile(filepath.Join(dir, "commit-msg"))
	require.NoError(t, err)
	assert.Equal(t, original, string(got))
	assert.NoFileExists(t, filepath.Join(dir, "prepare-commit-msg"))
	f.setenv("TMPDIR", t.TempDir())
	out, err = f.run(t, bin, "git-hook", "install", "--repo", f.repo, "--global")
	require.Error(t, err, out)
	assert.Contains(t, out, "[repo global]")
	assert.Contains(t, out, "none of the others can be")
	assert.Equal(t, dir, f.git(t, "config", "--get", "core.hooksPath"))
}

func testGitTrailerReplayExecutable(t *testing.T, bin string) {
	t.Helper()
	for _, operation := range []string{"cherry-pick", "rebase"} {
		t.Run(operation, func(t *testing.T) {
			f := newGitTrailerFixture(t, bin)
			f.install(t)
			f.stage(t, "base\n")
			f.git(t, "commit", "-qm", "Base")
			baseBranch := f.git(t, "symbolic-ref", "--short", "HEAD")
			f.git(t, "checkout", "-qb", "topic")
			require.NoError(t, os.WriteFile(filepath.Join(f.repo, "topic"), []byte("topic\n"), 0o600))
			f.git(t, "add", "topic")
			f.git(t, "commit", "-qm", "Topic")
			topic := f.git(t, "rev-parse", "HEAD")
			message := f.git(t, "log", "-1", "--format=%B")
			f.git(t, "checkout", "-q", baseBranch)
			require.NoError(t, os.WriteFile(filepath.Join(f.repo, "main-only"), []byte("main\n"), 0o600))
			f.git(t, "add", "main-only")
			f.git(t, "commit", "-qm", "Main")
			f.setenv("AGENTSVIEW_SESSION_ID", "replayer")
			switch operation {
			case "cherry-pick":
				f.git(t, "cherry-pick", topic)
			case "rebase":
				f.git(t, "rebase", baseBranch, "topic")
			}
			assert.Equal(t, message, f.git(t, "log", "-1", "--format=%B"))
		})
	}
}
