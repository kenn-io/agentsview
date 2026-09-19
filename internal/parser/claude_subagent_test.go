// ABOUTME: Tests for queue-operation parsing that maps Task tool_use IDs to subagent sessions.
// ABOUTME: Validates that ParsedToolCall entries get annotated with SubagentSessionID.
package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// parseAndGetToolCalls is a helper function that takes test lines, runs the parser,
// and returns the flattened tool calls.
func parseAndGetToolCalls(t *testing.T, filename string, lines []string) []ParsedToolCall {
	t.Helper()
	content := strings.Join(lines, "\n")
	path := createTestFile(t, filename, content)
	results, err := parseClaudeSession(path, "proj", "local")
	require.NoError(t, err, "ParseClaudeSession")
	require.NotEmpty(t, results, "no results")

	var toolCalls []ParsedToolCall
	for _, msg := range results[0].Messages {
		toolCalls = append(toolCalls, msg.ToolCalls...)
	}
	return toolCalls
}

func TestSubagentSessionIDMapping(t *testing.T) {
	tests := []struct {
		name      string
		lines     []string
		wantTools []ParsedToolCall
	}{
		{
			name: "Basic Mapping",
			lines: []string{
				// user entry with uuid
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
				// assistant with Task tool_use
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"u2","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_abc123","name":"Task","input":{"description":"do stuff","subagent_type":"general-purpose","prompt":"test"}}]}}`,
				// queue-operation enqueue linking toolu_abc123 -> task_id deadbeef123
				`{"type":"queue-operation","operation":"enqueue","timestamp":"2024-01-01T10:00:01Z","sessionId":"test-session","content":"{\"task_id\":\"deadbeef123\",\"tool_use_id\":\"toolu_abc123\",\"description\":\"do stuff\",\"task_type\":\"local_agent\"}"}`,
				// user with tool_result
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u3","parentUuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_abc123","content":"done"}]}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_abc123", ToolName: "Task", Category: "Task", SubagentSessionID: "agent-deadbeef123"},
			},
		},
		{
			name: "No UUIDs",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","message":{"content":"hello"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","message":{"content":[{"type":"tool_use","id":"toolu_xyz789","name":"Task","input":{"description":"research","subagent_type":"researcher","prompt":"look stuff up"}}]}}`,
				`{"type":"queue-operation","operation":"enqueue","timestamp":"2024-01-01T10:00:01Z","sessionId":"test-session","content":"{\"task_id\":\"cafebabe\",\"tool_use_id\":\"toolu_xyz789\",\"description\":\"research\",\"task_type\":\"local_agent\"}"}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_xyz789","content":"done"}]}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_xyz789", ToolName: "Task", Category: "Task", SubagentSessionID: "agent-cafebabe"},
			},
		},
		{
			name: "Agent Tool Name",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"u2","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_agent1","name":"Agent","input":{"description":"explore codebase","subagent_type":"general-purpose","prompt":"find all API endpoints"}}]}}`,
				`{"type":"queue-operation","operation":"enqueue","timestamp":"2024-01-01T10:00:01Z","sessionId":"test-session","content":"{\"task_id\":\"agent789\",\"tool_use_id\":\"toolu_agent1\",\"description\":\"explore codebase\",\"task_type\":\"local_agent\"}"}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u3","parentUuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_agent1","content":"found 5 endpoints"}]}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_agent1", ToolName: "Agent", Category: "Task", SubagentSessionID: "agent-agent789"},
			},
		},
		{
			name: "Agent Tool Result Agent ID",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"u2","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_agent_result","name":"Agent","input":{"description":"inspect schema","subagent_type":"Explore","prompt":"inspect the schema"}}]}}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u3","parentUuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_agent_result","content":"done"}]},"toolUseResult":{"status":"completed","agentId":"abc123def4567890"}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_agent_result", ToolName: "Agent", Category: "Task", SubagentSessionID: "agent-abc123def4567890"},
			},
		},
		{
			name: "Same Message ID Additive Agent Tool Uses",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"summarize with subagents"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"a1","parentUuid":"u1","message":{"id":"msg_same","content":[{"type":"text","text":"Launching agents."}],"usage":{"input_tokens":1,"output_tokens":1},"stop_reason":"tool_use"}}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:02Z","uuid":"a2","parentUuid":"a1","message":{"id":"msg_same","content":[{"type":"tool_use","id":"toolu_first","name":"Agent","input":{"description":"first","subagent_type":"Explore","prompt":"first"}}],"usage":{"input_tokens":1,"output_tokens":2},"stop_reason":"tool_use"}}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:03Z","uuid":"a3","parentUuid":"a2","message":{"id":"msg_same","content":[{"type":"tool_use","id":"toolu_second","name":"Agent","input":{"description":"second","subagent_type":"Explore","prompt":"second"}}],"usage":{"input_tokens":1,"output_tokens":3},"stop_reason":"tool_use"}}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:04Z","uuid":"a4","parentUuid":"a3","message":{"id":"msg_same","content":[{"type":"tool_use","id":"toolu_third","name":"Agent","input":{"description":"third","subagent_type":"Explore","prompt":"third"}}],"usage":{"input_tokens":1,"output_tokens":4},"stop_reason":"tool_use"}}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"r1","parentUuid":"a2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_first","content":"done first"}]},"toolUseResult":{"status":"completed","agentId":"childfirst"}}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:06Z","uuid":"r2","parentUuid":"a3","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_second","content":"done second"}]},"toolUseResult":{"status":"completed","agentId":"childsecond"}}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:07Z","uuid":"r3","parentUuid":"a4","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_third","content":"done third"}]},"toolUseResult":{"status":"completed","agentId":"childthird"}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_first", ToolName: "Agent", Category: "Task", SubagentSessionID: "agent-childfirst"},
				{ToolUseID: "toolu_second", ToolName: "Agent", Category: "Task", SubagentSessionID: "agent-childsecond"},
				{ToolUseID: "toolu_third", ToolName: "Agent", Category: "Task", SubagentSessionID: "agent-childthird"},
			},
		},
		{
			name: "Queue Mapping Beats Tool Result Agent ID",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"u2","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_agent_result","name":"Agent","input":{"description":"inspect schema","subagent_type":"Explore","prompt":"inspect the schema"}}]}}`,
				`{"type":"queue-operation","operation":"enqueue","timestamp":"2024-01-01T10:00:02Z","sessionId":"test-session","content":"{\"task_id\":\"queuefirst\",\"tool_use_id\":\"toolu_agent_result\",\"description\":\"inspect schema\",\"task_type\":\"local_agent\"}"}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u3","parentUuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_agent_result","content":"done"}]},"toolUseResult":{"status":"completed","agentId":"resultfallback"}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_agent_result", ToolName: "Agent", Category: "Task", SubagentSessionID: "agent-queuefirst"},
			},
		},
		{
			name: "Tool Result Agent ID Non-Task Tool Unchanged",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"u2","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_read_result","name":"Read","input":{"file_path":"/tmp/foo.txt"}}]}}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u3","parentUuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_read_result","content":"file contents"}]},"toolUseResult":{"status":"completed","agentId":"notasubagent"}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_read_result", ToolName: "Read", Category: "Read", SubagentSessionID: ""},
			},
		},
		{
			name: "Tool Result Agent ID Multiple Results Ignored",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"u2","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_first_result","name":"Agent","input":{"description":"first","subagent_type":"Explore","prompt":"first"}},{"type":"tool_use","id":"toolu_second_result","name":"Agent","input":{"description":"second","subagent_type":"Explore","prompt":"second"}}]}}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u3","parentUuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_first_result","content":"first done"},{"type":"tool_result","tool_use_id":"toolu_second_result","content":"second done"}]},"toolUseResult":{"status":"completed","agentId":"ambiguousagent"}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_first_result", ToolName: "Agent", Category: "Task", SubagentSessionID: ""},
				{ToolUseID: "toolu_second_result", ToolName: "Agent", Category: "Task", SubagentSessionID: ""},
			},
		},
		{
			name: "Tool Result Agent ID String Content Skipped",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"u2","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_string_content","name":"Agent","input":{"description":"x","subagent_type":"Explore","prompt":"x"}}]}}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u3","parentUuid":"u2","message":{"content":"plain string"},"toolUseResult":{"status":"completed","agentId":"strayagent"}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_string_content", ToolName: "Agent", Category: "Task", SubagentSessionID: ""},
			},
		},
		{
			name: "Tool Result Agent ID No Tool Result Blocks Skipped",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"u2","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_no_result","name":"Agent","input":{"description":"x","subagent_type":"Explore","prompt":"x"}}]}}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u3","parentUuid":"u2","message":{"content":[{"type":"text","text":"acknowledged"}]},"toolUseResult":{"status":"completed","agentId":"strayagent"}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_no_result", ToolName: "Agent", Category: "Task", SubagentSessionID: ""},
			},
		},
		{
			name: "Non-Task Tool Unchanged",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"u2","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_read1","name":"Read","input":{"file_path":"/tmp/foo.txt"}}]}}`,
				`{"type":"queue-operation","operation":"enqueue","timestamp":"2024-01-01T10:00:01Z","sessionId":"test-session","content":"{\"task_id\":\"deadbeef\",\"tool_use_id\":\"toolu_read1\",\"description\":\"test\",\"task_type\":\"local_agent\"}"}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u3","parentUuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_read1","content":"file contents"}]}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_read1", ToolName: "Read", Category: "Read", SubagentSessionID: ""},
			},
		},
		{
			name: "XML Content",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"u2","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_01CuRUbKy9rSQUo2Beu9xjLu","name":"Task","input":{"description":"do stuff","subagent_type":"general-purpose","prompt":"test"}}]}}`,
				`{"type":"queue-operation","operation":"enqueue","timestamp":"2024-01-01T10:00:01Z","sessionId":"test-session","content":"<task-notification>\n<task-id>a02eb277c065b35a2</task-id>\n<tool-use-id>toolu_01CuRUbKy9rSQUo2Beu9xjLu</tool-use-id>\n<status>completed</status>\n<summary>Agent completed</summary>\n</task-notification>"}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u3","parentUuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_01CuRUbKy9rSQUo2Beu9xjLu","content":"done"}]}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_01CuRUbKy9rSQUo2Beu9xjLu", ToolName: "Task", Category: "Task", SubagentSessionID: "agent-a02eb277c065b35a2"},
			},
		},
		{
			name: "XML Content No UUIDs",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","message":{"content":"hello"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","message":{"content":[{"type":"tool_use","id":"toolu_01XYZ","name":"Task","input":{"description":"research","subagent_type":"researcher","prompt":"look stuff up"}}]}}`,
				`{"type":"queue-operation","operation":"enqueue","timestamp":"2024-01-01T10:00:01Z","sessionId":"test-session","content":"<task-notification>\n<task-id>beef4567</task-id>\n<tool-use-id>toolu_01XYZ</tool-use-id>\n<status>running</status>\n</task-notification>"}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_01XYZ","content":"done"}]}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_01XYZ", ToolName: "Task", Category: "Task", SubagentSessionID: "agent-beef4567"},
			},
		},
		{
			name: "Agent Progress Events",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"u2","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_bdrk_01Wt5","name":"Agent","input":{"description":"wiki search","subagent_type":"wiki-search","prompt":"search wiki"}}]}}`,
				`{"type":"progress","timestamp":"2024-01-01T10:00:02Z","parentToolUseID":"toolu_bdrk_01Wt5","data":{"type":"agent_progress","agentId":"a78243c84a44ebcd4","message":{"type":"user","message":{"role":"user","content":[{"type":"text","text":"search wiki"}]}}}}`,
				`{"type":"progress","timestamp":"2024-01-01T10:00:03Z","parentToolUseID":"toolu_bdrk_01Wt5","data":{"type":"agent_progress","agentId":"a78243c84a44ebcd4","message":{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"found it"}]}}}}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u3","parentUuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_bdrk_01Wt5","content":"wiki results"}]}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_bdrk_01Wt5", ToolName: "Agent", Category: "Task", SubagentSessionID: "agent-a78243c84a44ebcd4"},
			},
		},
		{
			name: "Multiple Subagents",
			lines: []string{
				`{"type":"user","timestamp":"2024-01-01T10:00:00Z","uuid":"u1","message":{"content":"hello"},"cwd":"/tmp"}`,
				`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","uuid":"u2","parentUuid":"u1","message":{"content":[{"type":"tool_use","id":"toolu_a","name":"Task","input":{"description":"task A","subagent_type":"general-purpose","prompt":"A"}},{"type":"tool_use","id":"toolu_b","name":"Task","input":{"description":"task B","subagent_type":"researcher","prompt":"B"}}]}}`,
				`{"type":"queue-operation","operation":"enqueue","timestamp":"2024-01-01T10:00:01Z","sessionId":"test-session","content":"{\"task_id\":\"aaa111\",\"tool_use_id\":\"toolu_a\",\"description\":\"task A\",\"task_type\":\"local_agent\"}"}`,
				`{"type":"queue-operation","operation":"enqueue","timestamp":"2024-01-01T10:00:01Z","sessionId":"test-session","content":"{\"task_id\":\"bbb222\",\"tool_use_id\":\"toolu_b\",\"description\":\"task B\",\"task_type\":\"local_agent\"}"}`,
				`{"type":"user","timestamp":"2024-01-01T10:00:05Z","uuid":"u3","parentUuid":"u2","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_a","content":"done A"},{"type":"tool_result","tool_use_id":"toolu_b","content":"done B"}]}}`,
			},
			wantTools: []ParsedToolCall{
				{ToolUseID: "toolu_a", ToolName: "Task", Category: "Task", SubagentSessionID: "agent-aaa111"},
				{ToolUseID: "toolu_b", ToolName: "Task", Category: "Task", SubagentSessionID: "agent-bbb222"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a safe filename for the test
			filename := strings.ReplaceAll(tt.name, " ", "_") + ".jsonl"
			toolCalls := parseAndGetToolCalls(t, filename, tt.lines)

			assertToolCalls(t, toolCalls, tt.wantTools)
		})
	}
}
