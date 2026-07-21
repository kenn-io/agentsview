// ABOUTME: Tests for the Kilo (legacy) (VSCode extension) parser.
// ABOUTME: Exercises the Cline-format ui_messages transcript
// ABOUTME: handling and the metadata-derived session summary, with no
// ABOUTME: dependency on the live VSCode globalStorage layout.
package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeKiloLegacyFixture creates a minimal Kilo (legacy) task directory
// containing the three JSON files. Callers may overwrite
// individual files by writing to the returned paths.
func writeKiloLegacyFixture(t *testing.T) (taskDir string) {
	t.Helper()
	taskDir = t.TempDir()
	require.NoError(t, os.MkdirAll(taskDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "task_metadata.json"),
		[]byte(`{"files_in_context":[]}`), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		[]byte(`[]`), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "api_conversation_history.json"),
		[]byte(`[]`), 0o644,
	))
	return taskDir
}

func TestParseKiloLegacySessionBasic(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{
			"ts":   1700000000000,
			"type": "say",
			"say":  "text",
			"text": "Review the new parser implementation",
		},
		{
			"ts":   1700000005000,
			"type": "say",
			"say":  "text",
			"text": "I will inspect the file structure first.",
		},
		{
			"ts":   1700000010000,
			"type": "ask",
			"ask":  "tool",
			"text": `{"tool":"readFile","path":"src/foo.ts"}`,
		},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	sess, parsedMsgs, err := parseKiloLegacySession(
		taskDir, "myproject", "testmachine",
	)
	require.NoError(t, err)
	require.NotNil(t, sess)

	assert.True(t, strings.HasPrefix(sess.ID, "kilo-legacy:"),
		"session id should be kilo-legacy: prefixed")
	assert.Equal(t, AgentKiloLegacy, sess.Agent)
	assert.Equal(t, "testmachine", sess.Machine)
	assert.Equal(t, "myproject", sess.Project)
	assert.Equal(t, filepath.Base(taskDir), sess.SourceSessionID)
	assert.Equal(t, "Review the new parser implementation",
		sess.FirstMessage)
	assert.Equal(t, "Review the new parser implementation",
		sess.SessionName)
	assert.Equal(t, 3, sess.MessageCount)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "kilo-legacy-task-v1", sess.SourceVersion)
	// FirstUser is user, second is assistant, third is readFile
	// tool call (assistant with no content). User count remains 1.
	require.Len(t, parsedMsgs, 3)
	assert.Equal(t, RoleUser, parsedMsgs[0].Role)
	assert.Equal(t, RoleAssistant, parsedMsgs[1].Role)
	assert.Equal(t, RoleAssistant, parsedMsgs[2].Role)
	require.Len(t, parsedMsgs[2].ToolCalls, 1)
	assert.Equal(t, "readFile", parsedMsgs[2].ToolCalls[0].ToolName)
	assert.Equal(t, "Read", parsedMsgs[2].ToolCalls[0].Category)
	assert.True(t, parsedMsgs[2].HasToolUse)
}

func TestParseKiloLegacySessionProjectFromWorkspaceDir(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	// task_metadata.json stores only workspace-relative paths, as
	// Kilo actually does — so the project must come from the
	// transcript's Current Workspace Directory line, not this file.
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "task_metadata.json"),
		[]byte(`{"files_in_context":[{"path":"src/main.go"}]}`),
		0o644,
	))
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Do the thing"},
		{
			"ts":   1700000000500,
			"type": "say",
			"say":  "api_req_started",
			"text": `{"tokensIn":100,"tokensOut":50,"cost":0.02}`,
		},
		{"ts": 1700000001000, "type": "say",
			"say":  "api_req_started",
			"text": `Current Workspace Directory (/Users/dev/code/widgets) Files\nsrc/main.go`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	sess, _, err := parseKiloLegacySession(taskDir, "hintproject", "h")
	require.NoError(t, err)
	require.NotNil(t, sess)
	// Workspace directory overrides both the relative
	// files_in_context path and the coarse hint so the session's
	// cost is attributed to the real project.
	assert.Equal(t, "widgets", sess.Project,
		"project should derive from Current Workspace Directory")
	require.Len(t, sess.UsageEvents, 1,
		"a cost usage event should be emitted for the session")
	require.NotNil(t, sess.UsageEvents[0].CostUSD)
	assert.InDelta(t, 0.02, *sess.UsageEvents[0].CostUSD, 0.0001)
}

func TestParseKiloLegacySessionProjectFromAPIHistoryFallback(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "task_metadata.json"),
		[]byte(`{"files_in_context":[{"path":"src/main.go"}]}`),
		0o644,
	))
	// ui_messages.json carries no workspace directory (short
	// session); the line is only present in the Claude-shaped
	// api_conversation_history.json environment block.
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Do the thing"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)
	api := `[
		{"role":"user","content":[{"type":"text","text":"<environment_details>\n# Current Workspace Directory (/Users/dev/code/gadget) Files\nsrc/main.go\n</environment_details>"}]},
		{"role":"assistant","content":[{"type":"text","text":"ok"}]}
	]`
	mustWriteRaw(t,
		filepath.Join(taskDir, "api_conversation_history.json"), api)

	sess, _, err := parseKiloLegacySession(taskDir, "hintproject", "h")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "gadget", sess.Project,
		"project should fall back to workspace dir from api history")
}

func TestExtractKiloLegacyWorkspaceDir(t *testing.T) {
	taskDir := t.TempDir()
	apiPath := filepath.Join(taskDir, "api_conversation_history.json")

	t.Run("from ui bytes", func(t *testing.T) {
		ui := []byte(`[{"text":"Current Workspace Directory (/a/b/proj) Files"}]`)
		require.NoError(t, os.WriteFile(apiPath, []byte(`[]`), 0o644))
		assert.Equal(t, "/a/b/proj",
			extractKiloLegacyWorkspaceDir(ui, apiPath))
	})

	t.Run("falls back to api history", func(t *testing.T) {
		require.NoError(t, os.WriteFile(apiPath,
			[]byte(`[{"content":[{"text":"Current Workspace Directory (/x/y/other) Files"}]}]`),
			0o644))
		assert.Equal(t, "/x/y/other",
			extractKiloLegacyWorkspaceDir([]byte(`[]`), apiPath))
	})

	t.Run("empty when absent", func(t *testing.T) {
		require.NoError(t, os.WriteFile(apiPath, []byte(`[]`), 0o644))
		assert.Empty(t, extractKiloLegacyWorkspaceDir([]byte(`[]`), apiPath))
	})
}

func TestParseKiloLegacySessionReadFileExtractsEmbeddedResult(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Read the file"},
		{"ts": 1700000001000, "type": "ask", "ask": "tool",
			"text": `{"tool":"readFile","path":"src/foo.ts","content":"hi"}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	require.Len(t, parsedMsgs, 2)
	tc := parsedMsgs[1].ToolCalls[0]
	assert.Equal(t, "Read", tc.Category)
	// Embedded "content" is result data and must be stripped
	// from InputJSON while populating a completed ResultEvent.
	assert.NotContains(t, tc.InputJSON, `"content"`,
		"readFile content must be stripped from InputJSON: %s",
		tc.InputJSON)
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "completed", tc.ResultEvents[0].Status)
	assert.Equal(t, "hi", tc.ResultEvents[0].Content)
}

func TestParseKiloLegacySessionAppliedDiffKeepsDiff(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Apply the change"},
		{"ts": 1700000001000, "type": "ask", "ask": "tool",
			"text": `{"tool":"appliedDiff","path":"src/foo.ts","diff":"@@ -1 +1 @@\\n-old\\n+new","content":"dummy"}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	require.Len(t, parsedMsgs, 2)
	tc := parsedMsgs[1].ToolCalls[0]
	assert.Equal(t, "Edit", tc.Category)
	// Write/edit tools: the diff is an INPUT and must stay in
	// InputJSON. No completed ResultEvent because the schema does
	// not declare "content" as result data for this tool.
	assert.Contains(t, tc.InputJSON, `"diff"`,
		"diff should remain in InputJSON for edit tools: %s",
		tc.InputJSON)
	assert.Empty(t, tc.ResultEvents,
		"appliedDiff should not generate a completed ResultEvent")
}

func TestParseKiloLegacySessionCommandOutputPairsAndFlagsError(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Run it"},
		{"ts": 1700000001000, "type": "ask", "ask": "command",
			"text": "git --version"},
		{"ts": 1700000002000, "type": "say", "say": "command_output",
			"text": "git version 2.42.0"},
		{"ts": 1700000003000, "type": "say", "say": "text",
			"text": "Now break it"},
		{"ts": 1700000004000, "type": "ask", "ask": "command",
			"text": "false"},
		{"ts": 1700000005000, "type": "say", "say": "command_output",
			"text": "exit status: 1\nexit code 1"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)

	// Both execute_command tool calls emitted.
	require.Len(t, parsedMsgs, 4)
	toolMsg1, outputMsg1, toolMsg2, outputMsg2 := parsedMsgs[0],
		parsedMsgs[1], parsedMsgs[2], parsedMsgs[3]
	// Note ordering: our loop interleaves reasoning/tool pairs, so
	// re-derive by ToolCalls.
	toolCalls := 0
	for _, m := range parsedMsgs {
		if len(m.ToolCalls) > 0 {
			toolCalls++
		}
	}
	assert.Equal(t, 2, toolCalls, "expected 2 tool-call messages")
	_ = toolMsg1
	_ = outputMsg1
	_ = toolMsg2
	_ = outputMsg2

	// The first command_output should be paired as "completed",
	// the second as "errored" because of the exit code.
	var statuses []string
	for _, m := range parsedMsgs {
		for _, tc := range m.ToolCalls {
			for _, ev := range tc.ResultEvents {
				statuses = append(statuses, ev.Status)
			}
		}
	}
	require.Len(t, statuses, 2)
	assert.Equal(t, "completed", statuses[0])
	assert.Equal(t, "errored", statuses[1])
}

func TestParseKiloLegacySessionEmptyCommandOutputStillCompletes(t *testing.T) {
	// An empty but present command_output must complete the
	// preceding execute_command call rather than leave it pending.
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Run it"},
		{"ts": 1700000001000, "type": "ask", "ask": "command",
			"text": "true"},
		{"ts": 1700000002000, "type": "say", "say": "command_output",
			"text": ""},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	require.Len(t, parsedMsgs, 2)
	last := parsedMsgs[1]
	require.Len(t, last.ToolCalls, 1)
	require.Len(t, last.ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "completed",
		last.ToolCalls[0].ResultEvents[0].Status)
}

func TestParseKiloLegacySessionMCPResponsePairs(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Use MCP"},
		{"ts": 1700000001000, "type": "ask",
			"ask": "use_mcp_server",
			"text": `{"type":"use_mcp_tool","serverName":"brave","toolName":"search","arguments":"{\"query\":\"agentsview\"}"}`},
		{"ts": 1700000002000, "type": "say",
			"say":  "mcp_server_response",
			"text": "search results"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	require.Len(t, parsedMsgs, 2)
	last := parsedMsgs[1]
	require.Len(t, last.ToolCalls, 1)
	// MCP calls classify as "Other" to match the other harness
	// parsers (Claude/OpenCode/Zencoder), with the mcp__ name form.
	assert.Equal(t, "Other", last.ToolCalls[0].Category)
	assert.Equal(t, "mcp__brave__search", last.ToolCalls[0].ToolName)
	require.Len(t, last.ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "completed",
		last.ToolCalls[0].ResultEvents[0].Status)
	assert.Equal(t, "search results",
		last.ToolCalls[0].ResultEvents[0].Content)
}

// TestParseKiloLegacySessionMCPUseMcpToolShape exercises the real
// Kilo (legacy) MCP payload: an ask="use_mcp_server" whose text is a
// JSON string of the shape {"type":"use_mcp_tool","serverName":...,
// "toolName":...,"arguments":"..."}. The parser previously read
// only toolData["tool"] and dropped every one of these calls. It
// must now surface them as MCP tool calls with the mcp__<server>__<tool>
// qualified name (consistent with Claude/OpenCode/Zencoder) and
// pair the mcp_server_response back as a result.
func TestParseKiloLegacySessionMCPUseMcpToolShape(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Use MCP"},
		{"ts": 1700000001000, "type": "ask",
			"ask": "use_mcp_server",
			"text": `{"type":"use_mcp_tool","serverName":"chrome-devtools","toolName":"take_snapshot","arguments":"{\"verbose\":false}"}`},
		{"ts": 1700000002000, "type": "say",
			"say":  "mcp_server_response",
			"text": "<snapshot result>"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	require.Len(t, parsedMsgs, 2)
	last := parsedMsgs[1]
	require.Len(t, last.ToolCalls, 1,
		"the use_mcp_tool call must not be dropped")
	tc := last.ToolCalls[0]
	// Consistent with the other harness parsers: the mcp__ name
	// classifies as "Other" rather than a dedicated "MCP" category.
	assert.Equal(t, "Other", tc.Category)
	assert.Equal(t, "mcp__chrome-devtools__take_snapshot", tc.ToolName)
	// The arguments object is preserved in InputJSON.
	assert.Contains(t, tc.InputJSON, `"verbose":false`)
	require.Len(t, tc.ResultEvents, 1)
	assert.Equal(t, "completed", tc.ResultEvents[0].Status)
	assert.Equal(t, "<snapshot result>", tc.ResultEvents[0].Content)
}

// TestParseKiloLegacyMCPToolCallUnit isolates parseKiloLegacyToolCall on
// the use_mcp_tool shape, including the no-serverName case.
func TestParseKiloLegacyMCPToolCallUnit(t *testing.T) {
	// With serverName.
	tc := parseKiloLegacyToolCall(
		`{"type":"use_mcp_tool","serverName":"srv","toolName":"do_thing","arguments":"{\"a\":1}"}`,
		3,
	)
	require.NotNil(t, tc, "use_mcp_tool call must parse")
	assert.Equal(t, "mcp__srv__do_thing", tc.ToolName)
	assert.Equal(t, "Other", tc.Category)
	assert.Contains(t, tc.InputJSON, `"a":1`)

	// Without serverName, the name falls back to the raw tool name so
	// the call is still captured rather than dropped.
	tcNoServer := parseKiloLegacyToolCall(
		`{"type":"use_mcp_tool","toolName":"bare_tool","arguments":"{}"}`,
		4,
	)
	require.NotNil(t, tcNoServer)
	assert.Equal(t, "bare_tool", tcNoServer.ToolName)
	assert.Equal(t, "Other", tcNoServer.Category)

	// Legacy Cline shape is unaffected.
	tcLegacy := parseKiloLegacyToolCall(
		`{"tool":"readFile","path":"src/foo.ts"}`, 5,
	)
	require.NotNil(t, tcLegacy)
	assert.Equal(t, "readFile", tcLegacy.ToolName)
	assert.Equal(t, "Read", tcLegacy.Category)
}

func TestParseKiloLegacySessionCompactBoundaryEmitted(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Need to condense"},
		{"ts": 1700000001000, "type": "say",
			"say": "condense_context"},
		{"ts": 1700000002000, "type": "say", "say": "text",
			"text": "Continuing"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	require.Len(t, parsedMsgs, 3)
	assert.True(t, parsedMsgs[1].IsCompactBoundary,
		"condense_context should be a compact boundary")
	assert.Equal(t, RoleSystem, parsedMsgs[1].Role)
	assert.True(t, parsedMsgs[1].IsSystem)
}

func TestParseKiloLegacySessionDiffErrorPairsPendingToolCall(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Edit"},
		{"ts": 1700000001000, "type": "ask", "ask": "tool",
			"text": `{"tool":"appliedDiff","path":"src/foo.ts","diff":"@@ -1 +1 @@\n-old\n+new"}`},
		{"ts": 1700000002000, "type": "say",
			"say": "diff_error", "text": "search text not found"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	require.Len(t, parsedMsgs, 2)
	last := parsedMsgs[1]
	require.Len(t, last.ToolCalls, 1)
	require.Len(t, last.ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "errored",
		last.ToolCalls[0].ResultEvents[0].Status)
	assert.Equal(t, "search text not found",
		last.ToolCalls[0].ResultEvents[0].Content)
}

func TestParseKiloLegacySessionReasoningEmittedAsThinking(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "first"},
		{"ts": 1700000001000, "type": "say", "say": "reasoning",
			"reasoning": "thinking through the steps"},
		{"ts": 1700000002000, "type": "say", "say": "text",
			"text": "answer"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	require.Len(t, parsedMsgs, 3)
	assert.True(t, parsedMsgs[1].HasThinking)
	assert.Equal(t, "thinking through the steps",
		parsedMsgs[1].ThinkingText)
}

func TestParseKiloLegacySessionSkipsPartialMessages(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "first"},
		{"ts": 1700000000500, "type": "say", "say": "text",
			"text": "first (streaming)", "partial": true},
		{"ts": 1700000001000, "type": "say", "say": "text",
			"text": "second"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	require.Len(t, parsedMsgs, 2)
	assert.Equal(t, "first", parsedMsgs[0].Content)
	assert.Equal(t, "second", parsedMsgs[1].Content)
}

func TestParseKiloLegacySessionMissingMessagesFileIsEmptySession(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	require.NoError(t, os.Remove(
		filepath.Join(taskDir, "ui_messages.json"),
	))
	sess, msgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	require.NotNil(t, sess, "session should still be returned")
	assert.Empty(t, msgs, "no ui_messages means no parsed messages")
	assert.Equal(t, 0, sess.MessageCount)
}

func TestParseKiloLegacySessionAPIRecordingTracksPeakAndCost(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	api := `[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"text","text":"hello"}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}
	]`
	mustWriteRaw(t,
		filepath.Join(taskDir, "api_conversation_history.json"), api)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "first"},
		{"ts": 1700000000500, "type": "say",
			"say":  "api_req_started",
			"text": `{"tokensIn":1000,"tokensOut":50,"cacheReads":200,"cost":0.012,"usageMissing":false}`},
		{"ts": 1700000001000, "type": "say", "say": "text",
			"text": "second"},
		{"ts": 1700000001500, "type": "say",
			"say":  "api_req_started",
			"text": `{"tokensIn":2500,"tokensOut":120,"cacheReads":400,"cost":0.034,"inferenceProvider":"Z.AI","usageMissing":false}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	sess, _, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	assert.Equal(t, 170, sess.TotalOutputTokens,
		"output tokens are summed across api_req_started events")
	assert.Equal(t, 2500+400, sess.PeakContextTokens,
		"peak = max(tokensIn + cacheReads across events)")
	assert.True(t, sess.HasPeakContextTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.aggregateTokenPresenceKnown)
	require.Len(t, sess.UsageEvents, 1)
	ev := sess.UsageEvents[0]
	assert.Equal(t, 170, ev.OutputTokens)
	assert.Equal(t, 1000+2500, ev.InputTokens,
		"input tokens are summed across api_req_started events")
	require.NotNil(t, ev.CostUSD,
		"present-positive cost should populate CostUSD")
	assert.InDelta(t, 0.046, *ev.CostUSD, 0.0001,
		"summed cost across events")
	assert.Equal(t, "Z.AI", ev.Model,
		"inferenceProvider is surfaced as the usage-event model label")
}

func TestParseKiloLegacySessionModelFromAPIHistory(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)

	// Claude-shaped API history carrying the per-turn <model> inside
	// each user environment_details block. The model changes on the
	// third turn to exercise last-seen carry-forward.
	api := `[
		{"role":"user","content":[{"type":"text","text":"<environment_details>\n# Current Mode\n<slug>code</slug>\n<name>Code</name>\n<model>z-ai/glm-4.5-air:free</model>\n</environment_details>"}]},
		{"role":"assistant","content":[{"type":"text","text":"ok"}]},
		{"role":"user","content":[{"type":"text","text":"<environment_details>\n# Current Mode\n<model>z-ai/glm-4.5-air:free</model>\n</environment_details>"}]},
		{"role":"assistant","content":[{"type":"text","text":"second"}]},
		{"role":"user","content":[{"type":"text","text":"<environment_details>\n# Current Mode\n<model>moonshotai/kimi-k2.5:free</model>\n</environment_details>"}]},
		{"role":"assistant","content":[{"type":"text","text":"third"}]}
	]`
	mustWriteRaw(t,
		filepath.Join(taskDir, "api_conversation_history.json"), api)

	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "first"},
		{"ts": 1700000000500, "type": "say",
			"say":  "api_req_started",
			"text": `{"tokensIn":1000,"tokensOut":50,"cacheReads":200,"cost":0,"inferenceProvider":"Z.AI","usageMissing":false}`},
		{"ts": 1700000001000, "type": "say", "say": "text",
			"text": "second"},
		{"ts": 1700000001500, "type": "say",
			"say":  "api_req_started",
			"text": `{"tokensIn":2500,"tokensOut":120,"cacheReads":400,"cost":0,"inferenceProvider":"Z.AI","usageMissing":false}`},
		{"ts": 1700000002000, "type": "say", "say": "text",
			"text": "third"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	sess, parsed, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)

	// When multiple distinct models are observed, the model is omitted
	// from the usage event to avoid misattribution.
	require.Len(t, sess.UsageEvents, 1)
	assert.Equal(t, "", sess.UsageEvents[0].Model,
		"usage event omits model when multiple distinct models observed")
	assert.Equal(t, 1000+2500, sess.UsageEvents[0].InputTokens,
		"input tokens are summed across api_req_started events")

	// Every assistant turn carries the session's effective model.
	var asstModels []string
	for _, m := range parsed {
		if m.Role == RoleAssistant && m.Model != "" {
			asstModels = append(asstModels, m.Model)
		}
	}
	// Two assistant text turns (the api_req_started messages are
	// metadata and produce no assistant row).
	require.Len(t, asstModels, 2)
	assert.Equal(t, "moonshotai/kimi-k2.5:free", asstModels[0])
	assert.Equal(t, "moonshotai/kimi-k2.5:free", asstModels[1],
		"session effective model is applied to all assistant turns")
}

func TestParseKiloLegacyAPIHistoryModelsMissingFile(t *testing.T) {
	taskDir := t.TempDir()
	models, err := parseKiloLegacyAPIHistoryModels(
		filepath.Join(taskDir, "nope.json"))
	assert.NoError(t, err)
	assert.Empty(t, models)
}

func TestParseKiloLegacySessionMissingMetadataReturnsNilSession(t *testing.T) {
	dir := t.TempDir()
	sess, msgs, err := parseKiloLegacySession(dir, "", "h")
	require.NoError(t, err)
	assert.Nil(t, sess,
		"task dir without task_metadata.json should yield a nil session")
	assert.Nil(t, msgs)
}

func TestParseKiloLegacySessionOrphanToolCallTermination(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "first"},
		{"ts": 1700000001000, "type": "ask", "ask": "tool",
			"text": `{"tool":"readFile","path":"src/foo.ts"}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	sess, _, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	assert.Equal(t, TerminationToolCallPending,
		sess.TerminationStatus,
		"a session ending with an unresolved tool call must be tool_call_pending")
}

func TestParseKiloLegacySessionFinishTaskIsClean(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "first"},
		{"ts": 1700000001000, "type": "ask", "ask": "tool",
			"text": `{"tool":"readFile","path":"src/foo.ts"}`},
		{"ts": 1700000001500, "type": "ask", "ask": "tool",
			"text": `{"tool":"finishTask"}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	sess, _, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	assert.Equal(t, TerminationClean,
		sess.TerminationStatus,
		"a session ending with finishTask must be clean, not tool_call_pending")
}

func TestParseKiloLegacySessionFinishTaskOnlyIsClean(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "first"},
		{"ts": 1700000001000, "type": "ask", "ask": "tool",
			"text": `{"tool":"finishTask"}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	sess, _, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(t, err)
	assert.Equal(t, TerminationClean,
		sess.TerminationStatus,
		"a session whose only tool call is finishTask must be clean")
}

func TestKiloLegacyDefaultDirsCasing(t *testing.T) {
	dirs := kiloLegacyDefaultDirs()
	require.Len(t, dirs, 3, "three platform default dirs expected")
	var mac, linux, win string
	for _, d := range dirs {
		switch {
		case strings.HasPrefix(d, "Library/Application Support/"):
			mac = d
		case strings.HasPrefix(d, ".config/"):
			linux = d
		case strings.HasPrefix(d, "AppData/Roaming/"):
			win = d
		}
	}
	require.NotEmpty(t, mac, "macOS default present")
	require.NotEmpty(t, linux, "Linux default present")
	require.NotEmpty(t, win, "Windows default present")
	assert.Contains(t, mac, "kilocode.kilo-code",
		"lowercase extension id must appear on macOS")
	assert.Contains(t, linux, "kilocode.kilo-code",
		"lowercase extension id must appear on Linux")
	assert.Contains(t, win, "kilocode.kilo-code",
		"lowercase extension id must appear on Windows")
}

func TestKiloLegacyProviderCapabilities(t *testing.T) {
	caps := kiloLegacyProviderCapabilities()
	assert.Equal(t, CapabilitySupported, caps.Content.ToolCalls)
	assert.Equal(t, CapabilitySupported, caps.Content.ToolResultEvents)
	assert.Equal(t, CapabilitySupported, caps.Content.Thinking)
	assert.Equal(t, CapabilitySupported, caps.Content.AggregateUsageEvents)
	assert.Equal(t, CapabilitySupported, caps.Content.FirstMessage)
	assert.Equal(t, CapabilitySupported, caps.Content.SessionName)
	assert.Equal(t, CapabilitySupported, caps.Content.TerminationStatus)
}

func TestKiloLegacyDiscoverAndClassifyPath(t *testing.T) {
	root := t.TempDir()
	tasksDir := filepath.Join(root, "tasks")
	require.NoError(t, os.MkdirAll(tasksDir, 0o755))
	taskID := "019c06dc-dcb2-74ac-b596-c9016419612c"
	require.NoError(t, os.MkdirAll(
		filepath.Join(tasksDir, taskID), 0o755,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(tasksDir, taskID, "task_metadata.json"),
		[]byte(`{}`), 0o644,
	))
	require.NoError(t, os.MkdirAll(
		filepath.Join(tasksDir, "_index"), 0o755,
	))

	matches := kiloLegacyDiscoverFiles(root)
	require.Len(t, matches, 1)
	assert.True(t,
		filepath.IsAbs(matches[0].Path),
		"discovered path must be absolute")

	// ui_messages change should classify back to the same anchor.
	uiMsg := filepath.Join(
		tasksDir, taskID, "ui_messages.json",
	)
	match, ok := kiloLegacyClassifyPath(root, uiMsg, false)
	require.True(t, ok)
	wantAnchor := filepath.Join(
		tasksDir, taskID, "task_metadata.json",
	)
	assert.Equal(t, wantAnchor, match.Path)

	// Underscore-prefixed task dir is filtered out.
	bad := filepath.Join(tasksDir, "_index", "task_metadata.json")
	_, ok = kiloLegacyClassifyPath(root, bad, true)
	assert.False(t, ok)

	// Lookup by raw ID resolves to the same anchor.
	lookup, ok := kiloLegacyFindFile(root, taskID)
	require.True(t, ok)
	assert.Equal(t, wantAnchor, lookup.Path)
}

func TestKiloLegacyFingerprintComposite(t *testing.T) {
	root := t.TempDir()
	tasksDir := filepath.Join(root, "tasks")
	require.NoError(t, os.MkdirAll(tasksDir, 0o755))
	taskID := "abc"
	taskDir := filepath.Join(tasksDir, taskID)
	require.NoError(t, os.MkdirAll(taskDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "task_metadata.json"),
		[]byte(`{}`), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		[]byte(`[]`), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "api_conversation_history.json"),
		[]byte(`[]`), 0o644,
	))

	anchor := filepath.Join(taskDir, "task_metadata.json")
	fp, err := kiloLegacyFingerprintSource(anchor)
	require.NoError(t, err)
	assert.Greater(t, fp.Size, int64(0))
	assert.NotEmpty(t, fp.Hash)
	// Hash must change when a sibling file's content changes.
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		[]byte(`[{"ts":1,"type":"say","say":"text","text":"x"}]`),
		0o644,
	))
	fp2, err := kiloLegacyFingerprintSource(anchor)
	require.NoError(t, err)
	assert.NotEqual(t, fp.Hash, fp2.Hash,
		"sibling content change should change fingerprint hash")
}

func TestKiloLegacyParseFileReturnsUsageFromUI(t *testing.T) {
	root := t.TempDir()
	tasksDir := filepath.Join(root, "tasks")
	require.NoError(t, os.MkdirAll(tasksDir, 0o755))
	taskDir := filepath.Join(tasksDir, "abc")
	require.NoError(t, os.MkdirAll(taskDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(taskDir, "task_metadata.json"),
		[]byte(`{}`), 0o644,
	))
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "hi"},
		{"ts": 1700000000500, "type": "say",
			"say":  "api_req_started",
			"text": `{"tokensIn":500,"tokensOut":10,"cacheReads":0,"cost":0.0015,"usageMissing":false}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	matches := kiloLegacyDiscoverFiles(root)
	require.Len(t, matches, 1)
	results, _, err := kiloLegacyParseFile(
		singleFileSource{Root: root, Path: matches[0].Path},
		ParseRequest{
			Machine: "h",
			Source:  SourceRef{ProjectHint: "myproj"},
		},
	)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, 10, results[0].Session.TotalOutputTokens)
	require.Len(t, results[0].Session.UsageEvents, 1)
}

// mustWriteJSON marshalls v and writes it to path.
func mustWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, b, 0o644))
}

// mustWriteRaw writes raw bytes to path.
func mustWriteRaw(t *testing.T, path, raw string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(raw), 0o644))
}

func TestParseKiloLegacySessionCostAbsentNoUsageEvents(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), []kiloLegacyMessage{
		{Timestamp: 1688836851000, Type: "say", Say: "text", Text: "hello"},
	})
	sess, _, err := parseKiloLegacySession(taskDir, "", "")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Empty(t, sess.UsageEvents, "no api_req_started means no usage events")
}

func TestParseKiloLegacySessionToolUseIDsUnique(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []kiloLegacyMessage{
		{Timestamp: 1688836851000, Type: "say", Say: "text", Text: "task"},
		{Timestamp: 1688836852000, Type: "ask", Ask: "tool",
			Text: `{"tool":"readFile","path":"a.ts"}`},
		{Timestamp: 1688836853000, Type: "say", Say: "command_output", Text: "ok"},
		{Timestamp: 1688836854000, Type: "ask", Ask: "tool",
			Text: `{"tool":"readFile","path":"b.ts"}`},
		{Timestamp: 1688836855000, Type: "say", Say: "command_output", Text: "ok"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)
	_, parsed, err := parseKiloLegacySession(taskDir, "", "")
	require.NoError(t, err)

	ids := make(map[string]bool)
	for _, m := range parsed {
		for _, tc := range m.ToolCalls {
			assert.False(t, ids[tc.ToolUseID], "duplicate ToolUseID: %s", tc.ToolUseID)
			ids[tc.ToolUseID] = true
		}
	}
}

func TestParseKiloLegacySessionResultEventTimestamp(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []kiloLegacyMessage{
		{Timestamp: 1688836851000, Type: "say", Say: "text", Text: "task"},
		{Timestamp: 1688836852000, Type: "ask", Ask: "command",
			Text: `{"command":"ls"}`},
		{Timestamp: 1688836860000, Type: "say", Say: "command_output",
			Text: "file1\nfile2"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)
	_, parsed, err := parseKiloLegacySession(taskDir, "", "")
	require.NoError(t, err)

	var toolMsg *ParsedMessage
	for i := range parsed {
		if len(parsed[i].ToolCalls) > 0 {
			toolMsg = &parsed[i]
			break
		}
	}
	require.NotNil(t, toolMsg)
	require.NotEmpty(t, toolMsg.ToolCalls)
	require.NotEmpty(t, toolMsg.ToolCalls[0].ResultEvents)
	assert.False(t, toolMsg.ToolCalls[0].ResultEvents[0].Timestamp.IsZero(),
		"result event should carry a non-zero timestamp")
}

func TestParseKiloLegacySessionImageOnlyMessages(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []kiloLegacyMessage{
		{Timestamp: 1688836851000, Type: "say", Say: "text", Text: "",
			Images: []string{"data:image/png;base64,AAA"}},
		{Timestamp: 1688836852000, Type: "say", Say: "text",
			Text: "I see the screenshot."},
		{Timestamp: 1688836853000, Type: "say", Say: "text", Text: "",
			Images: []string{"data:image/png;base64,BBB",
				"data:image/png;base64,CCC"}},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)
	sess, parsed, err := parseKiloLegacySession(taskDir, "", "")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// First message: image-only prompt should become [image] placeholder.
	require.GreaterOrEqual(t, len(parsed), 1)
	assert.Equal(t, RoleUser, parsed[0].Role)
	assert.Contains(t, parsed[0].Content, "[image]")

	// Last message: two images -> two placeholders.
	last := parsed[len(parsed)-1]
	assert.Equal(t, RoleAssistant, last.Role)
	assert.Equal(t, "[image] [image]", last.Content)
}

func TestParseKiloLegacySessionSkillDetection(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []kiloLegacyMessage{
		{Timestamp: 1688836851000, Type: "say", Say: "text", Text: "task"},
		{Timestamp: 1688836852000, Type: "ask", Ask: "tool",
			Text: `{"tool":"skill","skillName":"my-skill","path":".codex/skills/my-skill/SKILL.md"}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)
	_, parsed, err := parseKiloLegacySession(taskDir, "", "")
	require.NoError(t, err)

	var skillTC *ParsedToolCall
	for i := range parsed {
		for j := range parsed[i].ToolCalls {
			if parsed[i].ToolCalls[j].ToolName == "skill" {
				skillTC = &parsed[i].ToolCalls[j]
			}
		}
	}
	require.NotNil(t, skillTC, "skill tool call should be extracted")
	assert.Equal(t, "Tool", skillTC.Category)
}

func TestParseKiloLegacySessionMetadataMessageSkipped(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []kiloLegacyMessage{
		{Timestamp: 1688836851000, Type: "say", Say: "text", Text: "task"},
		{Timestamp: 1688836852000, Type: "say", Say: "api_req_started",
			Text: `{"tokensIn":100,"tokensOut":50}`},
		{Timestamp: 1688836853000, Type: "say", Say: "checkpoint_saved",
			Text: ""},
		{Timestamp: 1688836854000, Type: "say", Say: "text",
			Text: "response"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)
	_, parsed, err := parseKiloLegacySession(taskDir, "", "")
	require.NoError(t, err)

	// Only the two "text" say messages should appear; metadata skipped.
	for _, m := range parsed {
		if m.Role == RoleAssistant {
			assert.Contains(t, m.Content, "response",
				"metadata messages should not appear in output")
		}
	}
}

func TestKiloLegacyFingerprintChangesOnAPIHistoryMutation(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), []kiloLegacyMessage{
		{Timestamp: 1688836851000, Type: "say", Say: "text", Text: "hello"},
	})

	fingerprint1, err := kiloLegacyFingerprintSource(
		filepath.Join(taskDir, "task_metadata.json"))
	require.NoError(t, err)

	// Mutate api_conversation_history.json and verify fingerprint changes.
	mustWriteJSON(t, filepath.Join(taskDir, "api_conversation_history.json"),
		[]kiloLegacyAPIHistoryMessage{
			{Role: "user", Content: []struct {
				Text string `json:"text"`
			}{{Text: "new content"}}},
		})

	fingerprint2, err := kiloLegacyFingerprintSource(
		filepath.Join(taskDir, "task_metadata.json"))
	require.NoError(t, err)
	assert.NotEqual(t, fingerprint1.Hash, fingerprint2.Hash,
		"changing api_conversation_history.json should change fingerprint")
}

func TestParseKiloLegacySessionCodebaseSearchResultPairs(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []kiloLegacyMessage{
		{Timestamp: 1688836851000, Type: "say", Say: "text", Text: "task"},
		{Timestamp: 1688836852000, Type: "ask", Ask: "tool",
			Text: `{"tool":"codebaseSearch","query":"find foo","path":null}`},
		{Timestamp: 1688836853000, Type: "say",
			Say: "codebase_search_result",
			Text: `{"tool":"codebaseSearch","content":{"query":"find foo","results":[{"filePath":"src/foo.ts","score":0.9}]}}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)
	_, parsed, err := parseKiloLegacySession(taskDir, "", "")
	require.NoError(t, err)

	// Find the tool call message.
	var toolMsg *ParsedMessage
	for i := range parsed {
		if len(parsed[i].ToolCalls) > 0 {
			toolMsg = &parsed[i]
			break
		}
	}
	require.NotNil(t, toolMsg)
	require.NotEmpty(t, toolMsg.ToolCalls)
	assert.Equal(t, "codebaseSearch", toolMsg.ToolCalls[0].ToolName)

	// The search result should be paired as a completed ResultEvent.
	require.NotEmpty(t, toolMsg.ToolCalls[0].ResultEvents,
		"codebase_search_result should be paired with the tool call")
	assert.Equal(t, "completed", toolMsg.ToolCalls[0].ResultEvents[0].Status)
	assert.Contains(t, toolMsg.ToolCalls[0].ResultEvents[0].Content,
		"codebaseSearch")

	// No standalone system message should be emitted for the result.
	for i := range parsed {
		m := &parsed[i]
		if m.IsSystem && m.Ordinal != toolMsg.Ordinal {
			assert.NotContains(t, m.Content, "codebase_search_result",
				"orphaned search result should not appear as standalone")
		}
	}
}

func TestParseKiloLegacySessionCodebaseSearchResultStandalone(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	// Emit a search result with no preceding tool call.
	msgs := []kiloLegacyMessage{
		{Timestamp: 1688836851000, Type: "say", Say: "text", Text: "task"},
		{Timestamp: 1688836852000, Type: "say",
			Say: "codebase_search_result",
			Text: `{"tool":"codebaseSearch","content":{"query":"orphan","results":[]}}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)
	_, parsed, err := parseKiloLegacySession(taskDir, "", "")
	require.NoError(t, err)

	// Orphaned result should appear as a standalone system message.
	found := false
	for _, m := range parsed {
		if m.IsSystem && strings.Contains(m.Content, "codebaseSearch") {
			found = true
			break
		}
	}
	assert.True(t, found,
		"orphaned codebase_search_result should emit as standalone system message")
}

func TestKiloUnwrapJSONEnvelope(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"question only", `{"question":"Should I proceed?"}`, "Should I proceed?"},
		{"suggest only", `{"suggest":"Done"}`, "Done"},
		{"question with suggest", `{"question":"Do this?","suggest":["Yes","No"]}`, "Do this?"},
		{"empty", "", ""},
		{"not json", "hello", ""},
		{"invalid json", "{bad", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kiloUnwrapJSONEnvelope(tt.input)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestParseKiloLegacySessionFollowupUnwrapsJSON(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []kiloLegacyMessage{
		{Timestamp: 1688836851000, Type: "say", Say: "text", Text: "task"},
		{Timestamp: 1688836852000, Type: "ask", Ask: "followup",
			Text: `{"question":"Should I proceed with the refactor?","suggest":["Yes","No"]}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)
	_, parsed, err := parseKiloLegacySession(taskDir, "", "")
	require.NoError(t, err)

	// Find the followup message.
	found := false
	for _, m := range parsed {
		if m.Role == RoleAssistant &&
			strings.Contains(m.Content, "Should I proceed with the refactor?") {
			found = true
			// Should NOT contain the raw JSON envelope.
			assert.NotContains(t, m.Content, "{\"question\":",
				"followup should be unwrapped, not raw JSON")
			break
		}
	}
	assert.True(t, found, "followup question text should be extracted from JSON envelope")
}

func TestParseKiloLegacySessionCompletionResultUnwrapsJSON(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	msgs := []kiloLegacyMessage{
		{Timestamp: 1688836851000, Type: "say", Say: "text", Text: "task"},
		{Timestamp: 1688836852000, Type: "ask", Ask: "completion_result",
			Text: `{"suggest":"The refactor is complete. All tests pass."}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)
	_, parsed, err := parseKiloLegacySession(taskDir, "", "")
	require.NoError(t, err)

	// Find the completion_result message.
	found := false
	for _, m := range parsed {
		if m.Role == RoleAssistant &&
			strings.Contains(m.Content, "The refactor is complete") {
			found = true
			assert.NotContains(t, m.Content, "{\"suggest\":",
				"completion_result should be unwrapped, not raw JSON")
			break
		}
	}
	assert.True(t, found, "completion_result text should be extracted from JSON envelope")
}
