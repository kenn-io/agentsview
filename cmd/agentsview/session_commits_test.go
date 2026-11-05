package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestSessionCommits(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSession(t, dataDir, "s-commit", "project-a")
	d, err := db.Open(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "s-commit", Ordinal: 0, Role: "user", Content: "Fix\x1b parser\n" + strings.Repeat("x", 200) + "END"},
		{SessionID: "s-commit", Ordinal: 1, Role: "assistant", HasToolUse: true, ToolCalls: []db.ToolCall{
			{SessionID: "s-commit", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"true"}`},
			{SessionID: "s-commit", CallIndex: 1, ToolName: "Bash", Category: "Bash", ToolUseID: "exact-call", InputJSON: `{"command":"git commit -m fix"}`, ResultContent: "[main abc1234] Fix parser", ResultEvents: []db.ToolResultEvent{{Source: "tool_result", Content: "[main abc1234] Fix parser"}}},
		}},
	}))
	require.NoError(t, d.Close())
	t.Run("json", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(), "session", "commits", "s-commit", "--format", "json")
		require.NoError(t, err)
		got := decodeCLIJSON[db.SessionCommitList](t, out)
		require.Len(t, got.Commits, 1)
		assert.Equal(t, "exact-call", got.Commits[0].ToolUseID)
		require.NotNil(t, got.Commits[0].Turn)
		assert.Equal(t, "Fix\x1b parser\n"+strings.Repeat("x", 200)+"END", got.Commits[0].Turn.Text)
	})
	t.Run("human", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(), "session", "commits", "s-commit")
		require.NoError(t, err)
		assert.Regexp(t, `SESSION\s+ORDINAL\s+CALL\s+TIME\s+SHA\s+BRANCH\s+SUBJECT\s+USER TURN`, out)
		assert.Regexp(t, `s-commit\s+1\s+1\s+abc1234\s+main\s+Fix parser`, out)
		assert.Contains(t, out, "0: Fix parser ")
		assert.Contains(t, out, "…")
		assert.NotContains(t, out, "END")
		assert.NotContains(t, out, "\x1b")
	})
}
