package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePoolsideSession(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_019e658b-56c3-7cb2-a9d1-8af2ea649438.ndjson")

	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.089334-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/Users/test/project"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.092132-04:00","type":"session.input","session_input":{"id":"","prompt":"Hello, can you help me?","estimated_prompt_token_usage":10,"mode":"build"}}
{"id":"event-3","timestamp":"2026-07-08T07:20:55.564843-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"event-4","timestamp":"2026-07-08T07:20:56.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"I can help you with that."}}
{"id":"event-5","step_id":"step-event-5","timestamp":"2026-07-08T07:21:00.000000-04:00","type":"tool_call.inference.start","tool_call_inference_start":{"chat_completion_request":{"model":"poolside/laguna-m.1"}}}
{"id":"event-6","step_id":"step-event-5","timestamp":"2026-07-08T07:21:00.000000-04:00","type":"tool_call.inference.end","tool_call_inference_end":{"input_tokens":1000,"output_tokens":50,"cache_read_input_tokens":500,"cache_write_input_tokens":0}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	sess, msgs, usageEvents, err := parsePoolsideSession(trajectoryPath, "", "test-machine")
	require.NoError(t, err)

	assert.Contains(t, sess.ID, "poolside:")
	assert.Equal(t, AgentPoolside, sess.Agent)
	assert.Equal(t, 2, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "/Users/test/project", sess.Cwd)
	assert.Equal(t, "test-machine", sess.Machine)
	assert.Equal(t, "poolside-trajectory-v1", sess.SourceVersion)
	assert.Equal(t, "I can help you with that.", msgs[1].Content)

	// Usage events.
	require.Len(t, usageEvents, 1)
	assert.Equal(t, "poolside/laguna-m.1", usageEvents[0].Model)
	assert.Equal(t, 1000, usageEvents[0].InputTokens)
	assert.Equal(t, 50, usageEvents[0].OutputTokens)
	assert.Equal(t, 500, usageEvents[0].CacheReadInputTokens)

	// Session-level token aggregates.
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 50, sess.TotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 1500, sess.PeakContextTokens)
}

func TestParsePoolsideSessionWithToolCalls(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_test123.ndjson")

	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.089334-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.092132-04:00","type":"session.input","session_input":{"id":"","prompt":"Read the file","mode":"build"}}
{"id":"event-3","timestamp":"2026-07-08T07:20:55.000000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"parsed-1","step_id":"step-1","timestamp":"2026-07-08T07:20:56.000000-04:00","type":"tool_call.parsed","tool_call_parsed":{"id":"chatcmpl-tool-1","name":"read","args":{"path":"/test/file.txt"}}}
{"id":"result-1","step_id":"step-1","timestamp":"2026-07-08T07:20:57.000000-04:00","type":"tool_call.result","tool_call_result":{"id":"chatcmpl-tool-1","tool_name":"read","observation":"file contents here"}}
{"id":"event-6","timestamp":"2026-07-08T07:21:00.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"Here is the file content."}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	sess, msgs, _, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)

	assert.Equal(t, 2, sess.MessageCount)
	assert.True(t, msgs[1].HasToolUse)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "read", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "Read", msgs[1].ToolCalls[0].Category)
	assert.Contains(t, msgs[1].ToolCalls[0].ToolUseID, "poolside:read:")
}

// TestParsePoolsideMultipleCallsSameStep verifies that multiple
// tool_call.parsed events sharing the same step_id each get their
// own result paired correctly, not overwritten by a later call.
func TestParsePoolsideMultipleCallsSameStep(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_multicall.ndjson")

	// Two tool calls in the same step, each with a unique payload ID
	// and its own result. The old step_id-keyed map would overwrite
	// the first parsed call, losing its result.
	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.000000-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.100000-04:00","type":"session.input","session_input":{"id":"","prompt":"read two files","mode":"build"}}
{"id":"event-3","timestamp":"2026-07-08T07:20:52.000000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"parsed-1","step_id":"step-multi","timestamp":"2026-07-08T07:20:53.000000-04:00","type":"tool_call.parsed","tool_call_parsed":{"id":"call-a","name":"read","args":{"path":"/test/a.txt"}}}
{"id":"parsed-2","step_id":"step-multi","timestamp":"2026-07-08T07:20:53.100000-04:00","type":"tool_call.parsed","tool_call_parsed":{"id":"call-b","name":"read","args":{"path":"/test/b.txt"}}}
{"id":"result-1","step_id":"step-multi","timestamp":"2026-07-08T07:20:54.000000-04:00","type":"tool_call.result","tool_call_result":{"id":"call-a","tool_name":"read","observation":"contents of A"}}
{"id":"result-2","step_id":"step-multi","timestamp":"2026-07-08T07:20:54.100000-04:00","type":"tool_call.result","tool_call_result":{"id":"call-b","tool_name":"read","observation":"contents of B"}}
{"id":"event-6","timestamp":"2026-07-08T07:21:00.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"Here are both files."}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	_, msgs, _, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)

	require.Len(t, msgs, 2)
	assistant := msgs[1]
	assert.True(t, assistant.HasToolUse)
	require.Len(t, assistant.ToolCalls, 2, "both tool calls must be present")

	// Each call must have its own result paired correctly.
	require.Len(t, assistant.ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "contents of A", assistant.ToolCalls[0].ResultEvents[0].Content,
		"first call must get its own result")
	require.Len(t, assistant.ToolCalls[1].ResultEvents, 1)
	assert.Equal(t, "contents of B", assistant.ToolCalls[1].ResultEvents[0].Content,
		"second call must get its own result, not overwrite the first")
}

// TestParsePoolsideMultipleShellCallsSameStep verifies that multiple
// shell tool calls in the same step each get their shell_id correctly
// enriched, not mixed up by a step_id-keyed map.
func TestParsePoolsideMultipleShellCallsSameStep(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_multishell.ndjson")

	// Two shell calls in the same step with different shell IDs.
	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.000000-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.100000-04:00","type":"session.input","session_input":{"id":"","prompt":"run two commands","mode":"build"}}
{"id":"event-3","timestamp":"2026-07-08T07:20:52.000000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"parsed-a","step_id":"step-shell","timestamp":"2026-07-08T07:20:53.000000-04:00","type":"tool_call.parsed","tool_call_parsed":{"id":"call-a","name":"shell","args":{"cmd":"npm test"}}}
{"id":"parsed-b","step_id":"step-shell","timestamp":"2026-07-08T07:20:53.100000-04:00","type":"tool_call.parsed","tool_call_parsed":{"id":"call-b","name":"shell","args":{"cmd":"npm run lint"}}}
{"id":"result-a","step_id":"step-shell","timestamp":"2026-07-08T07:20:54.000000-04:00","type":"tool_call.result","tool_call_result":{"id":"call-a","tool_name":"shell","observation":"tests passed","shell_run_tool_result":{"shell_id":"shell-test"}}}
{"id":"result-b","step_id":"step-shell","timestamp":"2026-07-08T07:20:54.100000-04:00","type":"tool_call.result","tool_call_result":{"id":"call-b","tool_name":"shell","observation":"lint passed","shell_run_tool_result":{"shell_id":"shell-lint"}}}
{"id":"parsed-status-a","step_id":"step-status-a","timestamp":"2026-07-08T07:21:00.000000-04:00","type":"tool_call.parsed","tool_call_parsed":{"id":"status-a","name":"shell_status","args":{"shell_id":"shell-test"}}}
{"id":"parsed-status-b","step_id":"step-status-b","timestamp":"2026-07-08T07:21:00.100000-04:00","type":"tool_call.parsed","tool_call_parsed":{"id":"status-b","name":"shell_status","args":{"shell_id":"shell-lint"}}}
{"id":"event-end","timestamp":"2026-07-08T07:22:00.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"Both commands finished."}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	_, msgs, _, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)

	// Find shell_status calls and verify enrichment.
	var statusA, statusB *ParsedToolCall
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			tc := &msgs[i].ToolCalls[j]
			if tc.ToolName == "shell_status" {
				if statusA == nil {
					statusA = tc
				} else {
					statusB = tc
				}
			}
		}
	}

	require.NotNil(t, statusA, "first shell_status call not found")
	require.NotNil(t, statusB, "second shell_status call not found")

	// Each shell_status must be enriched with the correct original
	// command, not mixed up by a step_id-keyed map.
	assert.Contains(t, statusA.InputJSON, "npm test",
		"first shell_status must be enriched with its shell's command")
	assert.Contains(t, statusA.InputJSON, "shell-test")
	assert.Contains(t, statusB.InputJSON, "npm run lint",
		"second shell_status must be enriched with its shell's command")
	assert.Contains(t, statusB.InputJSON, "shell-lint")
}

func TestParsePoolsideSessionWithThinking(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_thinking.ndjson")

	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.089334-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.092132-04:00","type":"session.input","session_input":{"id":"","prompt":"Think about this","mode":"build"}}
{"id":"event-3","timestamp":"2026-07-08T07:20:55.000000-04:00","type":"thought.start","thought_start":{}}
{"id":"event-4","timestamp":"2026-07-08T07:20:56.000000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"event-5","timestamp":"2026-07-08T07:20:57.000000-04:00","type":"thought.end","thought_end":{"thought":"Let me think about this carefully..."}}
{"id":"event-6","timestamp":"2026-07-08T07:21:00.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"After thinking, here is my answer."}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	sess, msgs, _, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)

	assert.Equal(t, 2, sess.MessageCount)
	// The assistant message should have thinking text.
	assistantMsg := msgs[1]
	assert.Equal(t, RoleAssistant, assistantMsg.Role)
	assert.True(t, assistantMsg.HasThinking)
	assert.Contains(t, assistantMsg.ThinkingText, "Let me think about this carefully")
}

func TestParsePoolsideSessionEmpty(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_empty.ndjson")

	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.089334-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	sess, msgs, _, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)

	assert.Contains(t, sess.ID, "poolside:")
	assert.Equal(t, 0, sess.MessageCount)
	assert.Empty(t, msgs)
}

func TestParsePoolsideSessionTermination(t *testing.T) {
	tests := []struct {
		name     string
		reason   string
		expected TerminationStatus
	}{
		{"clean exit", "exit_tool_called", TerminationClean},
		{"empty reason", "", TerminationClean},
		{"memory error", "memory_compression_error", TerminationTruncated},
		{"user cancel", "user_cancelled", TerminationClean},
		{"cancelled", "cancelled", TerminationClean},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			termination := classifyPoolsideTermination(tt.reason, nil, false)
			assert.Equal(t, tt.expected, termination)
		})
	}
}

// TestClassifyPoolsideExitToolNotOrphaned verifies that a trailing
// exit tool call with reason "exit_tool_called" is classified as
// clean even though the exit tool has no result event (the session
// terminates when exit is invoked).
func TestClassifyPoolsideExitToolNotOrphaned(t *testing.T) {
	messages := []ParsedMessage{
		{
			Ordinal: 1,
			Role:    RoleUser,
			Content: "help",
		},
		{
			Ordinal:    2,
			Role:       RoleAssistant,
			HasToolUse: true,
			ToolCalls: []ParsedToolCall{
				{
					ToolUseID: "poolside:exit:1",
					ToolName:  "exit",
					Category:  "Tool",
				},
			},
		},
	}

	// exit_tool_called must be clean, not tool_call_pending, even
	// though the exit tool has no result event.
	termination := classifyPoolsideTermination("exit_tool_called", messages, false)
	assert.Equal(t, TerminationClean, termination,
		"a trailing exit tool must not be classified as orphaned")
}

// TestClassifyPoolsideNonExitToolOrphaned verifies that a non-exit
// tool call without a result event is still classified as
// tool_call_pending.
func TestClassifyPoolsideNonExitToolOrphaned(t *testing.T) {
	messages := []ParsedMessage{
		{
			Ordinal: 1,
			Role:    RoleUser,
			Content: "read file",
		},
		{
			Ordinal:    2,
			Role:       RoleAssistant,
			HasToolUse: true,
			ToolCalls: []ParsedToolCall{
				{
					ToolUseID: "poolside:read:1",
					ToolName:  "read",
					Category:  "Read",
				},
			},
		},
	}

	termination := classifyPoolsideTermination("", messages, false)
	assert.Equal(t, TerminationToolCallPending, termination,
		"a non-exit tool without a result must be orphaned")
}

// TestClassifyPoolsideTruncationPrecedence verifies that truncation
// (malformed final record) takes precedence over exit_tool_called.
func TestClassifyPoolsideTruncationPrecedence(t *testing.T) {
	termination := classifyPoolsideTermination("exit_tool_called", nil, true)
	assert.Equal(t, TerminationTruncated, termination,
		"truncation must override exit_tool_called")
}

// TestParsePoolsideMultipleThoughts verifies that multiple thought.end
// events in one assistant turn are concatenated, not overwritten.
func TestParsePoolsideMultipleThoughts(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_multthought.ndjson")

	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.000000-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.100000-04:00","type":"session.input","session_input":{"id":"","prompt":"think deeply","mode":"build"}}
{"id":"event-3","timestamp":"2026-07-08T07:20:52.000000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"thought-1","timestamp":"2026-07-08T07:20:53.000000-04:00","type":"thought.end","thought_end":{"thought":"First reasoning block."}}
{"id":"thought-2","timestamp":"2026-07-08T07:20:54.000000-04:00","type":"thought.end","thought_end":{"thought":"Second reasoning block."}}
{"id":"event-4","timestamp":"2026-07-08T07:21:00.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"Here is my answer."}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	_, msgs, _, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)

	require.Len(t, msgs, 2)
	assistant := msgs[1]
	assert.Equal(t, RoleAssistant, assistant.Role)
	assert.True(t, assistant.HasThinking)
	assert.Equal(t, "First reasoning block.\nSecond reasoning block.",
		assistant.ThinkingText,
		"multiple thought.end events must be concatenated with newline")
}

// TestParsePoolsideMalformedFinalLineSetsTruncated verifies that a
// trajectory with a malformed final line sets IsTruncated.
func TestParsePoolsideMalformedFinalLineSetsTruncated(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_truncated.ndjson")

	// Valid events followed by a malformed final line (simulates a
	// trajectory cut off mid-write).
	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.000000-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.100000-04:00","type":"session.input","session_input":{"id":"","prompt":"hello","mode":"build"}}
{"id":"event-3","timestamp":"2026-07-08T07:20:52.000000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"event-4","timestamp":"2026-07-08T07:21:00.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"response"}}
{"id":"malformed`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	sess, _, _, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)

	assert.True(t, sess.IsTruncated,
		"a malformed final line must set IsTruncated")
	assert.Equal(t, TerminationTruncated, sess.TerminationStatus,
		"truncation must take precedence in termination classification")
	assert.Equal(t, 1, sess.MalformedLines)
}

func TestPoolsideToolCategory(t *testing.T) {
	tests := []struct {
		toolName string
		expected string
	}{
		{"read", "Read"},
		{"list_directory", "Read"},
		{"edit", "Edit"},
		{"write", "Write"},
		{"grep", "Grep"},
		{"glob", "Glob"},
		{"shell", "Bash"},
		{"todo_action", "Tool"},
		{"skill", "Tool"},
		{"switch_mode", "Tool"},
		{"question", "Tool"},
		{"exit", "Tool"},
		{"shell_kill", "Bash"},
		{"shell_status", "Bash"},
		{"shell_tail", "Bash"},
	}

	for _, tt := range tests {
		t.Run(tt.toolName, func(t *testing.T) {
			category := NormalizeToolCategory(tt.toolName)
			assert.Equal(t, tt.expected, category)
		})
	}
}

func TestPoolsideSessionIDFromFilename(t *testing.T) {
	tests := []struct {
		filename string
		expected string
	}{
		{
			"trajectory-standalone_019e658b-56c3-7cb2-a9d1-8af2ea649438.ndjson",
			"poolside:standalone_019e658b-56c3-7cb2-a9d1-8af2ea649438",
		},
		{
			"trajectory-session_abc123.ndjson",
			"poolside:session_abc123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			sessionID := poolsideIDPrefix +
				tt.filename[len("trajectory-"):len(tt.filename)-len(".ndjson")]
			assert.Equal(t, tt.expected, sessionID)
		})
	}
}

func TestPoolsideTerminationWithEmbeddedResults(t *testing.T) {
	// Test that tool calls with embedded ResultEvents are considered resolved.
	messages := []ParsedMessage{
		{
			Ordinal: 1,
			Role:    RoleUser,
			Content: "test",
		},
		{
			Ordinal:    2,
			Role:       RoleAssistant,
			Content:    "I'll read the file",
			HasToolUse: true,
			ToolCalls: []ParsedToolCall{
				{
					ToolUseID: "poolside:read:1",
					ToolName:  "read",
					Category:  "Read",
					ResultEvents: []ParsedToolResultEvent{
						{Status: "completed", Content: "file contents"},
					},
				},
			},
		},
	}

	// Should be clean because the tool call has an embedded ResultEvent.
	assert.False(t, hasOrphanedToolCall(messages))
}

func TestParsePoolsideShellEnrichment(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_shelltest.ndjson")

	// Simulate a shell command followed by shell_status
	// In real poolside data, tool_call.parsed and tool_call.result have
	// different event IDs but share the same step_id.
	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.000000-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.100000-04:00","type":"session.input","session_input":{"id":"","prompt":"run test","mode":"build"}}
{"id":"event-3","timestamp":"2026-07-08T07:20:52.000000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"parsed-shell","step_id":"step-shell","timestamp":"2026-07-08T07:20:53.000000-04:00","type":"tool_call.parsed","tool_call_parsed":{"id":"call-1","name":"shell","args":{"cmd":"npm test"}}}
{"id":"result-shell","step_id":"step-shell","timestamp":"2026-07-08T07:20:54.000000-04:00","type":"tool_call.result","tool_call_result":{"id":"call-1","tool_name":"shell","observation":"Running tests...","shell_run_tool_result":{"shell_id":"shell-npm-test"}}}
{"id":"parsed-status","step_id":"step-status","timestamp":"2026-07-08T07:21:00.000000-04:00","type":"tool_call.parsed","tool_call_parsed":{"id":"call-2","name":"shell_status","args":{"shell_id":"shell-npm-test"}}}
{"id":"result-status","step_id":"step-status","timestamp":"2026-07-08T07:21:01.000000-04:00","type":"tool_call.result","tool_call_result":{"id":"call-2","tool_name":"shell_status","observation":"running"}}
{"id":"event-8","timestamp":"2026-07-08T07:22:00.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"Tests passed."}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	_, msgs, _, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)

	// Find the shell_status tool call
	var shellStatusTC *ParsedToolCall
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			if msgs[i].ToolCalls[j].ToolName == "shell_status" {
				shellStatusTC = &msgs[i].ToolCalls[j]
				break
			}
		}
	}

	require.NotNil(t, shellStatusTC, "shell_status tool call not found")
	assert.Contains(t, shellStatusTC.InputJSON, "npm test", "shell_status should have enriched cmd")
	assert.Contains(t, shellStatusTC.InputJSON, "shell-npm-test", "shell_status should have shell_id")
}

func TestParsePoolsideDeniedToolCall(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_denied.ndjson")

	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.089334-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.092132-04:00","type":"session.input","session_input":{"id":"","prompt":"Delete a file","mode":"build"}}
{"id":"event-3","timestamp":"2026-07-08T07:20:55.000000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"parsed-denied","step_id":"step-denied","timestamp":"2026-07-08T07:20:56.000000-04:00","type":"tool_call.parsed","tool_call_parsed":{"id":"chatcmpl-tool-1","name":"shell","args":{"cmd":"rm -rf /"}}}
{"id":"result-denied","step_id":"step-denied","timestamp":"2026-07-08T07:20:57.000000-04:00","type":"tool_call.result","tool_call_result":{"id":"chatcmpl-tool-1","tool_name":"shell","execution_latency":0,"observation":"user denied tool","is_error":true,"execution_error_kind":"approval_denied"}}
{"id":"event-6","timestamp":"2026-07-08T07:21:00.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"I won't do that."}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	_, msgs, _, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)

	assert.Equal(t, 2, len(msgs))
	assert.True(t, msgs[1].HasToolUse)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "shell", msgs[1].ToolCalls[0].ToolName)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "denied", msgs[1].ToolCalls[0].ResultEvents[0].Status)
	assert.Equal(t, "user denied tool", msgs[1].ToolCalls[0].ResultEvents[0].Content)
}

func TestParsePoolsideSkillToolName(t *testing.T) {
	tests := []struct {
		name         string
		args         string
		expectedName string
	}{
		{
			name:         "skill with skill field",
			args:         `{"skill":"improve"}`,
			expectedName: "improve",
		},
		{
			name:         "skill with name field",
			args:         `{"name":"testing-without-tautologies"}`,
			expectedName: "testing-without-tautologies",
		},
		{
			name:         "skill with no name field",
			args:         `{}`,
			expectedName: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_skill.ndjson")

			content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.000000-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.100000-04:00","type":"session.input","session_input":{"id":"","prompt":"run skill","mode":"build"}}
{"id":"event-3","timestamp":"2026-07-08T07:20:52.000000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"parsed-skill","step_id":"step-skill","timestamp":"2026-07-08T07:20:53.000000-04:00","type":"tool_call.parsed","tool_call_parsed":{"id":"call-1","name":"skill","args":` + tt.args + `}}
{"id":"result-skill","step_id":"step-skill","timestamp":"2026-07-08T07:20:54.000000-04:00","type":"tool_call.result","tool_call_result":{"id":"call-1","tool_name":"skill","observation":"skill output"}}
{"id":"event-6","timestamp":"2026-07-08T07:22:00.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"Skill completed."}}
`
			require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

			_, msgs, _, err := parsePoolsideSession(trajectoryPath, "", "")
			require.NoError(t, err)

			require.Len(t, msgs[1].ToolCalls, 1)
			assert.Equal(t, "skill", msgs[1].ToolCalls[0].ToolName)
			assert.Equal(t, tt.expectedName, msgs[1].ToolCalls[0].SkillName)
		})
	}
}

func TestParsePoolsideSkillInferenceFromReadTool(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_skillinfer.ndjson")

	// Skill inference from read tools that reference SKILL.md
	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.000000-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.100000-04:00","type":"session.input","session_input":{"id":"","prompt":"read skill","mode":"build"}}
{"id":"event-3","timestamp":"2026-07-08T07:20:52.000000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"parsed-skillread","step_id":"step-skillread","timestamp":"2026-07-08T07:20:53.000000-04:00","type":"tool_call.parsed","tool_call_parsed":{"id":"call-1","name":"read","args":{"path":"/test/.agents/skills/foo/SKILL.md"}}}
{"id":"result-skillread","step_id":"step-skillread","timestamp":"2026-07-08T07:20:54.000000-04:00","type":"tool_call.result","tool_call_result":{"id":"call-1","tool_name":"read","observation":"skill content"}}
{"id":"event-6","timestamp":"2026-07-08T07:22:00.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"Read skill."}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	sess, msgs, _, err := parsePoolsideSession(trajectoryPath, "/test", "")
	require.NoError(t, err)

	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "read", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, "foo", msgs[1].ToolCalls[0].SkillName, "should infer skill name from SKILL.md path")
	_ = sess // avoid unused variable error
}

// TestParsePoolsideSessionModelSwitch verifies that mid-session model
// switches produce one usage event per inference, with each event
// correctly attributed to the model that served it via the
// tool_call.inference.start / end step_id pairing real Poolside
// trajectories use.
func TestParsePoolsideSessionModelSwitch(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_modelswitch.ndjson")

	// Two assistant turns. First turn uses poolside/laguna-m.1.
	// Second turn switches to poolside/laguna-s-2.1. Each turn's
	// events share step_id so the parser resolves per-message
	// attribution via pendingInferences[currentMsgStepID].
	// The ordering mirrors real poolside: tool_call.inference.start
	// fires BEFORE assistant_message.start; assistant_message.end
	// fires BEFORE tool_call.inference.end; the assistant_message
	// events share the cluster's step_id (not a separate one).
	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.000000-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.100000-04:00","type":"session.input","session_input":{"id":"","prompt":"help","mode":"build"}}
{"id":"inf1-start","step_id":"step-m1","timestamp":"2026-07-08T07:20:52.000000-04:00","type":"tool_call.inference.start","tool_call_inference_start":{"chat_completion_request":{"model":"poolside/laguna-m.1"}}}
{"id":"turn1-start","step_id":"step-m1","timestamp":"2026-07-08T07:20:52.500000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"turn1-end","step_id":"step-m1","timestamp":"2026-07-08T07:20:53.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"first turn response"}}
{"id":"inf1-end","step_id":"step-m1","timestamp":"2026-07-08T07:20:54.000000-04:00","type":"tool_call.inference.end","tool_call_inference_end":{"input_tokens":800,"output_tokens":40,"cache_read_input_tokens":300,"cache_write_input_tokens":0}}
{"id":"event-u1","timestamp":"2026-07-08T07:20:56.000000-04:00","type":"session.input","session_input":{"id":"","prompt":"continue","mode":"build"}}
{"id":"inf2-start","step_id":"step-s21","timestamp":"2026-07-08T07:20:57.000000-04:00","type":"tool_call.inference.start","tool_call_inference_start":{"chat_completion_request":{"model":"poolside/laguna-s-2.1"}}}
{"id":"turn2-start","step_id":"step-s21","timestamp":"2026-07-08T07:20:57.500000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"turn2-end","step_id":"step-s21","timestamp":"2026-07-08T07:20:58.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"second turn response"}}
{"id":"inf2-end","step_id":"step-s21","timestamp":"2026-07-08T07:20:59.000000-04:00","type":"tool_call.inference.end","tool_call_inference_end":{"input_tokens":2000,"output_tokens":100,"cache_read_input_tokens":0,"cache_write_input_tokens":150}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	sess, msgs, usageEvents, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)

	// Two inferences -> two usage events, one per model.
	require.Len(t, usageEvents, 2, "each inference must emit its own usage event")

	// Tokens per event match the inference.end payloads; models are
	// attributed via step_id pairing, not from currentModel order.
	assert.Equal(t, "poolside/laguna-m.1", usageEvents[0].Model)
	assert.Equal(t, 800, usageEvents[0].InputTokens)
	assert.Equal(t, 40, usageEvents[0].OutputTokens)
	assert.Equal(t, 300, usageEvents[0].CacheReadInputTokens)

	assert.Equal(t, "poolside/laguna-s-2.1", usageEvents[1].Model)
	assert.Equal(t, 2000, usageEvents[1].InputTokens)
	assert.Equal(t, 100, usageEvents[1].OutputTokens)
	assert.Equal(t, 0, usageEvents[1].CacheReadInputTokens)
	assert.Equal(t, 150, usageEvents[1].CacheCreationInputTokens)

	// Dedup keys encode step_id so re-parsing the same trajectory
	// does not duplicate events in storage.
	assert.Contains(t, usageEvents[0].DedupKey, "step-m1")
	assert.Contains(t, usageEvents[1].DedupKey, "step-s21")
	for i, ev := range usageEvents {
		assert.Equal(t, "inference", ev.Source,
			"event %d must be Source=inference", i)
	}

	// Session-level aggregates roll up across both models.
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 140, sess.TotalOutputTokens,
		"TotalOutputTokens sums output across inferences, not per-model")
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 2150, sess.PeakContextTokens,
		"PeakContextTokens is the max of total context (input + cache read + cache write) across inferences")

	// Assistant messages carry the model that produced the turn.
	// Turn order in msgs: [user@1, asst@2, user@3, asst@4]. The
	// second assistant is msgs[3], not msgs[2] (which is the
	// second user message).
	require.GreaterOrEqual(t, len(msgs), 4) // user + asst + user + asst
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, RoleUser, msgs[2].Role)
	assert.Equal(t, RoleAssistant, msgs[3].Role)
	assert.Equal(t, "poolside/laguna-m.1", msgs[1].Model,
		"first assistant message tags the model from its inference")
	assert.Equal(t, "poolside/laguna-s-2.1", msgs[3].Model,
		"second assistant message tags the switched model")
}

// TestParsePoolsideSessionToolOnlyModelAttribution verifies that
// tool-only assistant turns (no text content) still get their model
// attributed from the pendingInferences step_id lookup.
func TestParsePoolsideSessionToolOnlyModelAttribution(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_toolonly.ndjson")

	// Assistant turn has no text in assistant_message.end (empty
	// assistant_message), only tool calls. Model must still be stamped.
	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.000000-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.100000-04:00","type":"session.input","session_input":{"id":"","prompt":"read file","mode":"build"}}
{"id":"inf-start","step_id":"step-to1","timestamp":"2026-07-08T07:20:52.000000-04:00","type":"tool_call.inference.start","tool_call_inference_start":{"chat_completion_request":{"model":"poolside/laguna-m.1"}}}
{"id":"asst-start","step_id":"step-to1","timestamp":"2026-07-08T07:20:52.500000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"parsed-1","step_id":"step-to1","timestamp":"2026-07-08T07:20:53.000000-04:00","type":"tool_call.parsed","tool_call_parsed":{"id":"call-1","name":"read","args":{"path":"/test/file.txt"}}}
{"id":"asst-end","step_id":"step-to1","timestamp":"2026-07-08T07:20:54.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":""}}
{"id":"inf-end","step_id":"step-to1","timestamp":"2026-07-08T07:20:55.000000-04:00","type":"tool_call.inference.end","tool_call_inference_end":{"input_tokens":500,"output_tokens":30,"cache_read_input_tokens":0,"cache_write_input_tokens":0}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	sess, msgs, usageEvents, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)

	require.Len(t, msgs, 2, "user + assistant")
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)

	// Tool-only turn: no text content.
	assert.Empty(t, msgs[1].Content, "tool-only turn must have no content")
	assert.True(t, msgs[1].HasToolUse)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "read", msgs[1].ToolCalls[0].ToolName)

	// Model must still be attributed despite empty content.
	assert.Equal(t, "poolside/laguna-m.1", msgs[1].Model,
		"tool-only assistant turn must still receive model attribution")

	// Usage event emitted.
	require.Len(t, usageEvents, 1)
	assert.Equal(t, "poolside/laguna-m.1", usageEvents[0].Model)
	assert.Equal(t, 500, usageEvents[0].InputTokens)
	assert.Equal(t, 30, usageEvents[0].OutputTokens)

	// Session aggregates.
	assert.True(t, sess.HasTotalOutputTokens)
	assert.Equal(t, 30, sess.TotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
	assert.Equal(t, 500, sess.PeakContextTokens)
}

// TestParsePoolsideSessionPeakContextIsMax ensures peak context is
// measured as the largest single inference input, not a cumulative sum
// across rapid back-to-back inferences.
func TestParsePoolsideSessionPeakContextIsMax(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(tmpDir, "trajectory-standalone_peak.ndjson")

	// Three inferences on the same model; peak should be 2500, not
	// the sum of all input tokens.
	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.000000-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.100000-04:00","type":"session.input","session_input":{"id":"","prompt":"hi","mode":"build"}}
{"id":"a1","timestamp":"2026-07-08T07:20:52.000000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"s1","step_id":"step-a","timestamp":"2026-07-08T07:20:53.000000-04:00","type":"tool_call.inference.start","tool_call_inference_start":{"chat_completion_request":{"model":"poolside/laguna-m.1"}}}
{"id":"e1","step_id":"step-a","timestamp":"2026-07-08T07:20:54.000000-04:00","type":"tool_call.inference.end","tool_call_inference_end":{"input_tokens":1000,"output_tokens":10,"cache_read_input_tokens":0,"cache_write_input_tokens":0}}
{"id":"s2","step_id":"step-b","timestamp":"2026-07-08T07:20:55.000000-04:00","type":"tool_call.inference.start","tool_call_inference_start":{"chat_completion_request":{"model":"poolside/laguna-m.1"}}}
{"id":"e2","step_id":"step-b","timestamp":"2026-07-08T07:20:56.000000-04:00","type":"tool_call.inference.end","tool_call_inference_end":{"input_tokens":2500,"output_tokens":20,"cache_read_input_tokens":0,"cache_write_input_tokens":0}}
{"id":"s3","step_id":"step-c","timestamp":"2026-07-08T07:20:57.000000-04:00","type":"tool_call.inference.start","tool_call_inference_start":{"chat_completion_request":{"model":"poolside/laguna-m.1"}}}
{"id":"e3","step_id":"step-c","timestamp":"2026-07-08T07:20:58.000000-04:00","type":"tool_call.inference.end","tool_call_inference_end":{"input_tokens":500,"output_tokens":5,"cache_read_input_tokens":0,"cache_write_input_tokens":0}}
{"id":"x","timestamp":"2026-07-08T07:20:59.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"done"}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	sess, _, usageEvents, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)

	require.Len(t, usageEvents, 3)
	assert.Equal(t, 2500, sess.PeakContextTokens,
		"PeakContextTokens must be the max of total context (input + cache read + cache write) across inferences")
}

// TestParsePoolsideSessionInferenceWithoutStart covers the edge case
// of a truncated or partial trajectory: tool_call.inference.end
// arrives without a matching start. Tokens are still accounted for in
// the session aggregates and attributed to last-known currentModel so
// the cost engine can still price the row (or mark it unpriced).
func TestParsePoolsideSessionInferenceWithoutStart(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(
		tmpDir, "trajectory-standalone_unpaired.ndjson",
	)

	// One inference.start establishes currentModel, then a
	// second inference.end arrives with a step_id that has no
	// matching start. The unpaired end must still emit an event
	// attributed to the previous turn's model.
	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.000000-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.100000-04:00","type":"session.input","session_input":{"id":"","prompt":"hi","mode":"build"}}
{"id":"a","timestamp":"2026-07-08T07:20:52.000000-04:00","type":"assistant_message.start","assistant_message_start":{}}
{"id":"s","step_id":"step-x","timestamp":"2026-07-08T07:20:53.000000-04:00","type":"tool_call.inference.start","tool_call_inference_start":{"chat_completion_request":{"model":"poolside/laguna-m.1"}}}
{"id":"e","step_id":"step-x","timestamp":"2026-07-08T07:20:54.000000-04:00","type":"tool_call.inference.end","tool_call_inference_end":{"input_tokens":100,"output_tokens":10,"cache_read_input_tokens":0,"cache_write_input_tokens":0}}
// Unpaired end: no matching start for step-y.
{"id":"e2","step_id":"step-y","timestamp":"2026-07-08T07:20:55.000000-04:00","type":"tool_call.inference.end","tool_call_inference_end":{"input_tokens":200,"output_tokens":20,"cache_read_input_tokens":0,"cache_write_input_tokens":0}}
{"id":"end","timestamp":"2026-07-08T07:20:56.000000-04:00","type":"assistant_message.end","assistant_message_end":{"assistant_message":"done"}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	_, _, usageEvents, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)

	require.Len(t, usageEvents, 2)
	assert.Equal(t, "poolside/laguna-m.1", usageEvents[0].Model)
	assert.Equal(t, 100, usageEvents[0].InputTokens)
	assert.Equal(t, "poolside/laguna-m.1", usageEvents[1].Model,
		"unpaired end falls back to last-known currentModel")
	assert.Equal(t, 200, usageEvents[1].InputTokens)
}

func TestPoolsideInferenceSourceAndDedupKeyShape(t *testing.T) {
	tmpDir := t.TempDir()
	trajectoryPath := filepath.Join(
		tmpDir, "trajectory-standalone_inference_shape.ndjson",
	)

	content := `{"id":"event-1","timestamp":"2026-07-08T07:20:51.000000-04:00","type":"session.start","session_start":{"workspace":"","working_directories":["/test"],"prompt":""}}
{"id":"event-2","timestamp":"2026-07-08T07:20:51.100000-04:00","type":"session.input","session_input":{"id":"","prompt":"hi","mode":"build"}}
{"id":"s","step_id":"step-1","timestamp":"2026-07-08T07:20:52.000000-04:00","type":"tool_call.inference.start","tool_call_inference_start":{"chat_completion_request":{"model":"poolside/laguna-m.1"}}}
{"id":"e","step_id":"step-1","timestamp":"2026-07-08T07:20:53.000000-04:00","type":"tool_call.inference.end","tool_call_inference_end":{"input_tokens":50,"output_tokens":5,"cache_read_input_tokens":0,"cache_write_input_tokens":0}}
`
	require.NoError(t, os.WriteFile(trajectoryPath, []byte(content), 0644))

	_, _, usageEvents, err := parsePoolsideSession(trajectoryPath, "", "")
	require.NoError(t, err)
	require.Len(t, usageEvents, 1)
	ev := usageEvents[0]
	assert.Equal(t, "inference", ev.Source)
	assert.Contains(t, ev.DedupKey, "step-1",
		"dedup_key must incorporate step_id so per-inference rows are not double-counted on reparse")
	assert.Contains(t, ev.SessionID, "poolside:")
	assert.NotEmpty(t, ev.OccurredAt)
}

func TestPoolsideTrajectoriesDir(t *testing.T) {
	tests := []struct {
		name     string
		root     string
		expected string
	}{
		{
			name:     "application-data root appends trajectories",
			root:     "/home/user/.local/state/poolside",
			expected: "/home/user/.local/state/poolside/trajectories",
		},
		{
			name:     "trajectories directory used as-is",
			root:     "/home/user/.local/state/poolside/trajectories",
			expected: "/home/user/.local/state/poolside/trajectories",
		},
		{
			name:     "relative trajectories used as-is",
			root:     "trajectories",
			expected: "trajectories",
		},
		{
			name:     "trajectories with trailing slash",
			root:     "/home/user/.local/state/poolside/trajectories/",
			expected: "/home/user/.local/state/poolside/trajectories",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := poolsideTrajectoriesDir(tt.root)
			assert.Equal(t, filepath.FromSlash(tt.expected), got)
		})
	}
}
