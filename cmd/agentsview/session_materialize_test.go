package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	windows := claudeResumeCommand(`C:\checkout path`, "id", `C:\config path`, "project", true)
	assert.Contains(t, windows, "$env:CLAUDE_CONFIG_DIR = ")
	assert.Contains(t, windows, "$env:CLAUDE_CODE_PROJECT_DIR_NAME = ")
	assert.Contains(t, windows, "finally")
	assert.NotContains(t, claudeResumeCommand(`C:\checkout`, "id", "", "", true), "CLAUDE_CONFIG_DIR")
}

func TestSessionMaterializeUsageOnly(t *testing.T) {
	newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_ARCHIVE_CONTENT", "usage")
	_, err := executeCommand(newRootCommand(), "session", "materialize", "fixture", "--cwd", t.TempDir())
	require.ErrorContains(t, err, "archive_content=usage")
}
