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
		s.UserMessageCount = 3
		s.MessageCount = 10
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
		{
			SessionID: id, Ordinal: 5, Role: "assistant",
			Content: `prefix_%\path tail`, DialogueText: new(`prefix_%\path tail`),
			ThinkingText: `prefix_%\path tail`, HasThinking: true, HasToolUse: true,
			ToolCalls: []db.ToolCall{
				{
					ToolName: "Bash", Category: "Bash", ToolUseID: "path-tool",
					InputJSON:     `{"command":"prefix_%\\path input"}`,
					ResultContent: `prefix_%\path result`, ResultContentLength: len(`prefix_%\path result`),
				},
				{
					ToolName: "Task", Category: "Task", ToolUseID: "path-event",
					ResultEvents: []db.ToolResultEvent{{
						ToolUseID: "path-event", Source: "test", Status: "completed",
						Content: `prefix_%\path event`, ContentLength: len(`prefix_%\path event`),
					}},
				},
			},
		},
		{SessionID: id, Ordinal: 6, Role: "user", Content: "next question"},
		{SessionID: id, Ordinal: 7, Role: "assistant", Content: "Sure.\n[Bash] $ true\nDone.", DialogueText: new("Sure.\nDone.")},
		{SessionID: id, Ordinal: 8, Role: "tool", Content: "classifiedtoolneedle", DialogueText: new("")},
		{SessionID: id, Ordinal: 9, Role: "tool", Content: "unclassifiedneedle"},
	}))
}

// AssertDialogueSearchParity checks mirror hits and snippets against expected
// values and compares lexical pagination with SQLite.
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
						{"unclassifiedneedle", 9, "message", "unclassifiedneedle"},
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
			f := db.ContentSearchFilter{Pattern: "classifiedtoolneedle", Mode: mode, Project: "dialogue-search", Sources: []string{"messages"}}
			got, err := mirror.SearchContent(t.Context(), f)
			require.NoError(t, err)
			assert.Empty(t, got.Matches)

			if !lexical {
				return
			}
			pattern := "Sure.\nDone."
			if mode == "regex" {
				pattern = `Sure\.\nDone\.`
			}
			f.Pattern = pattern
			got, err = mirror.SearchContent(t.Context(), f)
			require.NoError(t, err)
			assert.Equal(t, []hit{{"dialogue-search", "message", 7, "Sure.\nDone."}}, project(got.Matches))

			if mode == "regex" {
				for _, source := range []string{"messages", "thinking"} {
					word, upper, mixed := "answer", "ANSWER", "Answer"
					location := "message"
					if source == "thinking" {
						word, upper, mixed = "reasoning", "REASONING", "Reasoning"
						location = "thinking"
					}
					for _, tc := range []struct {
						pattern string
						ordinal int
						snippet string
					}{
						{word + `.*needle`, 1, word + "needle"},
						{`(?i)` + upper + `NEEDLE`, 1, word + "needle"},
						{mixed + `needle`, -1, ""},
						{`prefix_%\\path\s+tail`, 5, `prefix_%\path tail`},
					} {
						want := []hit{}
						if tc.ordinal >= 0 {
							want = append(want, hit{"dialogue-search", location, tc.ordinal, tc.snippet})
						}
						got, err := mirror.SearchContent(t.Context(), db.ContentSearchFilter{
							Pattern: tc.pattern, Mode: mode, Sources: []string{source}, Project: "dialogue-search",
						})
						require.NoError(t, err)
						assert.Equal(t, want, project(got.Matches), "%s: %s", source, tc.pattern)
					}
				}
				// Tool sources apply the same literal-prefix filter, including
				// both scoped branches of tool results.
				for _, tc := range []struct{ source, pattern, location, snippet string }{
					{"tool_input", `prefix_%\\\\path\s+input`, "tool_input", `{"command":"prefix_%\\path input"}`},
					{"tool_result", `prefix_%\\path\s+result`, "tool_result", `prefix_%\path result`},
					{"tool_result", `prefix_%\\path\s+event`, "tool_result", `prefix_%\path event`},
				} {
					got, err := mirror.SearchContent(t.Context(), db.ContentSearchFilter{
						Pattern: tc.pattern, Mode: mode, Sources: []string{tc.source}, Project: "dialogue-search",
					})
					require.NoError(t, err)
					assert.Equal(t, []hit{{"dialogue-search", tc.location, 5, tc.snippet}}, project(got.Matches), "%s: %s", tc.source, tc.pattern)
				}
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
