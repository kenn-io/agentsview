package dbtest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// SeedNativeSearch archives handwritten native blocks and an unreparsed body.
// The same stored fixture exercises each backend's actual search reader.
func SeedNativeSearch(t *testing.T, archive *db.DB) {
	t.Helper()
	const id = "native-search"
	require.NoError(t, archive.UpsertSession(t.Context(), db.Session{
		ID: id, Project: "project", Machine: "local", Agent: "claude",
		MessageCount: 8, UserMessageCount: 2,
	}))
	body := parser.ExtractMessageContent(t.Context(), gjson.Parse(`[
		{"type":"thinking","thinking":"reasonneedle sharedneedle AKIA7QHWN2DKR4FYPLJM"},
		{"type":"text","text":"dialogueneedle sharedneedle [Thinking] literal"},
		{"type":"tool_use","id":"call","name":"Bash","input":{"command":"commandneedle"}},
		{"type":"tool_result","tool_use_id":"absent","content":"resultneedle sharedneedle"}
	]`))
	require.Equal(t, "dialogueneedle sharedneedle [Thinking] literal", body.Content)
	require.Equal(t, "reasonneedle sharedneedle AKIA7QHWN2DKR4FYPLJM", body.ThinkingText)
	require.Equal(t, "resultneedle sharedneedle", body.ToolResultText)
	require.Len(t, body.ToolCalls, 1)
	messages := []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "questionneedle"},
		{
			SessionID: id, Ordinal: 1, Role: "assistant", Content: body.Content,
			ThinkingText: body.ThinkingText, ToolResultText: body.ToolResultText,
			ContentLayout: body.ContentLayout, ContentLength: body.ContentLength,
			HasThinking: true, HasToolUse: true, ToolCalls: []db.ToolCall{{
				ToolName: "Bash", ToolUseID: "call", Category: "shell",
				InputJSON: body.ToolCalls[0].InputJSON, Rendering: body.ToolCalls[0].Rendering,
			}},
		},
		{SessionID: id, Ordinal: 2, Role: "user", Content: "systemdialogue", ThinkingText: "systemreason", IsSystem: true},
		{SessionID: id, Ordinal: 3, Role: "user", ToolResultText: "unmatchedneedle"},
		{SessionID: id, Ordinal: 4, Role: "assistant", ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{}}},
		{SessionID: id, Ordinal: 5, Role: "user", Content: "dialogueneedle legacyneedle [Thinking] reasonneedle"},
		{SessionID: id, Ordinal: 6, Role: "assistant", Content: "oldanswer"},
		{SessionID: id, Ordinal: 7, Role: "assistant", ThinkingText: "reasononlyneedle", HasThinking: true},
	}
	messages[5].SetContentLayout(nil)
	messages[6].SetContentLayout(nil)
	require.NoError(t, archive.InsertMessages(t.Context(), messages))
}

// CheckNativeSearch catches body-source swaps, legacy leakage, lost unmatched
// output, source-gate widening, and pagination before source merging.
func CheckNativeSearch(t *testing.T, store interface {
	SearchContent(context.Context, db.ContentSearchFilter) (db.ContentSearchPage, error)
}, termsSupported bool,
) {
	t.Helper()
	for _, mode := range []string{"substring", "regex", "fts"} {
		for _, needle := range []string{"dialogueneedle", "reasonneedle", "commandneedle", "resultneedle", "legacyneedle"} {
			t.Run(mode+"/messages/"+needle, func(t *testing.T) {
				page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
					SessionID: "native-search", Pattern: needle, Mode: mode, Sources: []string{"messages"},
				})
				require.NoError(t, err)
				if needle == "dialogueneedle" {
					require.Len(t, page.Matches, 1)
					assert.Equal(t, 1, page.Matches[0].Ordinal)
					assert.Contains(t, page.Matches[0].Snippet, "[Thinking] literal")
				} else {
					assert.Empty(t, page.Matches)
				}
			})
		}
	}
	for _, mode := range []string{"substring", "regex"} {
		for _, tc := range []struct {
			source, needle, location string
			ordinal                  int
		}{
			{"thinking", "reasonneedle", "thinking", 1},
			{"thinking", "reasononlyneedle", "thinking", 7},
			{"tool_input", "commandneedle", "tool_input", 1},
			{"tool_result", "resultneedle", "tool_result", 1},
			{"tool_result", "unmatchedneedle", "tool_result", 3},
		} {
			t.Run(mode+"/"+tc.source+"/"+tc.needle, func(t *testing.T) {
				filter := db.ContentSearchFilter{SessionID: "native-search", Pattern: tc.needle, Mode: mode, Sources: []string{tc.source}}
				page, err := store.SearchContent(t.Context(), filter)
				require.NoError(t, err)
				require.Len(t, page.Matches, 1)
				assert.Equal(t, tc.ordinal, page.Matches[0].Ordinal)
				assert.Equal(t, [2]int{tc.ordinal, tc.ordinal}, page.Matches[0].OrdinalRange)
				assert.Equal(t, tc.location, page.Matches[0].Location)
				assert.NotContains(t, page.Matches[0].Snippet, "AKIA7QHWN2DKR4FYPLJM")
				if tc.source == "thinking" && tc.ordinal == 1 {
					filter.RevealSecrets = true
					revealed, err := store.SearchContent(t.Context(), filter)
					require.NoError(t, err)
					require.Len(t, revealed.Matches, 1)
					assert.Contains(t, revealed.Matches[0].Snippet, "AKIA7QHWN2DKR4FYPLJM")
				}
			})
		}
		t.Run(mode+"/default-pagination", func(t *testing.T) {
			filter := db.ContentSearchFilter{SessionID: "native-search", Pattern: "sharedneedle", Mode: mode, Limit: 1}
			for i, location := range []string{"message", "thinking", "tool_result"} {
				page, err := store.SearchContent(t.Context(), filter)
				require.NoError(t, err)
				require.Len(t, page.Matches, 1)
				assert.Equal(t, location, page.Matches[0].Location)
				if i < 2 {
					assert.Equal(t, i+1, page.NextCursor)
				} else {
					assert.Zero(t, page.NextCursor)
				}
				filter.Cursor = page.NextCursor
			}
		})
		t.Run(mode+"/thinking-system-filter", func(t *testing.T) {
			filter := db.ContentSearchFilter{SessionID: "native-search", Pattern: "systemreason", Mode: mode, Sources: []string{"thinking"}}
			page, err := store.SearchContent(t.Context(), filter)
			require.NoError(t, err)
			require.Len(t, page.Matches, 1)
			filter.ExcludeSystem = true
			page, err = store.SearchContent(t.Context(), filter)
			require.NoError(t, err)
			assert.Empty(t, page.Matches)
		})
	}
	for _, mode := range []string{"fts", "terms", "semantic", "hybrid"} {
		t.Run(mode+"/thinking-source-gate", func(t *testing.T) {
			_, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
				Pattern: "reasonneedle", Mode: mode, Sources: []string{"thinking"},
			})
			require.ErrorAs(t, err, new(*db.SearchInputError))
		})
	}
	if termsSupported {
		for _, tc := range []struct {
			pattern string
			matches int
		}{
			{"questionneedle dialogueneedle", 1}, {"questionneedle reasonneedle", 0}, {"questionneedle legacyneedle", 0},
		} {
			page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{SessionID: "native-search", Pattern: tc.pattern, Mode: "terms"})
			require.NoError(t, err)
			assert.Len(t, page.Matches, tc.matches, tc.pattern)
		}
	}
	page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{SessionID: "native-search", Pattern: "^$", Mode: "regex", Sources: []string{"messages"}})
	require.NoError(t, err)
	assert.Empty(t, page.Matches, "empty native work rows are not dialogue")
}
