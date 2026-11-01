package gittrailer

import (
	"context"
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

func trailerRepo(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_CONFIG_PARAMETERS", "")
	gitTest(t, repo, "init", "-q")
	t.Chdir(repo)
	return repo
}

func gitTest(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", repo}, args...)...)
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, b)
	return strings.TrimSpace(string(b))
}

func messageFile(t *testing.T, repo, text string) string {
	t.Helper()
	path := filepath.Join(repo, "message")
	require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	return path
}

func messageText(t *testing.T, path string) string {
	t.Helper()
	b, e := os.ReadFile(path)
	require.NoError(t, e)
	return string(b)
}

func TestStampMessage(t *testing.T) {
	repo := trailerRepo(t)
	tests := []struct {
		name, text string
		session    Session
		want       string
	}{
		{"new", "Subject\n", Session{"pi", "one"}, "Subject\n\nAgent-Session: pi:one\n"},
		{"subject-only minimized regression", "Subject\n\n\n", Session{"pi", "0"}, "Subject\n\nAgent-Session: pi:0\n\n\n"},
		{"another session", "Subject\n\nAgent-Session: pi:one\n", Session{"pi", "two"}, "Subject\n\nAgent-Session: pi:one\nAgent-Session: pi:two\n"},
		{"custom case distinction", "Subject\n\nAgent-Session: pi:ONE\n", Session{"pi", "one"}, "Subject\n\nAgent-Session: pi:ONE\nAgent-Session: pi:one\n"},
		{"lowercase key", "Subject\n\nagent-session: pi:one\n", Session{"pi", "one"}, "Subject\n\nagent-session: pi:one\nAgent-Session: pi:one\n"},
		{"body duplicate", "Subject\n\nAgent-Session:\tpi:one \t\n\nLater body\n", Session{"pi", "one"}, "Subject\n\nAgent-Session:\tpi:one \t\n\nLater body\n"},
		{"comments only", "# instructions\n\n", Session{"pi", "one"}, "# instructions\n\n"},
		{"verbose empty", "# instructions\n# ------------------------ >8 ------------------------\ndiff --git a/x b/x\n", Session{"pi", "one"}, "# instructions\n# ------------------------ >8 ------------------------\ndiff --git a/x b/x\n"},
		{"discarded duplicate", "Subject\n# ------------------------ >8 ------------------------\nAgent-Session: pi:one\n", Session{"pi", "one"}, "Subject\n\nAgent-Session: pi:one\n# ------------------------ >8 ------------------------\nAgent-Session: pi:one\n"},
		{"long lines", "Subject\n\n" + strings.Repeat("x", 70000) + "\n\nAgent-Session: pi:one\n", Session{"pi", "one"}, "Subject\n\n" + strings.Repeat("x", 70000) + "\n\nAgent-Session: pi:one\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := messageFile(t, repo, tt.text)
			require.NoError(t, Stamp(t.Context(), path, nil, tt.session))
			assert.Equal(t, tt.want, messageText(t, path))
			require.NoError(t, Stamp(t.Context(), path, nil, tt.session))
			assert.Equal(t, tt.want, messageText(t, path))
		})
	}
}

func TestStampNativeIDCaseIsPreserved(t *testing.T) {
	repo := trailerRepo(t)
	path := messageFile(t, repo, "Subject\n\nAgent-Session: codex:"+sessionUUID+"\n")
	require.NoError(t, Stamp(t.Context(), path, nil, Session{"codex", strings.ToUpper(sessionUUID)}))
	assert.Equal(t, "Subject\n\nAgent-Session: codex:"+sessionUUID+"\nAgent-Session: codex:"+strings.ToUpper(sessionUUID)+"\n", messageText(t, path))
}

func TestStampIgnoresTrailerConfiguration(t *testing.T) {
	repo := trailerRepo(t)
	sentinel := filepath.Join(repo, "callback-ran")
	for _, setting := range [][2]string{
		{"trailer.separators", "%"},
		{"trailer.agent-session.key", "Session"},
		{"trailer.agent-session.ifmissing", "doNothing"},
		{"trailer.agent-session.cmd", "printf replacement"},
		{"trailer.other.command", "touch '" + sentinel + "'"},
		{"trailer.alias.key", "Agent-Session"},
	} {
		gitTest(t, repo, "config", setting[0], setting[1])
	}
	path := messageFile(t, repo, "Subject\n")
	require.NoError(t, Stamp(t.Context(), path, nil, Session{"pi", "one"}))
	assert.Equal(t, "Subject\n\nAgent-Session: pi:one\n", messageText(t, path))
	assert.NoFileExists(t, sentinel)
}

func TestStampTemplateContext(t *testing.T) {
	repo := trailerRepo(t)
	source := "template"
	session := Session{"pi", "one"}
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "untouched", true: "edited"}[changed], func(t *testing.T) {
			path := messageFile(t, repo, "Describe the change here\n# instruction\n")
			require.NoError(t, Stamp(t.Context(), path, &source, session))
			assert.Equal(t, "Describe the change here\n# instruction\n", messageText(t, path))
			if changed {
				require.NoError(t, os.WriteFile(path, []byte("Changed message\n"), 0o600))
			}
			require.NoError(t, Stamp(t.Context(), path, nil, session))
			if changed {
				assert.Equal(t, "Changed message\n\nAgent-Session: pi:one\n", messageText(t, path))
			} else {
				assert.Equal(t, "Describe the change here\n# instruction\n", messageText(t, path))
			}
			assert.NoFileExists(t, path+".agentsview-prepare")
		})
	}
	path := messageFile(t, repo, "Subject\n")
	state := path + ".agentsview-prepare"
	require.NoError(t, os.WriteFile(state, []byte("unowned content"), 0o600))
	require.Error(t, Stamp(t.Context(), path, &source, session))
	assert.Equal(t, "Subject\n", messageText(t, path))
	assert.Equal(t, "unowned content", messageText(t, state))
}

func TestStampSkipsReplayAndPrepareSources(t *testing.T) {
	repo := trailerRepo(t)
	session := Session{"pi", "one"}
	for _, marker := range []string{"rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD"} {
		t.Run(marker, func(t *testing.T) {
			p := gitTest(t, repo, "rev-parse", "--git-path", marker)
			require.NoError(t, os.WriteFile(p, []byte("fixture"), 0o600))
			path := messageFile(t, repo, "Subject\n")
			require.NoError(t, Stamp(t.Context(), path, nil, session))
			assert.Equal(t, "Subject\n", messageText(t, path))
			require.NoError(t, os.Remove(p))
		})
	}
	for _, source := range []string{"editor", "commit", "merge", "squash"} {
		path := messageFile(t, repo, "Subject\n")
		require.NoError(t, Stamp(t.Context(), path, &source, session))
		assert.Equal(t, "Subject\n", messageText(t, path))
	}
}

func TestStampCommentSettings(t *testing.T) {
	repo := trailerRepo(t)
	gitTest(t, repo, "config", "core.commentChar", ";")
	path := messageFile(t, repo, "; instructions\n; ------------------------ >8 ------------------------\ndiff --git a/a b/a\n")
	require.NoError(t, Stamp(t.Context(), path, nil, Session{"pi", "one"}))
	assert.NotContains(t, messageText(t, path), "Agent-Session")
	gitTest(t, repo, "config", "core.commentChar", "auto")
	path = messageFile(t, repo, "Subject\n")
	require.Error(t, Stamp(t.Context(), path, nil, Session{"pi", "one"}))
	assert.Equal(t, "Subject\n", messageText(t, path))
}

func TestStampFailurePreservesMessage(t *testing.T) {
	repo := trailerRepo(t)
	session := Session{"pi", "one"}
	path := messageFile(t, repo, "Subject\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Error(t, Stamp(ctx, path, nil, session))
	assert.Equal(t, "Subject\n", messageText(t, path))
	require.Error(t, Stamp(t.Context(), path, nil, Session{"pi", "bad\nvalue"}))
	assert.Equal(t, "Subject\n", messageText(t, path))
	require.Error(t, Stamp(t.Context(), filepath.Join(repo, "missing"), nil, session))
	path = messageFile(t, repo, strings.Repeat("x", (2<<20)+1))
	require.Error(t, Stamp(t.Context(), path, nil, session))
	assert.Len(t, messageText(t, path), (2<<20)+1)
	path = messageFile(t, repo, "subject\x00body\n")
	require.Error(t, Stamp(t.Context(), path, nil, session))
	assert.Equal(t, "subject\x00body\n", messageText(t, path))
}

func TestStampIdempotenceProperty(t *testing.T) {
	repo := trailerRepo(t)
	hegel.Test(t, func(ht *hegel.T) {
		body := hegel.Draw(ht, hegel.Text())
		id := hegel.Draw(ht, hegel.FromRegex(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`, true))
		path := filepath.Join(repo, "property-message")
		text := "Subject\n\n" + body + "\n"
		require.NoError(ht, os.WriteFile(path, []byte(text), 0o600))
		err := Stamp(ht.Context(), path, nil, Session{"pi", id})
		if strings.ContainsRune(body, 0) {
			require.Error(ht, err)
			b, e := os.ReadFile(path)
			require.NoError(ht, e)
			assert.Equal(ht, text, string(b))
			return
		}
		require.NoError(ht, err)
		first, e := os.ReadFile(path)
		require.NoError(ht, e)
		require.NoError(ht, Stamp(ht.Context(), path, nil, Session{"pi", id}))
		second, e := os.ReadFile(path)
		require.NoError(ht, e)
		assert.Equal(ht, string(first), string(second))
		assert.Contains(ht, string(second), "Agent-Session: pi:"+id+"\n")
	}, hegel.WithTestCases(100), hegel.WithDatabase(""))
}

func TestGitOutputIsBounded(t *testing.T) {
	repo := trailerRepo(t)
	path := filepath.Join(repo, ".git", "config")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = file.WriteString("\n[oversized]\nvalue = " + strings.Repeat("x", maxGitOutput+1) + "\n")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	_, err = runGit(t.Context(), "", nil, nil, "config", "--get", "oversized.value")
	require.Error(t, err)
}

func TestRetainedMessageBoundaryProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		discarded := hegel.Draw(ht, hegel.Text())
		prefix := hegel.Draw(ht, hegel.SampledFrom([]string{"#", ";", "##"}))
		text := "Subject\n\nAgent-Session: pi:Exact_ID\n"
		suffix := prefix + " ------------------------ >8 ------------------------\n" + discarded
		head, tail := retainedMessage([]byte(text+suffix), prefix)
		assert.Equal(ht, text, string(head))
		assert.Equal(ht, suffix, string(tail))
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}

func TestExactRetainedSessionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.FromRegex(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`, true))
		before := hegel.Draw(ht, hegel.Text())
		after := hegel.Draw(ht, hegel.Text())
		text := "> " + strings.ReplaceAll(before, "\n", "\n> ") + "\nAgent-Session: \tpi:" + id + " \t\n> " + strings.ReplaceAll(after, "\n", "\n> ")
		assert.True(ht, containsSession([]byte(text), Session{"pi", id}))
		assert.False(ht, containsSession([]byte(text), Session{"other", id}))
	}, hegel.WithTestCases(2000), hegel.WithDatabase(""))
}

func TestSignoffsDoNotAuthorizeEmptyOrUntouchedMessages(t *testing.T) {
	repo := trailerRepo(t)
	signoff := "Signed-off-by: Example <example@example.com>\n"
	path := messageFile(t, repo, "\n"+signoff)
	require.NoError(t, Stamp(t.Context(), path, nil, Session{"pi", "one"}))
	assert.Equal(t, "\n"+signoff, messageText(t, path))
	path = messageFile(t, repo, "Describe the change\n")
	source := "template"
	require.NoError(t, Stamp(t.Context(), path, &source, Session{"pi", "one"}))
	require.NoError(t, os.WriteFile(path, []byte("Describe the change\n\n"+signoff), 0o600))
	require.NoError(t, Stamp(t.Context(), path, nil, Session{"pi", "one"}))
	assert.Equal(t, "Describe the change\n\n"+signoff, messageText(t, path))
}

func TestCommentAliasesUseEffectiveOrder(t *testing.T) {
	repo := trailerRepo(t)
	gitTest(t, repo, "config", "--global", "core.commentString", "//")
	gitTest(t, repo, "config", "core.commentChar", "#")
	text := "Subject\n# ------------------------ >8 ------------------------\ndiff --git a/a b/a\n"
	path := messageFile(t, repo, text)
	require.NoError(t, Stamp(t.Context(), path, nil, Session{"pi", "one"}))
	assert.Equal(t, "Subject\n\nAgent-Session: pi:one\n# ------------------------ >8 ------------------------\ndiff --git a/a b/a\n", messageText(t, path))
	gitTest(t, repo, "config", "core.commentChar", "//")
	path = messageFile(t, repo, "// comment only\n")
	require.NoError(t, Stamp(t.Context(), path, nil, Session{"pi", "one"}))
	assert.Equal(t, "// comment only\n", messageText(t, path))
	gitTest(t, repo, "config", "core.commentString", "auto")
	path = messageFile(t, repo, "Subject\n")
	require.Error(t, Stamp(t.Context(), path, nil, Session{"pi", "one"}))
	assert.Equal(t, "Subject\n", messageText(t, path))
}

func TestManagedStampProcess(t *testing.T) {
	if os.Getenv("AV_TRAILER_HOOK_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) > 0 {
		args = args[1:]
	}
	require.NotEmpty(t, args)
	require.Equal(t, "git-trailer", args[0])
	args = args[1:]
	require.NotEmpty(t, args)
	record, err := os.OpenFile(os.Getenv("AV_TRAILER_TEST_CALLS"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	require.NoError(t, err)
	_, err = record.WriteString("call\n")
	require.NoError(t, err)
	require.NoError(t, record.Close())
	if len(args) > 0 {
		var source *string
		if len(args) > 1 {
			source = &args[1]
		}
		if len(args) > 2 {
			_ = StampHook(t.Context(), args[0], args[1], Session{"pi", "one"}, args[2])
		} else {
			_ = Stamp(t.Context(), args[0], source, Session{"pi", "one"})
		}
	}
	os.Exit(0) // Match the real hook command's fail-open boundary.
}

func managedHookFixture(t *testing.T) (repo, hooks, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("direct shell-hook execution uses Unix process parents")
	}
	repo = trailerRepo(t)
	hooks = t.TempDir()
	bin := t.TempDir()
	executable, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("AV_TRAILER_TEST_CALLS", filepath.Join(t.TempDir(), "calls"))
	t.Setenv("AV_TRAILER_HOOK_HELPER", "1")
	t.Setenv("AV_TRAILER_TEST_BINARY", executable)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "agentsview"), []byte("#!/bin/sh\nexec \"$AV_TRAILER_TEST_BINARY\" -test.run=^TestManagedStampProcess$ -- \"$@\"\n"), 0o755))
	require.NoError(t, Install(hooks))
	path = messageFile(t, repo, "Describe the change\n")
	return repo, hooks, path
}

func runManagedHook(t *testing.T, hooks, name, path string, args ...string) {
	t.Helper()
	before, _ := os.ReadFile(os.Getenv("AV_TRAILER_TEST_CALLS"))
	cmd := exec.CommandContext(t.Context(), filepath.Join(hooks, name), append([]string{path}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	after, err := os.ReadFile(os.Getenv("AV_TRAILER_TEST_CALLS"))
	require.NoError(t, err, string(out))
	assert.Len(t, after, len(before)+len("call\n"), string(out))
}

// Emulate a Git release that retains unknown config keys but does not implement
// commentString: its stripspace leaves the literal capability probe untouched.
func TestIgnoredCommentStringUsesCommentChar(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell Git capability fixture")
	}
	repo := trailerRepo(t)
	gitTest(t, repo, "config", "core.commentChar", "#")
	gitTest(t, repo, "config", "core.commentString", "//")
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = -c ] && [ \"$2\" = 'core.commentChar=#' ] && [ \"$3\" = -c ] && [ \"$4\" = 'core.commentString=//' ]; then\ncat\nexit 0\nfi\nexec " + ("'" + strings.ReplaceAll(realGit, "'", "'\\''") + "'") + " \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := commentPrefix(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "#", got)
}
