package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionMaterializeRejectsRemoteBackends(t *testing.T) {
	for _, flag := range []string{"--server=http://127.0.0.1:1", "--pg", "--server-token-file=", "--server-token-file=fixture"} {
		t.Run(flag, func(t *testing.T) {
			_, err := executeCommand(newRootCommand(), "session", "materialize", "fixture", "--cwd", t.TempDir(), flag)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "local archive")
		})
	}
}

func TestClaudeResumeCommandOverrides(t *testing.T) {
	posix := claudeResumeCommand("/checkout path", "id", "/config path", "project", false)
	assert.Contains(t, posix, "CLAUDE_CONFIG_DIR='/config path'")
	assert.Contains(t, posix, "CLAUDE_CODE_PROJECT_DIR_NAME=project")
	windows := claudeResumeCommand(`C:\checkout path`, "id", `C:\config path`, "project", true)
	assert.Contains(t, windows, "$env:CLAUDE_CONFIG_DIR = ")
	assert.Contains(t, windows, "$env:CLAUDE_CODE_PROJECT_DIR_NAME = ")
	assert.Contains(t, windows, "finally")
}
