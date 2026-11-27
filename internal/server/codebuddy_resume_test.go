package server

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// fakeURLLauncherEnv makes the test binary act as a system URL launcher:
// "fail" exits non-zero, "hang" waits until it is killed, and any other
// value exits 0.
const fakeURLLauncherEnv = "AGENTSVIEW_FAKE_URL_LAUNCHER"

func init() {
	switch os.Getenv(fakeURLLauncherEnv) {
	case "":
		return
	case "fail":
		os.Exit(3)
	case "hang":
		interrupted := make(chan os.Signal, 1)
		signal.Notify(interrupted, os.Interrupt)
		<-interrupted
	}
	os.Exit(0)
}

// stubURLLauncher runs the fake launcher in mode in place of the system one,
// with limit as the launcher wait, and returns the link it was given.
func stubURLLauncher(t *testing.T, mode string, limit time.Duration) *string {
	t.Helper()
	var link string
	origCommand, origLimit := openURLCommand, openURLLaunchTimeout
	openURLCommand = func(ctx context.Context, _ string, l string) *exec.Cmd {
		link = l
		cmd := exec.CommandContext(ctx, os.Args[0])
		cmd.Env = append(os.Environ(), fakeURLLauncherEnv+"="+mode)
		// A launcher left running by a failed test must not outlive it.
		t.Cleanup(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
		return cmd
	}
	openURLLaunchTimeout = limit
	t.Cleanup(func() { openURLCommand, openURLLaunchTimeout = origCommand, origLimit })
	return &link
}

func TestOpenCodeBuddyIDEConversationReportsLauncherResult(t *testing.T) {
	home := t.TempDir()
	transcript := filepath.Join(home, "projects", "-work", "run-77.jsonl")
	catalog, err := sql.Open("sqlite3", filepath.Join(home, parser.CodeBuddyCatalogDBName))
	require.NoError(t, err)
	_, err = catalog.ExecContext(t.Context(), `
		CREATE TABLE conversations (id TEXT PRIMARY KEY, runtime_session_id TEXT, deleted_at INTEGER);
		INSERT INTO conversations VALUES ('conv-1', 'run-77', NULL)`)
	require.NoError(t, err)
	require.NoError(t, catalog.Close())
	session := &db.Session{Agent: string(parser.AgentCodeBuddy), FilePath: &transcript}

	for _, tc := range []struct {
		name     string
		mode     string
		limit    time.Duration
		launched bool
		wantErr  string
	}{
		{"launcher hands off the link", "succeed", time.Minute, true, ""},
		{"launcher exits non-zero", "fail", time.Minute, false, "desktop_launch_failed"},
		{"launcher stalls past the limit", "hang", time.Second, false, "desktop_launch_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link := stubURLLauncher(t, tc.mode, tc.limit)
			start := time.Now()
			out, err := (&Server{}).openCodeBuddyIDEConversation(t.Context(), session,
				"run-77", "codebuddycn", "CodeBuddy CN", "codebuddy --resume run-77", "/work")
			require.NoError(t, err)
			assert.Less(t, time.Since(start), 30*time.Second, "the launcher wait is bounded")
			assert.Equal(t, "codebuddycn://codebuddy-ide/conversation/open?id=conv-1", *link)
			assert.Equal(t, tc.launched, out.Body.Launched)
			assert.Equal(t, tc.wantErr, out.Body.Error)
		})
	}
}

func TestCodeBuddyIDEConversationURL(t *testing.T) {
	assert.Equal(t, "codebuddycn://codebuddy-ide/conversation/open?id=conv_1-a",
		codebuddyIDEConversationURL("codebuddycn", "conv_1-a"))
	assert.Equal(t, "codebuddycn-insiders://codebuddy-ide/conversation/open?id=abc",
		codebuddyIDEConversationURL("codebuddycn-insiders", "abc"))
	for _, bad := range []string{"", "a/b", "a&id=b", "a b", strings.Repeat("x", 129)} {
		assert.Empty(t, codebuddyIDEConversationURL("codebuddycn", bad), bad)
	}
}

func TestWithCodeBuddyHome(t *testing.T) {
	const cmd = "codebuddy --resume run-77"
	for _, tc := range []struct {
		goos, home, want string
	}{
		{"darwin", "/homes/cb", "CODEBUDDY_CONFIG_DIR='/homes/cb' " + cmd},
		{"linux", "/homes/it's", `CODEBUDDY_CONFIG_DIR='/homes/it'"'"'s' ` + cmd},
		{"windows", `C:\homes\cb two`,
			`Set-Item -Path Env:CODEBUDDY_CONFIG_DIR -Value 'C:\homes\cb two' && ` + cmd},
		{"windows", `C:\it's $x`,
			`Set-Item -Path Env:CODEBUDDY_CONFIG_DIR -Value 'C:\it''s $x' && ` + cmd},
	} {
		assert.Equal(t, tc.want, withCodeBuddyHome(cmd, tc.home, tc.goos), tc.goos+" "+tc.home)
	}
}

// A macOS launcher runs a POSIX shell, and Terminal and iTerm windows that
// AppleScript opens do not inherit the environment, so the launched command
// itself selects the home there; other platforms keep it plain and rely on
// the terminal's environment.
func TestCodeBuddyLaunchCommandPerPlatform(t *testing.T) {
	const cmd = "codebuddy --resume run-77"
	assert.Equal(t, "CODEBUDDY_CONFIG_DIR='/Users/u/my home' "+cmd,
		codebuddyLaunchCommand(cmd, "/Users/u/my home", "darwin"))
	assert.Equal(t, cmd, codebuddyLaunchCommand(cmd, "/home/u/.codebuddy", "linux"))
	assert.Equal(t, cmd, codebuddyLaunchCommand(cmd, "C:\\Users\\u\\.codebuddy", "windows"))
}

// A transcript path from another machine keeps that machine's separators,
// whatever this server's path rules are, so a Linux home stays a Linux path
// on a Windows server and a Windows home stays a Windows path elsewhere.
func TestCodeBuddyRemoteHomeKeepsSourceSeparators(t *testing.T) {
	for _, tc := range []struct {
		path, home string
		windows    bool
	}{
		{"/home/user/.codebuddy-work/projects/-work/conv-1.jsonl", "/home/user/.codebuddy-work", false},
		{"/home/user/.codebuddy/projects/-work/conv-1/subagents/agent-a.jsonl", "/home/user/.codebuddy", false},
		{`C:\Users\u\.codebuddy-work\projects\-work\conv-1.jsonl`, `C:\Users\u\.codebuddy-work`, true},
		{"C:/Users/u/.codebuddy/projects/-work/conv-1.jsonl", "C:/Users/u/.codebuddy", true},
	} {
		home, ok := codebuddyRemoteHome(tc.path)
		require.True(t, ok, tc.path)
		assert.Equal(t, tc.home, home)
		assert.Equal(t, tc.windows, codebuddyRemoteHomeIsWindows(home), tc.home)
	}
	for _, path := range []string{"/home/user/notes/conv-1.jsonl", "projects/-work/conv-1.jsonl", "/home/user/.codebuddy/projects/-work/conv-1.txt"} {
		_, ok := codebuddyRemoteHome(path)
		assert.False(t, ok, path)
	}
}

func TestOpenURLCommandPerPlatform(t *testing.T) {
	const link = "codebuddycn://codebuddy-ide/conversation/open?id=abc"
	for _, tc := range []struct {
		goos string
		want []string
	}{
		{"darwin", []string{"open", link}},
		{"windows", []string{"rundll32", "url.dll,FileProtocolHandler", link}},
		{"linux", []string{"xdg-open", link}},
	} {
		cmd := openURLCommand(t.Context(), tc.goos, link)
		assert.Equal(t, tc.want[0], filepath.Base(cmd.Args[0]), tc.goos)
		assert.Equal(t, tc.want[1:], cmd.Args[1:], tc.goos)
	}
}
