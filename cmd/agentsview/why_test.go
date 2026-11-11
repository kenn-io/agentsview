package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	gitlog "go.kenn.io/agentsview/internal/db/git"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/testutil"
)

func TestWhyRemoteArchive(t *testing.T) {
	dataDir := newAgentDataDir(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("remote-token\n"), 0o600))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer remote-token", r.Header.Get("Authorization"))
		var body struct {
			SHAs []string `json:"shas"`
		}
		assert.NoError(t, json.UnmarshalRead(r.Body, &body))
		assert.Equal(t, []string{"abc1234"}, body.SHAs)
		writeJSONResponse(w, `{"commits":[{"sha":"abc1234","links":[{"session_id":"remote-session","sha":"abc1234","branch":"main","subject":"Remote change","repository":"/work/example","message_ordinal":1,"call_index":0,"tool_use_id":"remote-commit","committed_at":"2026-01-02T03:04:05Z","source":"transcript","missing":false,"ambiguous":false,"turn":{"ordinal":0,"text":"Make the remote change"}}]}]}`)
	}))
	t.Cleanup(server.Close)
	out, err := executeCommand(newRootCommand(), "why", "--commit", "abc1234", "--server", server.URL, "--server-token-file", tokenFile, "--json")
	require.NoError(t, err)
	got := decodeCLIJSON[whyResult](t, out)
	require.Len(t, got.Commits, 1)
	require.Len(t, got.Commits[0].Links, 1)
	assert.Equal(t, "remote-session", got.Commits[0].Links[0].SessionID)
	assert.True(t, strings.HasSuffix(out, "\n"), "JSON output must end with a newline")
	assertNoLocalSessionsDB(t, dataDir)
}

func TestWhyPGArchive(t *testing.T) {
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")
	pgDB := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "pg.db"))
	stub := stubPGReadStore(t, pgDB)
	out, err := executeCommand(newRootCommand(), "why", "--commit", "abc1234", "--pg", "--json")
	require.NoError(t, err)
	got := decodeCLIJSON[whyResult](t, out)
	require.Len(t, got.Commits, 1)
	assert.Equal(t, "abc1234", got.Commits[0].SHA)
	assert.True(t, stub.Opened)
	assert.Equal(t, "postgres://example.test/agentsview", stub.PG.URL)
	assert.True(t, stub.CleanupCalled)
	assertNoLocalSessionsDB(t, dataDir)
}

func TestWhyWriteFailure(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSession(t, dataDir, "claude:writer", "project-a")
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

func TestWhyHumanCommitEvidence(t *testing.T) {
	for _, state := range []string{"active", "trashed", "deleted"} {
		t.Run(state, func(t *testing.T) {
			dataDir := newAgentDataDir(t)
			seedSession(t, dataDir, "s-commit", "project-a")
			d, err := db.Open(t.Context(), sessionsDBPath(dataDir))
			require.NoError(t, err)
			t.Cleanup(func() { _ = d.Close() })
			require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
				{SessionID: "s-commit", Ordinal: 0, Role: "user", Content: "Fix the parser"},
				{SessionID: "s-commit", Ordinal: 1, Role: "assistant", Timestamp: "2026-01-02T03:04:05Z", ToolCalls: []db.ToolCall{
					{SessionID: "s-commit", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"true"}`},
					{SessionID: "s-commit", CallIndex: 1, ToolName: "Bash", Category: "Bash", ToolUseID: "commit-call-two", InputJSON: `{"command":"git commit -m fix","workdir":"/work/example"}`, ResultContent: "[feature/parser abc1234] Fix parser"},
				}},
			}))
			switch state {
			case "trashed":
				require.NoError(t, d.SoftDeleteSession(t.Context(), "s-commit"))
			case "deleted":
				require.NoError(t, d.DeleteSession(t.Context(), "s-commit"))
			}
			require.NoError(t, d.Close())
			out, err := executeCommand(newRootCommand(), "why", "--commit", "abc1234")
			require.NoError(t, err)
			assert.Contains(t, out, "transcript  s-commit  /work/example  Fix parser")
			assert.Contains(t, out, "feature/parser")
			assert.Contains(t, out, "2026-01-02T03:04:05Z")
			assert.Contains(t, out, "message 1  call 1  commit-call-two")
			if state == "active" {
				assert.Contains(t, out, "    Fix the parser")
			} else {
				assert.Contains(t, out, "(deleted or in trash)")
				assert.NotContains(t, out, "Fix the parser")
			}
		})
	}
}

func TestWhyHumanCommitMessageEvidence(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSession(t, dataDir, "s-commit", "project-a")
	d, err := db.Open(t.Context(), sessionsDBPath(dataDir))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "s-commit", Ordinal: 0, Role: "user", Content: "Fix the parser", Timestamp: "2026-10-30T10:09:00Z"},
	}))
	_, err = d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{
		{SHA: strings.Repeat("e", 40), Subject: "Editor commit", CommittedAt: "2026-10-30T10:10:00Z", Sessions: []string{"claude:s-commit"}},
		{SHA: strings.Repeat("f", 40), Subject: "Idle commit", CommittedAt: "2026-10-30T12:00:00Z", Sessions: []string{"claude:s-commit"}},
	})
	require.NoError(t, err)
	require.NoError(t, d.Close())

	t.Run("nearby message is an estimated prompt", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(), "why", "--commit", strings.Repeat("e", 12))
		require.NoError(t, err)
		assert.Contains(t, out, "  commit message  s-commit  repo  Editor commit")
		assert.Contains(t, out, "2026-10-30T10:10:00Z  message 0\n")
		assert.Contains(t, out, "    Fix the parser (estimated)\n")
	})
	t.Run("no nearby message omits unknown positions", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(), "why", "--commit", strings.Repeat("f", 12))
		require.NoError(t, err)
		assert.Contains(t, out, "  commit message  s-commit  repo  Idle commit")
		assert.Contains(t, out, "2026-10-30T12:00:00Z\n")
	})
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
	testutil.IsolateGit(t)
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
	seedSession(t, dataDir, "claude:recorded", "project-a")
	d, err := db.Open(t.Context(), sessionsDBPath(dataDir))
	require.NoError(t, err)
	recordFixtureInstallation(t, d)
	messages := []db.Message{}
	for i, entry := range []struct{ sha, prompt string }{{firstSHA, "Change the second line"}, {secondSHA, "Indent the third line"}} {
		messages = append(messages,
			db.Message{SessionID: "claude:recorded", Ordinal: 2 * i, Role: "user", Content: entry.prompt},
			db.Message{SessionID: "claude:recorded", Ordinal: 2*i + 1, Role: "assistant", HasToolUse: true, ToolCalls: []db.ToolCall{{SessionID: "claude:recorded", ToolName: "Bash", Category: "Bash", ToolUseID: fmt.Sprintf("commit-%d", i), InputJSON: `{"command":"git commit -am change","workdir":"/work/example"}`, ResultContent: fmt.Sprintf("[main %s] Accept recorded prompt %d", entry.sha[:7], i)}}},
		)
	}
	require.NoError(t, d.InsertMessages(t.Context(), messages))
	require.NoError(t, d.Close())
	t.Chdir(repo)
	t.Run("JSON links match commits", func(t *testing.T) {
		outText, err := executeCommand(newRootCommand(), "why", "file.go:2-3", "--format", "json")
		require.NoError(t, err)
		got := decodeCLIJSON[whyResult](t, outText)
		require.Len(t, got.Commits, 2)
		for i, want := range []struct{ sha, prompt string }{{firstSHA, "Change the second line"}, {secondSHA, "Indent the third line"}} {
			assert.Equal(t, want.sha, got.Commits[i].SHA)
			require.Len(t, got.Commits[i].Links, 1)
			link := got.Commits[i].Links[0]
			assert.Equal(t, "transcript", link.Source)
			assert.Equal(t, "claude:recorded", link.SessionID)
			require.NotNil(t, link.Turn)
			assert.Equal(t, want.prompt, link.Turn.Text)
		}
	})

	require.NoError(t, os.WriteFile(path, []byte("first\nchanged\n    third\nuncommitted\n"), 0o600))
	t.Run("JSON uncommitted lines", func(t *testing.T) {
		outText, err := executeCommand(newRootCommand(), "why", "file.go", "--json")
		require.NoError(t, err)
		got := decodeCLIJSON[whyResult](t, outText)
		require.NotNil(t, got.Uncommitted)
		assert.Equal(t, 1, *got.Uncommitted)
	})

	t.Run("human output uses stdout", func(t *testing.T) {
		outText, err := executeCommand(newRootCommand(), "why", "file.go:2")
		require.NoError(t, err)
		assert.Contains(t, outText, "Change second line  (1 line)")
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
		assert.Contains(t, stdout, "message 1  call 0  commit-0\n    Change the second line\n")
		assert.Contains(t, stdout, "message 3  call 0  commit-1\n    Indent the third line\n")
		assert.Contains(t, stdout, "No recorded session.")
		assert.Contains(t, stdout, "1 selected lines are not committed yet.")
		assert.Empty(t, stderr)
	})

	t.Chdir(t.TempDir())
	t.Run("commit JSON shape", func(t *testing.T) {
		outText, err := executeCommand(newRootCommand(), "why", "--commit", firstSHA[:7], "--json")
		require.NoError(t, err)
		got := decodeCLIJSON[whyResult](t, outText)
		require.Len(t, got.Commits, 1)
		require.Len(t, got.Commits[0].Links, 1)
		require.NotNil(t, got.Commits[0].Links[0].Turn)
		assert.Equal(t, "Change the second line", got.Commits[0].Links[0].Turn.Text)
		assert.NotContains(t, outText, `"uncommitted"`)
		assert.NotContains(t, outText, `"lines"`)
		shape := decodeCLIJSON[struct {
			Commits []map[string]any `json:"commits"`
		}](t, outText)
		assert.NotContains(t, shape.Commits[0], "subject")
	})
	t.Run("absolute path outside checkout", func(t *testing.T) {
		outText, err := executeCommand(newRootCommand(), "why", path+":2-3", "--json")
		require.NoError(t, err)
		got := decodeCLIJSON[whyResult](t, outText)
		assert.Len(t, got.Commits, 2)
	})
	t.Run("parent of symlink uses target repository", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows resolves link/.. before following directory symlinks")
		}
		otherRepo := initGitRepoOnBranch(t, "main")
		runGit(t, otherRepo, "config", "user.name", "Test User")
		runGit(t, otherRepo, "config", "user.email", "test@example.com")
		runGit(t, otherRepo, "config", "commit.gpgsign", "false")
		require.NoError(t, os.WriteFile(filepath.Join(otherRepo, "file.go"), []byte("unrelated\nfile\nlines\n"), 0o600))
		runGit(t, otherRepo, "add", "file.go")
		runGit(t, otherRepo, "commit", "-qm", "Unrelated file")
		subdir := filepath.Join(repo, "subdir")
		require.NoError(t, os.Mkdir(subdir, 0o700))
		requireSymlinkOrSkip(t, subdir, filepath.Join(otherRepo, "link"))
		t.Chdir(otherRepo)

		// Keep .. intact so the filesystem traverses the symlink first.
		outText, err := executeCommand(newRootCommand(), "why", "link/../file.go:2-3", "--json")
		require.NoError(t, err)
		got := decodeCLIJSON[whyResult](t, outText)
		require.Len(t, got.Commits, 2)
		assert.Equal(t, firstSHA, got.Commits[0].SHA)
		assert.Equal(t, secondSHA, got.Commits[1].SHA)
	})
	t.Run("argument validation", func(t *testing.T) {
		_, err := executeCommand(newRootCommand(), "why", "--commit", "invalid")
		require.ErrorContains(t, err, db.ErrInvalidCommitSHA.Error())
		for _, args := range [][]string{{"why"}, {"why", "file.go", "--commit", firstSHA}} {
			_, err := executeCommand(newRootCommand(), args...)
			require.ErrorContains(t, err, "exactly one")
		}
	})
}
