package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestNativeBodyComparisonFindsSameLengthMutation(t *testing.T) {
	for _, field := range []string{"content_layout", "tool_result_text", "rendering"} {
		t.Run(field, func(t *testing.T) {
			e, database, pending := pdWriteSingleMessageSession(t, "native-tier", parser.ParsedMessage{
				Role: parser.RoleAssistant, Content: "answer", ToolResultText: "prior", ContentLength: 77,
				ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
					{Kind: "text", End: 6}, {Kind: "tool_call"}, {Kind: "tool_result", End: 5},
				}},
				ToolCalls: []parser.ParsedToolCall{{ToolName: "read", Category: "file", Rendering: "old"}},
			})
			t.Cleanup(e.Close)
			switch field {
			case "content_layout":
				pending.msgs[0].ContentLayout = &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
					{Kind: "tool_call"}, {Kind: "text", End: 6}, {Kind: "tool_result", End: 5},
				}}
			case "tool_result_text":
				pending.msgs[0].ToolResultText = "after"
			case "rendering":
				pending.msgs[0].ToolCalls[0].Rendering = "new"
			}
			prepared, messages, verdict := e.prepareSessionWrite(pending, nil)
			require.Equal(t, sessionWriteOK, verdict)
			stored := pdFetchStored(t, database, prepared.ID)
			diffs, err := e.compareStoredSession(t.Context(), stored, prepared, messages, nil)
			require.NoError(t, err)
			require.Len(t, diffs, 1)
			assert.Contains(t, diffs[0].Detail, field+" differs")
		})
	}
}
