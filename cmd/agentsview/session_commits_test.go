package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	gitlog "go.kenn.io/agentsview/internal/db/git"
)

func TestSessionCommits(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSession(t, dataDir, "s-commit", "project-a")
	d, err := db.Open(t.Context(), filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "s-commit", Ordinal: 0, Role: "user", Content: "Fix\x1b parser\n" + strings.Repeat("x", 200) + "END", Timestamp: "2026-10-30T10:09:00Z"},
		{SessionID: "s-commit", Ordinal: 1, Role: "assistant", HasToolUse: true, ToolCalls: []db.ToolCall{
			{SessionID: "s-commit", ToolName: "Bash", Category: "Bash", ToolUseID: "exact-call", InputJSON: `{"command":"git commit -m fix"}`, ResultContent: "[main abc1234] Fix parser", ResultEvents: []db.ToolResultEvent{{Source: "tool_result", Content: "[main abc1234] Fix parser"}}},
		}},
	}))
	_, err = d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: strings.Repeat("e", 40), Subject: "Editor commit", CommittedAt: "2026-10-30T10:10:00Z", Sessions: []string{"claude:s-commit"}}})
	require.NoError(t, err)
	require.NoError(t, d.Close())
	t.Run("json", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(), "session", "commits", "s-commit", "--format", "json")
		require.NoError(t, err)
		got := decodeCLIJSON[db.SessionCommitList](t, out)
		require.Len(t, got.Commits, 2)
		var transcript, trailer []db.CommitLink
		for _, commit := range got.Commits {
			switch commit.Source {
			case "transcript":
				transcript = append(transcript, commit)
			case "trailer":
				trailer = append(trailer, commit)
			}
		}
		require.Len(t, transcript, 1)
		assert.Equal(t, "abc1234", transcript[0].SHA)
		assert.Equal(t, "exact-call", transcript[0].ToolUseID)
		require.Len(t, trailer, 1)
		require.NotNil(t, transcript[0].Turn)
		// JSON preserves the full prompt; human output sanitizes and truncates it.
		assert.Equal(t, "Fix\x1b parser\n"+strings.Repeat("x", 200)+"END", transcript[0].Turn.Text)
	})
	t.Run("human", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(), "session", "commits", "s-commit")
		require.NoError(t, err)
		assert.Contains(t, out, "commit message")
		assert.Contains(t, out, "Editor commit")
		assert.Regexp(t, `SESSION\s+ORDINAL\s+TIME\s+SOURCE\s+SHA\s+BRANCH\s+SUBJECT\s+USER TURN`, out)
		assert.Regexp(t, `s-commit\s+1\s+transcript\s+abc1234\s+main\s+Fix parser`, out)
		assert.Contains(t, out, "0: Fix parser ")
		assert.Contains(t, out, "(estimated)")
		assert.Contains(t, out, "…")
		assert.NotContains(t, out, "END")
		assert.NotContains(t, out, "\x1b")
	})
}
