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
		{SessionID: "s-commit", Ordinal: 0, Role: "user", Content: "Tidy docs", Timestamp: "2026-10-30T10:00:00Z"},
		{SessionID: "s-commit", Ordinal: 1, Role: "user", Content: "Fix\x1b parser\n" + strings.Repeat("x", 200) + "END", Timestamp: "2026-10-30T11:00:00Z"},
		{SessionID: "s-commit", Ordinal: 2, Role: "assistant", HasToolUse: true, ToolCalls: []db.ToolCall{
			{SessionID: "s-commit", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"true"}`},
			{SessionID: "s-commit", CallIndex: 1, ToolName: "Bash", Category: "Bash", ToolUseID: "exact-call", InputJSON: `{"command":"git commit -m fix"}`, ResultContent: "[main abc1234] Fix parser", ResultEvents: []db.ToolResultEvent{{Source: "tool_result", Content: "[main abc1234] Fix parser"}}},
		}},
	}))
	require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: strings.Repeat("e", 40), Subject: "Editor commit", CommittedAt: "2026-10-30T10:10:00Z", Sessions: []string{"claude:s-commit"}}}))
	require.NoError(t, d.Close())
	t.Run("json", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(), "session", "commits", "s-commit", "--format", "json")
		require.NoError(t, err)
		got := decodeCLIJSON[db.SessionCommitList](t, out)
		require.Len(t, got.Commits, 2)
		assert.ElementsMatch(t, []string{"transcript", "trailer"}, []string{got.Commits[0].Source, got.Commits[1].Source})
		var exact *db.CommitLink
		for i := range got.Commits {
			if got.Commits[i].Source == "transcript" {
				exact = &got.Commits[i]
			}
		}
		require.NotNil(t, exact)
		assert.Equal(t, "abc1234", exact.SHA)
		assert.Equal(t, "s-commit", exact.SessionID)
		// JSON preserves the full prompt; human output sanitizes and truncates it.
		assert.Equal(t, &db.CommitTurn{Ordinal: 1, Text: "Fix\x1b parser\n" + strings.Repeat("x", 200) + "END"}, exact.Turn)
	})
	t.Run("human", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(), "session", "commits", "s-commit")
		require.NoError(t, err)
		assert.Contains(t, out, "commit message")
		assert.Contains(t, out, "Editor commit")
		assert.Contains(t, out, "SOURCE")
		assert.Contains(t, out, "transcript")
		assert.Regexp(t, `s-commit\s+2\s+1\s+exact-call`, out)
		assert.Contains(t, out, "abc1234")
		assert.Contains(t, out, "Fix parser")
		assert.Contains(t, out, "1: Fix parser ")
		assert.Contains(t, out, "(heuristic)")
		assert.Contains(t, out, "…")
		assert.NotContains(t, out, "END")
		assert.NotContains(t, out, "\x1b")
	})
}
