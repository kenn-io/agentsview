package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitlog "go.kenn.io/agentsview/internal/db/git"
)

func TestTrailerCommitLinksPromptAndPrecedence(t *testing.T) {
	for _, sha := range []string{"abc1234", "abc1234" + strings.Repeat("0", 33)} {
		t.Run(sha, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, "session-a", "project-a")
			insertSession(t, d, "codex:thread", "project-a")
			insertSession(t, d, "thread", "project-a")
			messages := []Message{
				{SessionID: "session-a", Ordinal: 0, Role: "user", Content: "Before", Timestamp: "2026-10-30T10:00:00Z"},
				{SessionID: "session-a", Ordinal: 1, Role: "user", Content: "Within tolerance", Timestamp: "2026-10-30T10:14:00Z"},
				{SessionID: "session-a", Ordinal: 2, Role: "user", Content: "Too late", Timestamp: "2026-10-30T10:16:00Z"},
				{SessionID: "session-a", Ordinal: 3, Role: "user", Content: "System", IsSystem: true, Timestamp: "2026-10-30T10:14:00Z"},
				{SessionID: "session-a", Ordinal: 4, Role: "user", Content: "Tool result", SourceSubtype: "tool_result", Timestamp: "2026-10-30T10:14:00Z"},
			}
			insertMessages(t, d, messages...)
			c := gitlog.TrailerCommit{SHA: "abc1234" + strings.Repeat("0", 33), Branch: "main", Subject: "Editor", CommittedAt: "2026-10-30T10:10:00Z", Sessions: []string{"claude:session-a", "codex:thread", "unknown:thread"}}
			exact := commitMessages("session-a", `{"command":"git commit -m fix"}`, "[main "+sha+"] Exact")[2]
			exact.Ordinal = 5
			insertMessages(t, d, exact)
			require.NoError(t, d.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{c}))
			found, err := d.CommitSessionLinks(t.Context(), []string{"abc1234"})
			require.NoError(t, err)
			require.Len(t, found.Commits[0].Links, 2)
			for _, l := range found.Commits[0].Links {
				assert.False(t, l.Ambiguous)
				if l.SessionID == "session-a" {
					assert.Equal(t, "transcript", l.Source)
					assert.Equal(t, sha, l.SHA)
				} else {
					assert.Equal(t, "codex:thread", l.SessionID)
					assert.Equal(t, "trailer", l.Source)
					assert.Nil(t, l.Turn)
				}
			}
			// A full transcript replacement removes exact evidence, preserving trailers.
			require.NoError(t, d.ReplaceSessionMessages(t.Context(), "session-a", messages))
			list, err := d.SessionCommitLinks(t.Context(), "session-a")
			require.NoError(t, err)
			require.Len(t, list.Commits, 1)
			l := list.Commits[0]
			assert.Equal(t, "trailer", l.Source)
			assert.Equal(t, -1, l.CallIndex)
			assert.Empty(t, l.ToolUseID)
			require.NotNil(t, l.Turn)
			assert.True(t, l.Turn.Heuristic)
			assert.Equal(t, "Within tolerance", l.Turn.Text)
		})
	}
}
