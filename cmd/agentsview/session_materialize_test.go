package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestSessionMaterializeRejectsRemoteBackends(t *testing.T) {
	for _, flag := range []string{"--server=http://127.0.0.1:1", "--pg", "--server-token-file="} {
		t.Run(flag, func(t *testing.T) {
			newAgentDataDir(t)
			_, err := executeCommand(newRootCommand(), "session", "materialize", "fixture", "--cwd", t.TempDir(), flag)
			require.ErrorContains(t, err, "requires the local archive")
		})
	}
}

func TestClaudeResumeCommandOverrides(t *testing.T) {
	defaultCommand := claudeResumeCommand("/checkout", "id", "", "", false)
	assert.NotContains(t, defaultCommand, "CLAUDE_CONFIG_DIR")
	assert.NotContains(t, defaultCommand, "CLAUDE_CODE_PROJECT_DIR_NAME")
	posix := claudeResumeCommand("/checkout path", "id", "/config path/", "project", false)
	assert.Contains(t, posix, "CLAUDE_CONFIG_DIR='/config path/'")
	assert.Contains(t, posix, "CLAUDE_CODE_PROJECT_DIR_NAME=project")
	assert.Contains(t, claudeResumeCommand("/checkout", "id", "/config", "", false), "CLAUDE_CODE_PROJECT_DIR_NAME=''")
	windows := claudeResumeCommand(`C:\checkout path`, "id", `C:\config path`, "project", true)
	assert.Contains(t, windows, `Set-Location -LiteralPath 'C:\checkout path'`)
	assert.Contains(t, windows, `$env:CLAUDE_CONFIG_DIR = 'C:\config path'`)
	assert.Contains(t, windows, `$env:CLAUDE_CODE_PROJECT_DIR_NAME = 'project'`)
	assert.Contains(t, windows, "finally")
	assert.NotContains(t, claudeResumeCommand(`C:\checkout`, "id", "", "", true), "CLAUDE_CONFIG_DIR")
}

func TestSessionMaterializeUsageOnly(t *testing.T) {
	newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_ARCHIVE_CONTENT", "usage")
	_, err := executeCommand(newRootCommand(), "session", "materialize", "fixture", "--cwd", t.TempDir())
	require.ErrorContains(t, err, "archive_content=usage")
}

func TestPiResumeCommandQuotesPaths(t *testing.T) {
	assert.Equal(t, "cd '/work dir' && pi --session '/data/a b.jsonl'", piResumeCommand("/work dir", "/data/a b.jsonl", false))
	assert.Equal(t, "Set-Location -LiteralPath 'C:\\work dir'; if ($?) { pi --session 'C:\\data\\a b.jsonl' }", piResumeCommand(`C:\work dir`, `C:\data\a b.jsonl`, true))
}

func TestPiMaterializeCommandReturnsNativeResume(t *testing.T) {
	dataDir := newAgentDataDir(t)
	root, dest, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", dest)
	t.Setenv("PI_DIR", root)
	const machine = "0123456789abcdef0123456789abcdef"
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "telemetry-install-id"), []byte(machine), 0o600))
	name := filepath.Join(root, "session.jsonl")
	require.NoError(t, os.WriteFile(name, []byte(`{"type":"session","id":"example","cwd":"/old"}`+"\n"), 0o600))
	database, err := db.OpenIsolatedContext(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "pi:example", Agent: "pi", Machine: machine, Project: "fixture", FilePath: &name}))
	before, err := database.GetSessionFull(t.Context(), "pi:example")
	require.NoError(t, err)
	require.NoError(t, database.Close())
	output, err := executeCommand(newRootCommand(), "session", "materialize", "pi:example", "--cwd", cwd, "--json")
	require.NoError(t, err, output)
	var result struct {
		Command string `json:"command"`
	}
	require.NoError(t, json.Unmarshal([]byte(output), &result))
	resolved, err := filepath.EvalSymlinks(cwd)
	require.NoError(t, err)
	assert.Contains(t, result.Command, resolved)
	assert.Contains(t, result.Command, filepath.Join(dest, "example.jsonl"))
	database, err = db.OpenIsolatedContext(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	after, err := database.GetSessionFull(t.Context(), "pi:example")
	require.NoError(t, err)
	assert.Equal(t, before, after)
	require.NoError(t, database.Close())
}
