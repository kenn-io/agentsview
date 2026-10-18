package dbtest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// SeedDialogueSearch writes projected, legacy, and empty dialogue with a
// structured tool result. Use AssertDialogueSearchParity after pushing it.
func SeedDialogueSearch(t *testing.T, d *db.DB) {
	t.Helper()
	const id = "dialogue-search"
	SeedSession(t, d, id, "dialogue-search", func(s *db.Session) {
		s.StartedAt = new("2026-05-20T12:00:00Z")
		s.UserMessageCount = 2
		s.MessageCount = 5
	})
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), id, []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "hello"},
		{
			SessionID: id, Ordinal: 1, Role: "assistant",
			Content:      "answerneedle\n[Thinking] reasoningneedle\n[Bash] $ toolneedle",
			DialogueText: new("answerneedle"), ThinkingText: "reasoningneedle", HasThinking: true, HasToolUse: true,
			ToolCalls: []db.ToolCall{{
				ToolName: "Bash", Category: "Bash", ToolUseID: "dialogue-tool",
				InputJSON: `{"command":"toolneedle"}`, ResultContent: "resultneedle", ResultContentLength: len("resultneedle"),
			}},
		},
		{
			SessionID: id, Ordinal: 2, Role: "assistant",
			Content: "legacy answer\n[Thinking] legacyneedle", ThinkingText: "legacyneedle", HasThinking: true,
		},
		{
			SessionID: id, Ordinal: 3, Role: "assistant",
			Content: "[Thinking] emptyneedle", DialogueText: new(""), ThinkingText: "emptyneedle", HasThinking: true,
		},
		{
			SessionID: id, Ordinal: 4, Role: "assistant", IsSystem: true,
			Content: "systemneedle", DialogueText: new("systemneedle"), ThinkingText: "systemneedle", HasThinking: true,
		},
	}))
}

// AssertDialogueSearchParity compares lexical hits and snippets with SQLite.
// Callers supply their supported modes. Substring and regex cover every source;
// other modes search messages only.
func AssertDialogueSearchParity(t *testing.T, archive, mirror db.Store, modes ...string) {
	t.Helper()
	type hit struct {
		Session, Location string
		Ordinal           int
		Snippet           string
	}
	project := func(matches []db.ContentMatch) []hit {
		out := make([]hit, len(matches))
		for i, m := range matches {
			out[i] = hit{m.SessionID, m.Location, m.Ordinal, m.Snippet}
		}
		return out
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			lexical := mode == "" || mode == "substring" || mode == "regex"
			sources := []string{"messages"}
			if lexical {
				sources = []string{"", "messages", "thinking", "tool_input", "tool_result"}
			}
			for _, source := range sources {
				t.Run(source, func(t *testing.T) {
					for _, tc := range []struct {
						pattern           string
						ordinal           int
						location, snippet string
					}{
						{"answerneedle", 1, "message", "answerneedle"},
						{"reasoningneedle", 1, "thinking", "reasoningneedle"},
						{"toolneedle", 1, "tool_input", `{"command":"toolneedle"}`},
						{"resultneedle", 1, "tool_result", "resultneedle"},
						{"legacyneedle", 2, "message", "legacy answer\n[Thinking] legacyneedle"},
						{"emptyneedle", 3, "thinking", "emptyneedle"},
					} {
						t.Run(tc.pattern, func(t *testing.T) {
							f := db.ContentSearchFilter{Pattern: tc.pattern, Mode: mode, Project: "dialogue-search", Limit: 50}
							if source != "" {
								f.Sources = []string{source}
							}
							want := []hit{}
							location, snippet := tc.location, tc.snippet
							if tc.pattern == "legacyneedle" && source == "thinking" {
								location, snippet = "thinking", "legacyneedle"
							}
							if source == "" || source == location || source == "messages" && location == "message" {
								want = append(want, hit{"dialogue-search", location, tc.ordinal, snippet})
							}
							got, err := mirror.SearchContent(t.Context(), f)
							require.NoError(t, err)
							assert.Equal(t, want, project(got.Matches), "mirror contract")
						})
					}
				})
			}
			if !lexical {
				return
			}
			// A legacy message and its thinking must consume only one slot,
			// including when a cursor falls on their boundary.
			for _, excludeSystem := range []bool{false, true} {
				for cursor := range 10 {
					f := db.ContentSearchFilter{Pattern: "needle", Mode: mode, Project: "dialogue-search", Limit: 1, Cursor: cursor, ExcludeSystem: excludeSystem}
					want, err := archive.SearchContent(t.Context(), f)
					require.NoError(t, err)
					got, err := mirror.SearchContent(t.Context(), f)
					require.NoError(t, err)
					assert.Equal(t, project(want.Matches), project(got.Matches), "cursor %d, exclude system %v", cursor, excludeSystem)
					assert.Equal(t, want.NextCursor, got.NextCursor)
					if want.NextCursor == 0 {
						break
					}
				}
			}
		})
	}
}
