package sync

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestNativeBodyConversionPreservesProvenance(t *testing.T) {
	for _, staged := range []bool{false, true} {
		t.Run(map[bool]string{false: "collecting", true: "staged"}[staged], func(t *testing.T) {
			messages := []parser.ParsedMessage{{
				Role: parser.RoleAssistant, Content: "answer", ThinkingText: "plan",
				ToolResultText: "output", ContentLength: 77, HasThinking: true, HasToolUse: true,
				ContentLayout: &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{
					{Kind: "thinking", End: 4},
					{Kind: "text", End: 6},
					{Kind: "tool_call"},
					{Kind: "tool_result", End: 6},
				}},
				ToolCalls: []parser.ParsedToolCall{{ToolName: "read", Category: "file", Rendering: "[Tool: read input]"}},
			}, {Ordinal: 1, Role: parser.RoleAssistant, Content: "[Thinking] legacy body", ContentLength: 22}}
			if staged {
				sink, err := newCodexStagingSink(t.Context(), t.TempDir(), nil)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, sink.Close()) })
				for _, message := range messages {
					sink.AppendMessage(message)
				}
				messages = sink.Messages()
			}
			converted := toDBMessages(pendingWrite{
				sess: parser.ParsedSession{ID: "native-body", Agent: parser.AgentCodex}, msgs: messages,
			}, nil)
			require.Len(t, converted, 2)
			database := openTestDB(t)
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{ID: "native-body", Agent: "codex", Project: "project"}))
			require.NoError(t, database.InsertMessages(t.Context(), converted))
			stored, err := database.GetAllMessages(t.Context(), "native-body")
			require.NoError(t, err)
			require.Len(t, stored, 2)
			encoded, err := json.Marshal(stored[0])
			require.NoError(t, err)
			var actual, expected map[string]any
			require.NoError(t, json.Unmarshal(encoded, &actual))
			require.NoError(t, json.Unmarshal([]byte(`{"version":1,"blocks":[{"kind":"thinking","start":0,"end":4,"call_index":0},{"kind":"text","start":0,"end":6,"call_index":0},{"kind":"tool_call","start":0,"end":0,"call_index":0},{"kind":"tool_result","start":0,"end":6,"call_index":0}]}`), &expected))
			assert.Equal(t, expected, actual["content_layout"])
			assert.Equal(t, "output", actual["tool_result_text"])
			assert.Equal(t, "answer", stored[0].Content)
			assert.Equal(t, "plan", stored[0].ThinkingText)
			assert.Equal(t, 77, stored[0].ContentLength)
			require.Len(t, stored[0].ToolCalls, 1)
			assert.Equal(t, "[Tool: read input]", stored[0].ToolCalls[0].Rendering)
			encoded, err = json.Marshal(stored[1])
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(encoded, &actual))
			assert.Nil(t, actual["content_layout"])
			assert.Equal(t, "[Thinking] legacy body", stored[1].Content)
		})
	}
}

func TestNativeBodyParseDiffReportsSameLengthChanges(t *testing.T) {
	for _, field := range []string{"content_layout", "tool_result_text"} {
		t.Run(field, func(t *testing.T) {
			var stored, parsed db.Message
			require.NoError(t, json.Unmarshal([]byte(`{"role":"assistant","content":"answer","content_length":6,"tool_result_text":"prior","content_layout":{"version":1,"blocks":[{"kind":"text","start":0,"end":6,"call_index":0}]}}`), &stored))
			fixture := `{"role":"assistant","content":"answer","content_length":6,"tool_result_text":"after","content_layout":{"version":1,"blocks":[{"kind":"text","start":0,"end":6,"call_index":0}]}}`
			if field == "content_layout" {
				fixture = `{"role":"assistant","content":"answer","content_length":6,"tool_result_text":"prior","content_layout":{"version":1,"blocks":[{"kind":"text","start":0,"end":3,"call_index":0},{"kind":"text","start":3,"end":6,"call_index":0}]}}`
			}
			require.NoError(t, json.Unmarshal([]byte(fixture), &parsed))
			diffs := compareMessageMetadata([]db.Message{stored}, []db.Message{parsed}, true, false, false)
			require.Len(t, diffs, 1)
			assert.Equal(t, FieldMessageMetadata, diffs[0].Field)
			assert.Contains(t, diffs[0].Detail, field+" differs")
		})
	}
}
