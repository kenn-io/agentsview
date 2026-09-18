// ABOUTME: Tests for the Kilo (legacy) (VSCode extension) parser.
// ABOUTME: Exercises the Cline-format ui_messages transcript
// ABOUTME: handling and the metadata-derived session summary, with no
// ABOUTME: dependency on the live VSCode globalStorage layout.
package parser

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
)

func kiloLegacyDiscoverMatchesForTest(t *testing.T, root string) []singleFileMatch {
	t.Helper()
	var matches []singleFileMatch
	require.NoError(t, kiloLegacyDiscoverEach(t.Context(), root,
		func(match singleFileMatch) error {
			matches = append(matches, match)
			return nil
		}))
	return matches
}

// TestKiloLegacyDiscoveryUnreadableDirsFail guards the same contract
// as TestRooCodeDiscoveryUnreadableTasksDirFails: a traversal failure
// must propagate instead of presenting an authoritative empty
// enumeration, which would tombstone baselined sessions locally and
// let remote sync evict valid mirror data.
func TestKiloLegacyDiscoveryUnreadableDirsFail(t *testing.T) {
	parentRequire := require.New(t)

	if runtime.GOOS == "windows" {
		t.Skip("directory-permission read failures are not portable to Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := t.TempDir()
	taskDir := filepath.Join(root, "tasks", "task-1")
	parentRequire.NoError(os.MkdirAll(taskDir, 0o755))
	metadataPath := filepath.Join(taskDir, "task_metadata.json")
	parentRequire.NoError(os.WriteFile(metadataPath, []byte(`{}`), 0o644))
	provider, ok := NewProvider(AgentKiloLegacy, ProviderConfig{
		Roots: []string{root},
	})
	parentRequire.True(ok)

	matches := kiloLegacyDiscoverMatchesForTest(t, root)
	parentRequire.Len(matches, 1)
	parentRequire.Equal(metadataPath, matches[0].Path)

	t.Run("unreadable task dir", func(t *testing.T) {
		require := require.New(t)

		require.NoError(os.Chmod(taskDir, 0o000))
		t.Cleanup(func() { require.NoError(os.Chmod(taskDir, 0o755)) })
		err := kiloLegacyDiscoverEach(t.Context(), root,
			func(singleFileMatch) error { return nil })
		require.Error(err)
		assert.ErrorIs(t, err, os.ErrPermission)
		_, err = provider.Discover(t.Context())
		require.Error(err,
			"an unreadable task directory must not collect an authoritative empty discovery")
	})

	t.Run("unreadable tasks root", func(t *testing.T) {
		require := require.New(t)

		tasksDir := filepath.Join(root, "tasks")
		require.NoError(os.Chmod(tasksDir, 0o000))
		t.Cleanup(func() { require.NoError(os.Chmod(tasksDir, 0o755)) })
		err := kiloLegacyDiscoverEach(t.Context(), root,
			func(singleFileMatch) error { return nil })
		require.Error(err)
		assert.ErrorIs(t, err, os.ErrPermission)
		_, err = provider.Discover(t.Context())
		require.Error(err,
			"an unreadable tasks directory must not collect an authoritative empty discovery")
	})
}

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
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	require.NotNil(sess)

	assert.True(strings.HasPrefix(sess.ID, "kilo-legacy:"),
		"session id should be kilo-legacy: prefixed")
	assert.Equal(AgentKiloLegacy, sess.Agent)
	assert.Equal("testmachine", sess.Machine)
	assert.Equal("myproject", sess.Project)
	assert.Equal(filepath.Base(taskDir), sess.SourceSessionID)
	assert.Equal("Review the new parser implementation",
		sess.FirstMessage)
	assert.Equal("Review the new parser implementation",
		sess.SessionName)
	assert.Equal(3, sess.MessageCount)
	assert.Equal(1, sess.UserMessageCount)
	assert.Equal("kilo-legacy-task-v1", sess.SourceVersion)
	// FirstUser is user, second is assistant, third is readFile
	// tool call (assistant with no content). User count remains 1.
	require.Len(parsedMsgs, 3)
	assert.Equal(RoleUser, parsedMsgs[0].Role)
	assert.Equal(RoleAssistant, parsedMsgs[1].Role)
	assert.Equal(RoleAssistant, parsedMsgs[2].Role)
	require.Len(parsedMsgs[2].ToolCalls, 1)
	assert.Equal("readFile", parsedMsgs[2].ToolCalls[0].ToolName)
	assert.Equal("Read", parsedMsgs[2].ToolCalls[0].Category)
	assert.True(parsedMsgs[2].HasToolUse)
}

func TestParseKiloLegacySessionProjectFromWorkspaceDir(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	// task_metadata.json stores only workspace-relative paths, as
	// Kilo actually does — so the project must come from the
	// transcript's Current Workspace Directory line, not this file.
	require.NoError(os.WriteFile(
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
	require.NoError(err)
	require.NotNil(sess)
	// Workspace directory overrides both the relative
	// files_in_context path and the coarse hint so the session's
	// cost is attributed to the real project.
	assert.Equal("widgets", sess.Project,
		"project should derive from Current Workspace Directory")
	require.Len(sess.UsageEvents, 1,
		"a cost usage event should be emitted for the session")
	require.NotNil(sess.UsageEvents[0].Cost)
	assert.Equal(money.MustParseDollars("0.02"), *sess.UsageEvents[0].Cost)
}

func TestParseKiloLegacySessionProjectFromAPIHistoryFallback(t *testing.T) {
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	require.NoError(os.WriteFile(
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
	require.NoError(err)
	require.NotNil(sess)
	assert.Equal(t, "gadget", sess.Project,
		"project should fall back to workspace dir from api history")
}

func TestExtractKiloLegacyWorkspaceDir(t *testing.T) {
	taskDir := t.TempDir()
	apiPath := filepath.Join(taskDir, "api_conversation_history.json")

	t.Run("from api_req_started message", func(t *testing.T) {
		ui := []byte(`[{"type":"say","say":"api_req_started","text":"Current Workspace Directory (/a/b/proj) Files"}]`)
		require.NoError(t, os.WriteFile(apiPath, []byte(`[]`), 0o644))
		assert.Equal(t, "/a/b/proj",
			extractKiloLegacyWorkspaceDir(ui, apiPath))
	})

	t.Run("falls back to api history environment_details", func(t *testing.T) {
		require.NoError(t, os.WriteFile(apiPath,
			[]byte(`[{"role":"user","content":[{"text":"<environment_details>\nCurrent Workspace Directory (/x/y/other) Files\n</environment_details>"}]}]`),
			0o644))
		assert.Equal(t, "/x/y/other",
			extractKiloLegacyWorkspaceDir([]byte(`[]`), apiPath))
	})

	t.Run("empty when absent", func(t *testing.T) {
		require.NoError(t, os.WriteFile(apiPath, []byte(`[]`), 0o644))
		assert.Empty(t, extractKiloLegacyWorkspaceDir([]byte(`[]`), apiPath))
	})

	t.Run("windows path in decoded text", func(t *testing.T) {
		// In JSON, Windows backslashes are escaped. The raw bytes
		// contain doubled backslashes, but the decoded text field
		// has single backslashes. The regex must match the decoded
		// form, not the raw JSON.
		ui := []byte(`[{"type":"say","say":"api_req_started","text":"Current Workspace Directory (C:\\Users\\dev\\code) Files"}]`)
		require.NoError(t, os.WriteFile(apiPath, []byte(`[]`), 0o644))
		assert.Equal(t, `C:\Users\dev\code`,
			extractKiloLegacyWorkspaceDir(ui, apiPath))
	})

	t.Run("ignores user prompt with marker in ui", func(t *testing.T) {
		// A user prompt containing the workspace marker must not
		// be selected — only api_req_started messages carry the
		// authoritative environment block.
		ui := []byte(`[{"type":"say","say":"text","text":"Please set the Current Workspace Directory (/wrong/path) Files"}]`)
		require.NoError(t, os.WriteFile(apiPath, []byte(`[]`), 0o644))
		assert.Empty(t,
			extractKiloLegacyWorkspaceDir(ui, apiPath),
			"user prompt containing the marker must not be selected")
	})

	t.Run("ignores user prompt spoofing in api history", func(t *testing.T) {
		// A user prompt containing the workspace marker without
		// <environment_details> tags must not be selected — only
		// environment_details blocks carry the authoritative path.
		require.NoError(t, os.WriteFile(apiPath,
			[]byte(`[{"role":"user","content":[{"text":"Please set the Current Workspace Directory (/wrong/path) Files"}]}]`),
			0o644))
		assert.Empty(t,
			extractKiloLegacyWorkspaceDir([]byte(`[]`), apiPath),
			"user prompt without environment_details must not be selected")
	})

	t.Run("ignores assistant role in api history", func(t *testing.T) {
		// Assistant-role messages in the API history must not be
		// searched — only user-role environment blocks carry the
		// workspace directory.
		require.NoError(t, os.WriteFile(apiPath,
			[]byte(`[{"role":"assistant","content":[{"text":"<environment_details>\nCurrent Workspace Directory (/wrong/path) Files\n</environment_details>"}]}]`),
			0o644))
		assert.Empty(t,
			extractKiloLegacyWorkspaceDir([]byte(`[]`), apiPath),
			"assistant role in api history must not be searched")
	})
}

func TestParseKiloLegacySessionReadFileExtractsEmbeddedResult(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Read the file"},
		{"ts": 1700000001000, "type": "ask", "ask": "tool",
			"text": `{"tool":"readFile","path":"src/foo.ts","content":"hi"}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(err)
	require.Len(parsedMsgs, 2)
	tc := parsedMsgs[1].ToolCalls[0]
	assert.Equal("Read", tc.Category)
	// Embedded "content" is result data and must be stripped
	// from InputJSON while populating a completed ResultEvent.
	assert.NotContains(tc.InputJSON, `"content"`,
		"readFile content must be stripped from InputJSON: %s",
		tc.InputJSON)
	require.Len(tc.ResultEvents, 1)
	assert.Equal("completed", tc.ResultEvents[0].Status)
	assert.Equal("hi", tc.ResultEvents[0].Content)
}

func TestParseKiloLegacySessionAppliedDiffKeepsDiff(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Apply the change"},
		{"ts": 1700000001000, "type": "ask", "ask": "tool",
			"text": `{"tool":"appliedDiff","path":"src/foo.ts","diff":"@@ -1 +1 @@\\n-old\\n+new","content":"dummy"}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(err)
	require.Len(parsedMsgs, 2)
	tc := parsedMsgs[1].ToolCalls[0]
	assert.Equal("Edit", tc.Category)
	// Write/edit tools: the diff is an INPUT and must stay in
	// InputJSON. No completed ResultEvent because the schema does
	// not declare "content" as result data for this tool.
	assert.Contains(tc.InputJSON, `"diff"`,
		"diff should remain in InputJSON for edit tools: %s",
		tc.InputJSON)
	assert.Empty(tc.ResultEvents,
		"appliedDiff should not generate a completed ResultEvent")
}

func TestParseKiloLegacySessionCommandOutputPairsAndFlagsError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)

	// Both execute_command tool calls emitted.
	require.Len(parsedMsgs, 4)
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
	assert.Equal(2, toolCalls, "expected 2 tool-call messages")
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
	require.Len(statuses, 2)
	assert.Equal("completed", statuses[0])
	assert.Equal("errored", statuses[1])
}

func TestParseKiloLegacySessionEmptyCommandOutputStillCompletes(t *testing.T) {
	require := require.New(t)

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
	require.NoError(err)
	require.Len(parsedMsgs, 2)
	last := parsedMsgs[1]
	require.Len(last.ToolCalls, 1)
	require.Len(last.ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "completed",
		last.ToolCalls[0].ResultEvents[0].Status)
}

func TestParseKiloLegacySessionMCPResponsePairs(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Use MCP"},
		{"ts": 1700000001000, "type": "ask",
			"ask":  "use_mcp_server",
			"text": `{"type":"use_mcp_tool","serverName":"brave","toolName":"search","arguments":"{\"query\":\"agentsview\"}"}`},
		{"ts": 1700000002000, "type": "say",
			"say":  "mcp_server_response",
			"text": "search results"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(err)
	require.Len(parsedMsgs, 2)
	last := parsedMsgs[1]
	require.Len(last.ToolCalls, 1)
	// MCP calls use Category="MCP" matching RooCode, enabling
	// correct pending tracking via the mcp__ name form.
	assert.Equal("MCP", last.ToolCalls[0].Category)
	assert.Equal("mcp__brave__search", last.ToolCalls[0].ToolName)
	require.Len(last.ToolCalls[0].ResultEvents, 1)
	assert.Equal("completed",
		last.ToolCalls[0].ResultEvents[0].Status)
	assert.Equal("search results",
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
	assert := assert.New(t)
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "Use MCP"},
		{"ts": 1700000001000, "type": "ask",
			"ask":  "use_mcp_server",
			"text": `{"type":"use_mcp_tool","serverName":"chrome-devtools","toolName":"take_snapshot","arguments":"{\"verbose\":false}"}`},
		{"ts": 1700000002000, "type": "say",
			"say":  "mcp_server_response",
			"text": "<snapshot result>"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	_, parsedMsgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(err)
	require.Len(parsedMsgs, 2)
	last := parsedMsgs[1]
	require.Len(last.ToolCalls, 1,
		"the use_mcp_tool call must not be dropped")
	tc := last.ToolCalls[0]
	// MCP calls use Category="MCP" matching RooCode, enabling
	// correct pending tracking via the mcp__ name form.
	assert.Equal("MCP", tc.Category)
	assert.Equal("mcp__chrome-devtools__take_snapshot", tc.ToolName)
	// The arguments object is preserved in InputJSON.
	assert.Contains(tc.InputJSON, `"verbose":false`)
	require.Len(tc.ResultEvents, 1)
	assert.Equal("completed", tc.ResultEvents[0].Status)
	assert.Equal("<snapshot result>", tc.ResultEvents[0].Content)
}

// TestParseKiloLegacyMCPToolCallUnit isolates parseKiloLegacyToolCall on
// the use_mcp_tool shape, including the no-serverName case.
func TestParseKiloLegacyMCPToolCallUnit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	// With serverName.
	tc := parseKiloLegacyToolCall(
		`{"type":"use_mcp_tool","serverName":"srv","toolName":"do_thing","arguments":"{\"a\":1}"}`,
		3,
	)
	require.NotNil(tc, "use_mcp_tool call must parse")
	assert.Equal("mcp__srv__do_thing", tc.ToolName)
	assert.Equal("MCP", tc.Category,
		"MCP tool calls should have Category=MCP matching RooCode")
	assert.Contains(tc.InputJSON, `"a":1`)

	// Without serverName, the name falls back to the raw tool name so
	// the call is still captured rather than dropped.
	tcNoServer := parseKiloLegacyToolCall(
		`{"type":"use_mcp_tool","toolName":"bare_tool","arguments":"{}"}`,
		4,
	)
	require.NotNil(tcNoServer)
	assert.Equal("bare_tool", tcNoServer.ToolName)
	assert.Equal("MCP", tcNoServer.Category,
		"MCP calls without serverName should still have Category=MCP")

	// Legacy Cline shape is unaffected.
	tcLegacy := parseKiloLegacyToolCall(
		`{"tool":"readFile","path":"src/foo.ts"}`, 5,
	)
	require.NotNil(tcLegacy)
	assert.Equal("readFile", tcLegacy.ToolName)
	assert.Equal("Read", tcLegacy.Category)
}

func TestBuildKiloLegacyMCPInputJSONDeterministic(t *testing.T) {
	assert.Equal(t, `{"a":1,"z":2}`, buildKiloLegacyMCPInputJSON(
		map[string]any{
			"arguments": map[string]any{"z": 2, "a": 1},
		},
	))
	assert.Equal(t,
		`{"arguments":"invalid","serverName":"srv","toolName":"tool"}`,
		buildKiloLegacyMCPInputJSON(map[string]any{
			"type": "use_mcp_tool", "toolName": "tool",
			"serverName": "srv", "arguments": "invalid",
		}),
	)
}

func TestParseKiloLegacySessionCompactBoundaryEmitted(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	require.Len(parsedMsgs, 3)
	assert.True(parsedMsgs[1].IsCompactBoundary,
		"condense_context should be a compact boundary")
	assert.Equal(RoleSystem, parsedMsgs[1].Role)
	assert.True(parsedMsgs[1].IsSystem)
}

func TestParseKiloLegacySessionDiffErrorPairsPendingToolCall(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	require.Len(parsedMsgs, 2)
	last := parsedMsgs[1]
	require.Len(last.ToolCalls, 1)
	require.Len(last.ToolCalls[0].ResultEvents, 1)
	assert.Equal("errored",
		last.ToolCalls[0].ResultEvents[0].Status)
	assert.Equal("search text not found",
		last.ToolCalls[0].ResultEvents[0].Content)
}

func TestParseKiloLegacySessionReasoningEmittedAsThinking(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	require.Len(parsedMsgs, 3)
	assert.True(parsedMsgs[1].HasThinking)
	assert.Equal("thinking through the steps",
		parsedMsgs[1].ThinkingText)
}

func TestParseKiloLegacySessionSkipsPartialMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	require.Len(parsedMsgs, 2)
	assert.Equal("first", parsedMsgs[0].Content)
	assert.Equal("second", parsedMsgs[1].Content)
}

func TestParseKiloLegacySessionMissingMessagesFileIsEmptySession(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	require.NoError(os.Remove(
		filepath.Join(taskDir, "ui_messages.json"),
	))
	sess, msgs, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(err)
	require.NotNil(sess, "session should still be returned")
	assert.Empty(msgs, "no ui_messages means no parsed messages")
	assert.Equal(0, sess.MessageCount)
}

func TestParseKiloLegacySessionAPIRecordingTracksPeakAndCost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	assert.Equal(170, sess.TotalOutputTokens,
		"output tokens are summed across api_req_started events")
	assert.Equal(2500+400, sess.PeakContextTokens,
		"peak = max(tokensIn + cacheReads across events)")
	assert.True(sess.HasPeakContextTokens)
	assert.True(sess.HasTotalOutputTokens)
	assert.True(sess.aggregateTokenPresenceKnown)
	require.Len(sess.UsageEvents, 1)
	ev := sess.UsageEvents[0]
	assert.Equal(170, ev.OutputTokens)
	assert.Equal(1000+2500, ev.InputTokens,
		"input tokens are summed across api_req_started events")
	require.NotNil(ev.Cost,
		"present-positive cost should populate Cost")
	assert.Equal(money.MustParseDollars("0.046"), *ev.Cost,
		"summed cost across events")
	assert.Equal("Z.AI", ev.Model,
		"inferenceProvider is surfaced as the usage-event model label")
}

func TestParseKiloLegacySessionQuantizesEachRequestCostBeforeSumming(
	t *testing.T,
) {
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text", "text": "first"},
		{"ts": 1700000000500, "type": "say", "say": "api_req_started",
			"text": `{"tokensIn":1,"cost":0.0000004,"inferenceProvider":"Z.AI"}`},
		{"ts": 1700000001000, "type": "say", "say": "api_req_started",
			"text": `{"tokensIn":1,"cost":0.0000004,"inferenceProvider":"Z.AI"}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	sess, _, err := parseKiloLegacySession(taskDir, "", "h")

	require.NoError(err)
	require.Len(sess.UsageEvents, 1)
	require.NotNil(sess.UsageEvents[0].Cost)
	assert.Equal(t, money.Money{}, *sess.UsageEvents[0].Cost)
}

func TestParseKiloLegacySessionInvalidCostPreservesTokenUsage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text", "text": "first"},
		{"ts": 1700000000500, "type": "say", "say": "api_req_started",
			"text": `{"tokensIn":1,"cost":-0.01,"inferenceProvider":"Z.AI"}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	sess, _, err := parseKiloLegacySession(taskDir, "", "h")

	require.NoError(err)
	require.Len(sess.UsageEvents, 1)
	assert.Equal(1, sess.UsageEvents[0].InputTokens)
	assert.Nil(sess.UsageEvents[0].Cost)
}

func TestParseKiloLegacySessionModelFromAPIHistory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)

	// When multiple distinct models are observed, the model is omitted
	// from the usage event to avoid misattribution.
	require.Len(sess.UsageEvents, 1)
	assert.Equal("", sess.UsageEvents[0].Model,
		"usage event omits model when multiple distinct models observed")
	assert.Equal(1000+2500, sess.UsageEvents[0].InputTokens,
		"input tokens are summed across api_req_started events")

	// Every assistant turn carries the session's effective model.
	var asstModels []string
	for _, m := range parsed {
		if m.Role == RoleAssistant && m.Model != "" {
			asstModels = append(asstModels, m.Model)
		}
	}
	// Multi-model sessions should not stamp any model on assistant
	// messages to avoid misattribution.
	require.Len(asstModels, 0,
		"multi-model sessions should not stamp model on assistant messages")
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
	assert := assert.New(t)
	require := require.New(t)

	dirs := kiloLegacyDefaultDirs()
	require.Len(dirs, 3, "three platform default dirs expected")
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
	require.NotEmpty(mac, "macOS default present")
	require.NotEmpty(linux, "Linux default present")
	require.NotEmpty(win, "Windows default present")
	assert.Contains(mac, "kilocode.kilo-code",
		"lowercase extension id must appear on macOS")
	assert.Contains(linux, "kilocode.kilo-code",
		"lowercase extension id must appear on Linux")
	assert.Contains(win, "kilocode.kilo-code",
		"lowercase extension id must appear on Windows")
}

func TestKiloLegacyProviderCapabilities(t *testing.T) {
	assert := assert.New(t)

	caps := kiloLegacyProviderCapabilities()
	assert.Equal(CapabilitySupported, caps.Content.ToolCalls)
	assert.Equal(CapabilitySupported, caps.Content.ToolResultEvents)
	assert.Equal(CapabilitySupported, caps.Content.Thinking)
	assert.Equal(CapabilitySupported, caps.Content.AggregateUsageEvents)
	assert.Equal(CapabilitySupported, caps.Content.FirstMessage)
	assert.Equal(CapabilitySupported, caps.Content.SessionName)
	assert.Equal(CapabilitySupported, caps.Content.TerminationStatus)
}

func TestKiloLegacyDiscoverAndClassifyPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	tasksDir := filepath.Join(root, "tasks")
	require.NoError(os.MkdirAll(tasksDir, 0o755))
	taskID := "019c06dc-dcb2-74ac-b596-c9016419612c"
	require.NoError(os.MkdirAll(
		filepath.Join(tasksDir, taskID), 0o755,
	))
	require.NoError(os.WriteFile(
		filepath.Join(tasksDir, taskID, "task_metadata.json"),
		[]byte(`{}`), 0o644,
	))
	require.NoError(os.MkdirAll(
		filepath.Join(tasksDir, "_index"), 0o755,
	))

	matches := kiloLegacyDiscoverMatchesForTest(t, root)
	require.Len(matches, 1)
	assert.True(filepath.IsAbs(matches[0].Path),
		"discovered path must be absolute")

	// ui_messages change should classify back to the same anchor.
	uiMsg := filepath.Join(
		tasksDir, taskID, "ui_messages.json",
	)
	match, ok := kiloLegacyClassifyPath(root, uiMsg, false)
	require.True(ok)
	wantAnchor := filepath.Join(
		tasksDir, taskID, "task_metadata.json",
	)
	assert.Equal(wantAnchor, match.Path)

	// Underscore-prefixed task dir is filtered out.
	bad := filepath.Join(tasksDir, "_index", "task_metadata.json")
	_, ok = kiloLegacyClassifyPath(root, bad, true)
	assert.False(ok)

	// Lookup by raw ID resolves to the same anchor.
	lookup, ok := kiloLegacyFindFile(root, taskID)
	require.True(ok)
	assert.Equal(wantAnchor, lookup.Path)
}

func TestKiloLegacyFingerprintComposite(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	tasksDir := filepath.Join(root, "tasks")
	require.NoError(os.MkdirAll(tasksDir, 0o755))
	taskID := "abc"
	taskDir := filepath.Join(tasksDir, taskID)
	require.NoError(os.MkdirAll(taskDir, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(taskDir, "task_metadata.json"),
		[]byte(`{}`), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		[]byte(`[]`), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(taskDir, "api_conversation_history.json"),
		[]byte(`[]`), 0o644,
	))

	anchor := filepath.Join(taskDir, "task_metadata.json")
	fp, err := kiloLegacyFingerprintSource(anchor)
	require.NoError(err)
	assert.Greater(fp.Size, int64(0))
	assert.NotEmpty(fp.Hash)
	// Hash must change when a sibling file's content changes.
	require.NoError(os.WriteFile(
		filepath.Join(taskDir, "ui_messages.json"),
		[]byte(`[{"ts":1,"type":"say","say":"text","text":"x"}]`),
		0o644,
	))
	fp2, err := kiloLegacyFingerprintSource(anchor)
	require.NoError(err)
	assert.NotEqual(fp.Hash, fp2.Hash,
		"sibling content change should change fingerprint hash")
}

func TestKiloLegacyParseFileReturnsUsageFromUI(t *testing.T) {
	require := require.New(t)

	root := t.TempDir()
	tasksDir := filepath.Join(root, "tasks")
	require.NoError(os.MkdirAll(tasksDir, 0o755))
	taskDir := filepath.Join(tasksDir, "abc")
	require.NoError(os.MkdirAll(taskDir, 0o755))
	require.NoError(os.WriteFile(
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

	matches := kiloLegacyDiscoverMatchesForTest(t, root)
	require.Len(matches, 1)
	results, _, err := kiloLegacyParseFile(
		singleFileSource{Root: root, Path: matches[0].Path},
		ParseRequest{
			Machine: "h",
			Source:  SourceRef{ProjectHint: "myproj"},
		},
	)
	require.NoError(err)
	require.Len(results, 1)
	assert.Equal(t, 10, results[0].Session.TotalOutputTokens)
	require.Len(results[0].Session.UsageEvents, 1)
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
	require := require.New(t)

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
	require.NoError(err)

	var toolMsg *ParsedMessage
	for i := range parsed {
		if len(parsed[i].ToolCalls) > 0 {
			toolMsg = &parsed[i]
			break
		}
	}
	require.NotNil(toolMsg)
	require.NotEmpty(toolMsg.ToolCalls)
	require.NotEmpty(toolMsg.ToolCalls[0].ResultEvents)
	assert.False(t, toolMsg.ToolCalls[0].ResultEvents[0].Timestamp.IsZero(),
		"result event should carry a non-zero timestamp")
}

func TestParseKiloLegacySessionImageOnlyMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	require.NotNil(sess)

	// First message: image-only prompt should become [image] placeholder.
	require.GreaterOrEqual(len(parsed), 1)
	assert.Equal(RoleUser, parsed[0].Role)
	assert.Contains(parsed[0].Content, "[image]")

	// Last message: two images -> two placeholders.
	last := parsed[len(parsed)-1]
	assert.Equal(RoleAssistant, last.Role)
	assert.Equal("[image] [image]", last.Content)
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
	assert := assert.New(t)
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	msgs := []kiloLegacyMessage{
		{Timestamp: 1688836851000, Type: "say", Say: "text", Text: "task"},
		{Timestamp: 1688836852000, Type: "ask", Ask: "tool",
			Text: `{"tool":"codebaseSearch","query":"find foo","path":null}`},
		{Timestamp: 1688836853000, Type: "say",
			Say:  "codebase_search_result",
			Text: `{"tool":"codebaseSearch","content":{"query":"find foo","results":[{"filePath":"src/foo.ts","score":0.9}]}}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)
	_, parsed, err := parseKiloLegacySession(taskDir, "", "")
	require.NoError(err)

	// Find the tool call message.
	var toolMsg *ParsedMessage
	for i := range parsed {
		if len(parsed[i].ToolCalls) > 0 {
			toolMsg = &parsed[i]
			break
		}
	}
	require.NotNil(toolMsg)
	require.NotEmpty(toolMsg.ToolCalls)
	assert.Equal("codebaseSearch", toolMsg.ToolCalls[0].ToolName)

	// The search result should be paired as a completed ResultEvent.
	require.NotEmpty(toolMsg.ToolCalls[0].ResultEvents,
		"codebase_search_result should be paired with the tool call")
	assert.Equal("completed", toolMsg.ToolCalls[0].ResultEvents[0].Status)
	assert.Contains(toolMsg.ToolCalls[0].ResultEvents[0].Content,
		"codebaseSearch")

	// No standalone system message should be emitted for the result.
	for i := range parsed {
		m := &parsed[i]
		if m.IsSystem && m.Ordinal != toolMsg.Ordinal {
			assert.NotContains(m.Content, "codebase_search_result",
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
			Say:  "codebase_search_result",
			Text: `{"tool":"codebaseSearch","content":{"query":"orphan","results":[]}}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)
	_, parsed, err := parseKiloLegacySession(taskDir, "", "")
	require.NoError(t, err)

	// Orphaned result should appear as a standalone system message that is
	// marked as tool output.
	found := false
	for _, m := range parsed {
		if m.IsSystem && strings.Contains(m.Content, "codebaseSearch") {
			found = true
			assert.Equal(t, SourceSubtypeToolResult, m.SourceSubtype)
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

func TestParseKiloLegacySessionPartialCostExcluded(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "first"},
		// A valid JSON request payload with usageMissing and no
		// cost field — a real API request that didn't return usage
		// data. The payload is valid JSON so it counts in the
		// denominator; costPresent is false so it doesn't count
		// in requestsWithCost.
		{"ts": 1700000000500, "type": "say",
			"say":  "api_req_started",
			"text": `{"usageMissing":true}`},
		{"ts": 1700000001000, "type": "say", "say": "text",
			"text": "second"},
		{"ts": 1700000001500, "type": "say",
			"say":  "api_req_started",
			"text": `{"tokensIn":2500,"tokensOut":120,"cacheReads":400,"cost":0.034,"inferenceProvider":"Z.AI","usageMissing":false}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	sess, _, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(err)
	require.Len(sess.UsageEvents, 1)
	// The first request is a valid JSON payload (usageMissing) and
	// counts in the denominator. Since only one of two requests has
	// cost, Cost must not be set.
	assert.Nil(sess.UsageEvents[0].Cost,
		"partial cost must not be treated as authoritative")
	assert.Equal(120, sess.UsageEvents[0].OutputTokens,
		"output tokens from the priced request are still counted")
}

func TestParseKiloLegacySessionWorkspaceDirExcluded(t *testing.T) {
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "first"},
		// Non-JSON workspace metadata — should be excluded from
		// the cost-coverage denominator entirely.
		{"ts": 1700000000500, "type": "say",
			"say":  "api_req_started",
			"text": `Current Workspace Directory (/a/b/proj) Files`},
		{"ts": 1700000001000, "type": "say",
			"say":  "api_req_started",
			"text": `{"tokensIn":2500,"tokensOut":120,"cacheReads":400,"cost":0.034,"inferenceProvider":"Z.AI","usageMissing":false}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	sess, _, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(err)
	require.Len(sess.UsageEvents, 1)
	// Workspace metadata is not a valid JSON payload, so it is
	// excluded from the denominator. The single priced request
	// is authoritative.
	require.NotNil(sess.UsageEvents[0].Cost,
		"single priced request with workspace metadata excluded should be authoritative")
	assert.Equal(t, money.MustParseDollars("0.034"), *sess.UsageEvents[0].Cost)
}

func TestKiloLegacyDiscoverRejectsSymlinkedTaskDir(t *testing.T) {
	require := require.New(t)

	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	root := t.TempDir()
	tasksDir := filepath.Join(root, "tasks")
	require.NoError(os.MkdirAll(tasksDir, 0o755))

	// Create a real task directory.
	realTask := filepath.Join(tasksDir, "real-task")
	require.NoError(os.MkdirAll(realTask, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(realTask, "task_metadata.json"),
		[]byte(`{}`), 0o644,
	))

	// Create a symlinked task directory pointing outside root.
	outsideDir := filepath.Join(t.TempDir(), "escaped-task")
	require.NoError(os.MkdirAll(outsideDir, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(outsideDir, "task_metadata.json"),
		[]byte(`{}`), 0o644,
	))
	symlinkTask := filepath.Join(tasksDir, "symlink-task")
	require.NoError(os.Symlink(outsideDir, symlinkTask))

	matches := kiloLegacyDiscoverMatchesForTest(t, root)
	require.Len(matches, 1,
		"only the real task should be discovered; symlink should be rejected")
	assert.Contains(t, matches[0].Path, "real-task",
		"discovered path should be the real task, not the symlink")
}

func TestParseKiloLegacySessionMalformedAPIHistoryContinues(t *testing.T) {
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	// Malformed api_conversation_history.json — should not abort import.
	mustWriteRaw(t,
		filepath.Join(taskDir, "api_conversation_history.json"),
		`{not valid json`,
	)
	msgs := []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text",
			"text": "hello"},
		{"ts": 1700000000500, "type": "say",
			"say":  "api_req_started",
			"text": `{"tokensIn":500,"tokensOut":10,"cost":0.001,"usageMissing":false}`},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)

	sess, _, err := parseKiloLegacySession(taskDir, "", "h")
	require.NoError(err,
		"malformed api_conversation_history.json should not abort import")
	require.NotNil(sess)
	assert.Equal(t, 10, sess.TotalOutputTokens,
		"transcript should still be parsed from ui_messages.json")
	require.Len(sess.UsageEvents, 1)
}

func TestParseKiloLegacySessionUnpairedCommandOutputIsMarkedToolOutput(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	taskDir := writeKiloLegacyFixture(t)
	msgs := []kiloLegacyMessage{
		{Timestamp: 1688836851000, Type: "say", Say: "text", Text: "task"},
		{Timestamp: 1688836860000, Type: "say", Say: "command_output",
			Text: "secret=abc123"},
	}
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), msgs)
	_, parsed, err := parseKiloLegacySession(taskDir, "", "")
	require.NoError(err)

	var fallback *ParsedMessage
	for i := range parsed {
		if parsed[i].Content == "secret=abc123" {
			fallback = &parsed[i]
			break
		}
	}
	require.NotNil(fallback, "unpaired command output falls back to a row")
	assert.True(fallback.IsSystem)
	assert.Equal(SourceSubtypeToolResult, fallback.SourceSubtype,
		"the fallback row's text is tool output")
}
