package gittrailer

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestHookLifecycle(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Install(dir))
	statuses, err := Status(dir)
	require.NoError(t, err)
	require.Len(t, statuses, 2)
	assert.True(t, statuses[0].Installed)
	assert.True(t, statuses[1].Installed)
	before := map[string][]byte{}
	for _, name := range []string{"prepare-commit-msg", "commit-msg"} {
		b, e := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, e)
		before[name] = b
	}
	require.NoError(t, Install(dir))
	for name, b := range before {
		got, e := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, e)
		assert.Equal(t, b, got)
	}
	require.NoError(t, Uninstall(dir))
	require.NoError(t, Uninstall(dir))
	for name := range before {
		assert.NoFileExists(t, filepath.Join(dir, name))
	}
	statuses, err = Status(dir)
	require.NoError(t, err)
	for _, s := range statuses {
		assert.False(t, s.Installed)
	}
}

func TestExistingHookRestoration(t *testing.T) {
	for _, original := range []string{"#!/bin/sh\n", "#!/bin/sh -eu\n# keep every byte\nprintf '%s' \"$1\"\nexit 0", "#!/usr/bin/env bash\nexit 0\n"} {
		t.Run(original, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "commit-msg")
			require.NoError(t, os.WriteFile(path, []byte(original), 0o751))
			require.NoError(t, Install(dir))
			require.NoError(t, Uninstall(dir))
			got, e := os.ReadFile(path)
			require.NoError(t, e)
			assert.Equal(t, original, string(got))
			if runtime.GOOS != "windows" {
				info, e := os.Stat(path)
				require.NoError(t, e)
				assert.Equal(t, os.FileMode(0o751), info.Mode().Perm())
			}
		})
	}
}

func TestHooksPreserveExistingStatusAndArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell options tested on Unix")
	}
	for _, name := range []string{"prepare-commit-msg", "commit-msg"} {
		for _, binary := range []string{"missing", "failure", "signal"} {
			t.Run(name+"/"+binary, func(t *testing.T) {
				dir := t.TempDir()
				bin := t.TempDir()
				sentinel := filepath.Join(t.TempDir(), "args")
				t.Setenv("HOOK_SENTINEL", sentinel)
				t.Setenv("PATH", bin)
				if binary != "missing" {
					script := "#!/bin/sh\nexit 42\n"
					if binary == "signal" {
						script = "#!/bin/sh\nkill -TERM $$\n"
					}
					require.NoError(t, os.WriteFile(filepath.Join(bin, "agentsview"), []byte(script), 0o755))
				}
				original := "#!/bin/sh -eu\nprintf '%s\\n' \"$@\" > \"$HOOK_SENTINEL\"\nexit 7\n"
				path := filepath.Join(dir, name)
				require.NoError(t, os.WriteFile(path, []byte(original), 0o755))
				require.NoError(t, Install(dir))
				args := []string{"message file", "message", "extra argument"}
				cmd := exec.CommandContext(t.Context(), path, args...)
				b, e := cmd.CombinedOutput()
				require.Error(t, e, string(b))
				var exit *exec.ExitError
				require.ErrorAs(t, e, &exit)
				assert.Equal(t, 7, exit.ExitCode())
				got, e := os.ReadFile(sentinel)
				require.NoError(t, e)
				assert.Equal(t, "message file\nmessage\nextra argument\n", string(got))
				// Editor commits omit the source argument, even with -u enabled.
				cmd = exec.CommandContext(t.Context(), path, "message file")
				b, e = cmd.CombinedOutput()
				require.Error(t, e, string(b))
				got, e = os.ReadFile(sentinel)
				require.NoError(t, e)
				assert.Equal(t, "message file\n", string(got))
			})
		}
	}
}

func TestInstallPrevalidatesBothHooks(t *testing.T) {
	for _, original := range []string{"#!/usr/bin/python3\nprint('keep')\n", "no shebang\n", "#!/bin/sh\n# >>> agentsview git-trailer v1 corrupt\n"} {
		dir := t.TempDir()
		bad := filepath.Join(dir, "commit-msg")
		require.NoError(t, os.WriteFile(bad, []byte(original), 0o755))
		require.Error(t, Install(dir))
		assert.NoFileExists(t, filepath.Join(dir, "prepare-commit-msg"))
		got, e := os.ReadFile(bad)
		require.NoError(t, e)
		assert.Equal(t, original, string(got))
	}
	if runtime.GOOS != "windows" {
		dir := t.TempDir()
		path := filepath.Join(dir, "commit-msg")
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o644))
		require.Error(t, Install(dir))
		assert.NoFileExists(t, filepath.Join(dir, "prepare-commit-msg"))
	}
}

func TestHookUninstallPreservesLaterEdits(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, Install(dir))
	path := filepath.Join(dir, "commit-msg")
	f, e := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, e)
	_, e = f.WriteString("# later edit\n")
	require.NoError(t, e)
	require.NoError(t, f.Close())
	require.NoError(t, Uninstall(dir))
	got, e := os.ReadFile(path)
	require.NoError(t, e)
	assert.Equal(t, "#!/bin/sh\n# later edit\n", string(got))
}

func TestHookRestorationProperty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "commit-msg")
	hegel.Test(t, func(ht *hegel.T) {
		// Shell comments admit arbitrary Unicode text once physical newlines are prefixed.
		comment := hegel.Draw(ht, hegel.Text())
		if hegel.Draw(ht, hegel.WeightedBooleans(0.01)) {
			comment = strings.Repeat("x", hegel.Draw(ht, hegel.Integers(maxMessageBytes-512, maxMessageBytes+1)))
		}
		ht.Assume(!strings.Contains(comment, "agentsview git-trailer"))
		original := "#!/bin/sh\n# " + strings.ReplaceAll(comment, "\n", "\n# ") + "\nexit 0\n"
		require.NoError(ht, os.WriteFile(path, []byte(original), 0o755))
		err := Install(dir)
		if len(original)+len(managedBlock("commit-msg", false)) > 2<<20 {
			require.Error(ht, err)
			got, e := os.ReadFile(path)
			require.NoError(ht, e)
			assert.Equal(ht, original, string(got))
			assert.NoFileExists(ht, filepath.Join(dir, "prepare-commit-msg"))
			return
		}
		require.NoError(ht, err)
		statuses, e := Status(dir)
		require.NoError(ht, e)
		require.Len(ht, statuses, 2)
		assert.True(ht, statuses[1].Installed)
		require.NoError(ht, Uninstall(dir))
		got, e := os.ReadFile(path)
		require.NoError(ht, e)
		assert.Equal(ht, original, string(got))
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestHooksDir(t *testing.T) {
	repo := trailerRepo(t)
	got, err := HooksDir(t.Context(), repo, false)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(repo, ".git", "hooks"), got)
	gitTest(t, repo, "config", "core.hooksPath", "relative hooks")
	got, err = HooksDir(t.Context(), repo, false)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(repo, "relative hooks"), got)
	global := filepath.Join(t.TempDir(), "global hooks")
	gitTest(t, repo, "config", "--global", "core.hooksPath", global)
	got, err = HooksDir(t.Context(), "", true)
	require.NoError(t, err)
	assert.Equal(t, global, got)
	gitTest(t, repo, "config", "--global", "core.hooksPath", "relative")
	_, err = HooksDir(t.Context(), "", true)
	require.Error(t, err)
}

func TestGlobalHooksDirFollowsIncludesWithoutRepositoryConditions(t *testing.T) {
	repo := trailerRepo(t)
	root := t.TempDir()
	ordinary := filepath.Join(root, "global hooks")
	conditional := filepath.Join(root, "conditional hooks")
	included := filepath.Join(root, "included config")
	condition := filepath.Join(root, "conditional config")
	require.NoError(t, os.WriteFile(included, []byte("[core]\nhooksPath = "+filepath.ToSlash(ordinary)+"\n"), 0o600))
	require.NoError(t, os.WriteFile(condition, []byte("[core]\nhooksPath = "+filepath.ToSlash(conditional)+"\n"), 0o600))
	globalConfig := "[include]\npath = " + filepath.ToSlash(included) + "\n[includeIf \"gitdir:" + filepath.ToSlash(filepath.Join(repo, ".git")) + "\"]\npath = " + filepath.ToSlash(condition) + "\n"
	require.NoError(t, os.WriteFile(os.Getenv("GIT_CONFIG_GLOBAL"), []byte(globalConfig), 0o600))
	got, err := HooksDir(t.Context(), "", true)
	require.NoError(t, err)
	assert.Equal(t, ordinary, got)
	got, err = HooksDir(t.Context(), repo, false)
	require.NoError(t, err)
	assert.Equal(t, conditional, got)
}

func TestGlobalHooksDirUsesXDGAndExpandsHome(t *testing.T) {
	trailerRepo(t)
	require.NoError(t, os.Unsetenv("GIT_CONFIG_GLOBAL")) // trailerRepo's Setenv restores it.
	xdg := os.Getenv("XDG_CONFIG_HOME")
	dir := filepath.Join(xdg, "git")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config"), []byte("[core]\nhooksPath = ~/shared hooks\n"), 0o600))
	got, err := HooksDir(t.Context(), "", true)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(os.Getenv("HOME"), "shared hooks"), got)
}

func TestLinkedWorktreeReplayStateIsIndependent(t *testing.T) {
	repo := trailerRepo(t)
	gitTest(t, repo, "config", "user.name", "Example")
	gitTest(t, repo, "config", "user.email", "example@example.com")
	gitTest(t, repo, "commit", "--allow-empty", "-qm", "Base")
	linked := filepath.Join(t.TempDir(), "linked")
	gitTest(t, repo, "worktree", "add", "--detach", linked, "HEAD")
	got, err := HooksDir(t.Context(), linked, false)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(repo, ".git", "hooks"), got)
	marker := gitTest(t, repo, "rev-parse", "--path-format=absolute", "--git-path", "CHERRY_PICK_HEAD")
	require.NoError(t, os.WriteFile(marker, []byte("fixture"), 0o600))
	t.Chdir(linked)
	path := messageFile(t, linked, "Subject\n")
	require.NoError(t, Stamp(t.Context(), path, nil, Session{"pi", "one"}))
	assert.Equal(t, "Subject\n\nAgent-Session: pi:one\n", messageText(t, path))
}

func TestInstallRejectsProjectedOversizeBeforeChangingEitherHook(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "commit-msg")
	original := "#!/bin/sh\n#" + strings.Repeat("x", maxMessageBytes-len("#!/bin/sh\n#"))
	require.NoError(t, os.WriteFile(path, []byte(original), 0o755))
	require.Error(t, Install(dir))
	assert.NoFileExists(t, filepath.Join(dir, "prepare-commit-msg"))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, string(got))
}

func TestHookLifecycleEmptyCommentRegression(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "commit-msg")
	original := "#!/bin/sh\n# \nexit 0\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o755))
	require.NoError(t, Install(dir))
	statuses, err := Status(dir)
	require.NoError(t, err)
	require.Len(t, statuses, 2)
	assert.True(t, statuses[1].Installed)
	require.NoError(t, Uninstall(dir))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, string(got))
}
