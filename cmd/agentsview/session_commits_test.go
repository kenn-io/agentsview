package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestSessionCommitsJSON(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSession(t, dataDir, "s-commit", "project-a")
	d, err := db.Open(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "s-commit", Ordinal: 0, Role: "user", Content: "Fix parser"},
		{SessionID: "s-commit", Ordinal: 1, Role: "assistant", ToolCalls: []db.ToolCall{{SessionID: "s-commit", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"git commit -m fix"}`, ResultContent: "[main abc1234] Fix parser", ResultEvents: []db.ToolResultEvent{{Source: "tool_result", Content: "[main abc1234] Fix parser"}}}}},
	}))
	require.NoError(t, d.Close())
	out, err := executeCommand(newRootCommand(), "session", "commits", "s-commit", "--format", "json")
	require.NoError(t, err)
	got := decodeCLIJSON[db.SessionCommitList](t, out)
	require.Len(t, got.Commits, 1)
	assert.Equal(t, "abc1234", got.Commits[0].SHA)
	assert.Equal(t, "s-commit", got.Commits[0].SessionID)
	require.NotNil(t, got.Commits[0].Turn)
	assert.Equal(t, "Fix parser", got.Commits[0].Turn.Text)

	d, err = db.Open(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, d.DeleteSession(t.Context(), "s-commit"))
	require.NoError(t, d.Close())
	out, err = executeCommand(newRootCommand(), "session", "commits", "s-commit", "--format", "json")
	require.NoError(t, err)
	got = decodeCLIJSON[db.SessionCommitList](t, out)
	require.Len(t, got.Commits, 1)
	assert.True(t, got.Commits[0].Missing)
	assert.Nil(t, got.Commits[0].Turn)
}
