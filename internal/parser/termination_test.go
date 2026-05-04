package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name       string
		messages   []ParsedMessage
		stopReason string
		truncated  bool
		want       TerminationStatus
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
			name: "awaiting_user: claude end_turn",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "hello"},
				{Role: RoleAssistant, Content: "hi"},
			},
			stopReason: "end_turn",
			want:       TerminationAwaitingUser,
		},
		{
			name: "awaiting_user: codex task_complete",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "build it"},
				{Role: RoleAssistant, Content: "done"},
			},
			stopReason: "task_complete",
			want:       TerminationAwaitingUser,
		},
		{
			name: "clean: stop_reason is max_tokens (not awaiting)",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "long task"},
				{Role: RoleAssistant, Content: "response"},
			},
			stopReason: "max_tokens",
			want:       TerminationClean,
		},
		{
			name: "clean: no stop_reason recorded falls back to clean",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "hello"},
				{Role: RoleAssistant, Content: "hi"},
			},
			want: TerminationClean,
		},
		{
			name: "clean: tool call resolved by tool result",
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
			stopReason: "end_turn",
			want:       TerminationAwaitingUser,
		},
		{
			name: "tool_call_pending: last assistant has unmatched tool_use",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "read file"},
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: "toolu_1", ToolName: "Read"},
				}},
			},
			stopReason: "tool_use",
			want:       TerminationToolCallPending,
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
			name: "ignores empty ToolUseID — falls through to clean/awaiting",
			messages: []ParsedMessage{
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: ""},
				}},
			},
			stopReason: "end_turn",
			want:       TerminationAwaitingUser,
		},
		{
			// Regression: an earlier message reusing a ToolUseID
			// must NOT mark a later unresolved call as resolved.
			// hasOrphanedToolCall only counts results that appear
			// strictly AFTER the last assistant message.
			name: "earlier matching result does not resolve final orphan",
			messages: []ParsedMessage{
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: "toolu_dup"},
				}},
				{Role: RoleUser, ToolResults: []ParsedToolResult{
					{ToolUseID: "toolu_dup"},
				}},
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: "toolu_dup"},
				}},
			},
			want: TerminationToolCallPending,
		},
		{
			// Regression: once the user replies after an end_turn,
			// the agent is no longer parked. The last assistant's
			// stop_reason is still end_turn but the transcript has
			// moved on, so awaiting_user would mislead.
			name: "user reply after end_turn is not awaiting_user",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "hello"},
				{Role: RoleAssistant, Content: "hi"},
				{Role: RoleUser, Content: "follow-up"},
			},
			stopReason: "end_turn",
			want:       TerminationClean,
		},
		{
			name: "user reply after task_complete is not awaiting_user",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "build it"},
				{Role: RoleAssistant, Content: "done"},
				{Role: RoleUser, Content: "now test"},
			},
			stopReason: "task_complete",
			want:       TerminationClean,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.messages, tc.stopReason, tc.truncated)
			assert.Equal(t, tc.want, got)
		})
	}
}
