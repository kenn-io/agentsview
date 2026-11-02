package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/testutil"
)

func TestWhyJSONWithGitOutputEncoding(t *testing.T) {
	testutil.IsolateGitEnv(t)
	_, err := exec.LookPath("git")
	require.NoError(t, err)
	repo := initGitRepoOnBranch(t, "main")
	runGit(t, repo, "config", "user.name", "Test User")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "commit.gpgsign", "false")
	runGit(t, repo, "config", "i18n.logOutputEncoding", "ISO-8859-1")
	path := filepath.Join(repo, "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("first\n"), 0o600))
	runGit(t, repo, "add", "a.txt")
	runGit(t, repo, "commit", "-qm", "Fix café")
	dataDir := newAgentDataDir(t)
	seedSessionArchiveRows(t, dataDir, sessionSeed{id: "claude:encoding", project: "project-a"})
	out, err := executeCommand(newRootCommand(), "why", path, "--json")
	require.NoError(t, err)
	got := decodeCLIJSON[whyResult](t, out)
	require.Len(t, got.Commits, 1)
	assert.Equal(t, "Fix café", got.Commits[0].Subject)
}

func TestWhyWriteFailure(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSessionArchiveRows(t, dataDir, sessionSeed{id: "claude:writer", project: "project-a"})
	for _, format := range []string{"human", "json"} {
		t.Run(format, func(t *testing.T) {
			sentinel := errors.New("output unavailable")
			root := newRootCommand()
			root.SetOut(&failingRawSyncWriter{err: sentinel})
			root.SetArgs([]string{"why", "--commit", "abcdef1234", "--format", format})
			require.ErrorIs(t, root.Execute(), sentinel)
		})
	}
}

func TestSplitWhyPath(t *testing.T) {
	for _, tc := range []struct {
		arg, path  string
		start, end int
		invalid    bool
	}{
		{"a.go:3", "a.go", 3, 3, false},
		{"a.go:3-5", "a.go", 3, 5, false},
		{"a.go", "a.go", 0, 0, false},
		{`C:\x\a.go:2`, `C:\x\a.go`, 2, 2, false},
		{`C:\x\a.go`, `C:\x\a.go`, 0, 0, false},
		{"a.go:5-3", "", 0, 0, true},
		{"a.go:0", "", 0, 0, true},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			path, start, end, err := splitWhyPath(tc.arg)
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.path, path)
			assert.Equal(t, tc.start, start)
			assert.Equal(t, tc.end, end)
		})
	}
}

func TestWhyRecordedPrompts(t *testing.T) {
	testutil.IsolateGitEnv(t)
	_, err := exec.LookPath("git")
	require.NoError(t, err)
	repo := initGitRepoOnBranch(t, "main")
	runGit(t, repo, "config", "user.name", "Test User")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "commit.gpgsign", "false")
	path := filepath.Join(repo, "file.go")
	require.NoError(t, os.WriteFile(path, []byte("first\nsecond\nthird\n"), 0o600))
	runGit(t, repo, "add", "file.go")
	runGit(t, repo, "commit", "-qm", "Initial lines")
	require.NoError(t, os.WriteFile(path, []byte("first\nchanged\nthird\n"), 0o600))
	runGit(t, repo, "commit", "-qam", "Change second line")
	cmd := exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	require.NoError(t, err)
	firstSHA := strings.TrimSpace(string(out))
	require.NoError(t, os.WriteFile(path, []byte("first\nchanged\n    third\n"), 0o600))
	runGit(t, repo, "commit", "-qam", "Indent third line")
	cmd = exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD")
	cmd.Dir = repo
	out, err = cmd.Output()
	require.NoError(t, err)
	secondSHA := strings.TrimSpace(string(out))

	dataDir := newAgentDataDir(t)
	// Seed only archive rows so the command must answer without a daemon.
	seedSessionArchiveRows(t, dataDir, sessionSeed{id: "claude:recorded", project: "project-a"})
	d, err := db.Open(t.Context(), sessionsDBPath(dataDir))
	require.NoError(t, err)
	recordFixtureInstallation(t, d)
	messages := []db.Message{}
	for i, entry := range []struct{ sha, prompt string }{{firstSHA, "Change the second line"}, {secondSHA, "Indent the third line"}} {
		messages = append(messages,
			db.Message{SessionID: "claude:recorded", Ordinal: 2 * i, Role: "user", Content: entry.prompt},
			db.Message{SessionID: "claude:recorded", Ordinal: 2*i + 1, Role: "assistant", HasToolUse: true, ToolCalls: []db.ToolCall{{SessionID: "claude:recorded", ToolName: "Bash", Category: "Bash", ToolUseID: fmt.Sprintf("commit-%d", i), InputJSON: `{"command":"git commit -am change"}`, ResultContent: fmt.Sprintf("[main %s] change", entry.sha[:7])}}},
		)
	}
	require.NoError(t, d.InsertMessages(t.Context(), messages))
	require.NoError(t, d.Close())
	t.Chdir(repo)
	outText, err := executeCommand(newRootCommand(), "why", "file.go:2-3", "--format", "json")
	require.NoError(t, err)
	got := decodeCLIJSON[whyResult](t, outText)
	require.Len(t, got.Commits, 2)
	for i, want := range []struct{ sha, prompt string }{{firstSHA, "Change the second line"}, {secondSHA, "Indent the third line"}} {
		assert.Equal(t, want.sha, got.Commits[i].SHA)
		require.NotNil(t, got.Commits[i].Lines)
		assert.Equal(t, 1, *got.Commits[i].Lines)
		require.Len(t, got.Commits[i].Links, 1)
		link := got.Commits[i].Links[0]
		assert.Equal(t, "transcript", link.Source)
		assert.Equal(t, "claude:recorded", link.SessionID)
		require.NotNil(t, link.Turn)
		assert.Equal(t, want.prompt, link.Turn.Text)
	}
	outText, err = executeCommand(newRootCommand(), "why", "file.go")
	require.NoError(t, err)
	assert.Contains(t, outText, "    Change the second line")
	assert.Contains(t, outText, "    Indent the third line")
	assert.Contains(t, outText, "No recorded session.")
	require.NoError(t, os.WriteFile(path, []byte("first\nchanged\n    third\nuncommitted\n"), 0o600))
	outText, err = executeCommand(newRootCommand(), "why", "file.go", "--json")
	require.NoError(t, err)
	got = decodeCLIJSON[whyResult](t, outText)
	require.NotNil(t, got.Uncommitted)
	assert.Equal(t, 1, *got.Uncommitted)
	assert.Len(t, got.Commits, 3)

	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			// Leave Cobra's output unset to exercise the CLI's default streams.
			root := newRootCommand()
			root.SetArgs([]string{"why", "file.go"})
			require.NoError(t, root.Execute())
		})
	})
	assert.Contains(t, stdout, "Initial lines")
	assert.Contains(t, stdout, "    Change the second line")
	assert.Contains(t, stdout, "    Indent the third line")
	assert.Contains(t, stdout, "No recorded session.")
	assert.Contains(t, stdout, "1 selected lines are not committed yet.")
	assert.Empty(t, stderr)

	t.Chdir(t.TempDir())
	outText, err = executeCommand(newRootCommand(), "why", "--commit", firstSHA[:7], "--json")
	require.NoError(t, err)
	got = decodeCLIJSON[whyResult](t, outText)
	require.Len(t, got.Commits, 1)
	require.Len(t, got.Commits[0].Links, 1)
	assert.Equal(t, "Change the second line", got.Commits[0].Links[0].Turn.Text)
	assert.NotContains(t, outText, `"uncommitted"`)
	assert.NotContains(t, outText, `"lines"`)
	shape := decodeCLIJSON[struct {
		Commits []map[string]any `json:"commits"`
	}](t, outText)
	assert.NotContains(t, shape.Commits[0], "subject")
	// An absolute path also works outside a checkout.
	_, err = executeCommand(newRootCommand(), "why", path+":2-3", "--json")
	require.NoError(t, err)
	_, err = executeCommand(newRootCommand(), "why", "--commit", "invalid")
	require.ErrorIs(t, err, db.ErrInvalidCommitSHA)
	for _, args := range [][]string{{"why"}, {"why", "file.go", "--commit", firstSHA}} {
		_, err := executeCommand(newRootCommand(), args...)
		require.ErrorContains(t, err, "exactly one")
	}
	registerSQLiteDaemonRuntime(t, dataDir)
	outText, err = executeCommand(newRootCommand(), "why", "--commit", secondSHA[:7], "--json")
	require.NoError(t, err)
	got = decodeCLIJSON[whyResult](t, outText)
	require.Len(t, got.Commits, 1)
	require.Len(t, got.Commits[0].Links, 1)
	require.NotNil(t, got.Commits[0].Links[0].Turn)
	assert.Equal(t, "Indent the third line", got.Commits[0].Links[0].Turn.Text)
}

func TestWhyCommitEvidence(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSessionArchiveRows(t, dataDir, sessionSeed{id: "claude:evidence", project: "project-a"})
	d, err := db.Open(t.Context(), sessionsDBPath(dataDir))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	recordFixtureInstallation(t, d)
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "claude:evidence", Ordinal: 0, Role: "user", Content: "Fix parser handling"},
		{SessionID: "claude:evidence", Ordinal: 1, Role: "assistant", HasToolUse: true, ToolCalls: []db.ToolCall{{
			SessionID: "claude:evidence", ToolName: "Bash", Category: "Bash", ToolUseID: "commit",
			InputJSON:     `{"command":"git commit -am change","workdir":"/work/example"}`,
			ResultContent: "[main abcdef1234] Accept late parser results",
		}}},
	}))
	out, err := executeCommand(newRootCommand(), "why", "--commit", "abcdef1234")
	require.NoError(t, err)
	assert.Contains(t, out, "/work/example")
	assert.Contains(t, out, "Accept late parser results")
}
