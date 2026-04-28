package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name      string
		messages  []ParsedMessage
		truncated bool
		want      TerminationStatus
	}{
		{
			name:     "empty messages, not truncated",
			messages: nil,
			want:     "",
		},
		{
			name:      "empty messages, truncated wins",
			messages:  nil,
			truncated: true,
			want:      TerminationTruncated,
		},
		{
			name: "clean session: assistant text only, no tool calls",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "hello"},
				{Role: RoleAssistant, Content: "hi"},
			},
			want: TerminationClean,
		},
		{
			name: "clean session: tool call resolved by tool result",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "read file"},
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: "toolu_1", ToolName: "Read"},
				}},
				{Role: RoleUser, ToolResults: []ParsedToolResult{
					{ToolUseID: "toolu_1"},
				}},
				{Role: RoleAssistant, Content: "done"},
			},
			want: TerminationClean,
		},
		{
			name: "tool_call_pending: last assistant has unmatched tool_use",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "read file"},
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: "toolu_1", ToolName: "Read"},
				}},
			},
			want: TerminationToolCallPending,
		},
		{
			name: "tool_call_pending: prior turns matched, last has unmatched",
			messages: []ParsedMessage{
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: "toolu_1"},
				}},
				{Role: RoleUser, ToolResults: []ParsedToolResult{
					{ToolUseID: "toolu_1"},
				}},
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: "toolu_2"},
				}},
			},
			want: TerminationToolCallPending,
		},
		{
			name: "truncated overrides tool_call_pending",
			messages: []ParsedMessage{
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: "toolu_1"},
				}},
			},
			truncated: true,
			want:      TerminationTruncated,
		},
		{
			name: "ignores empty ToolUseID",
			messages: []ParsedMessage{
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: ""},
				}},
			},
			want: TerminationClean,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.messages, tc.truncated)
			assert.Equal(t, tc.want, got)
		})
	}
}
