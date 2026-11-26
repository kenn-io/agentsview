package parser

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClaudeLaunchCallCwd(t *testing.T) {
	user := `{"type":"user","uuid":"u1","cwd":"/repo","message":{"content":"run it"}}` + "\n"
	assistant := `{"type":"assistant","uuid":"a1","parentUuid":"u1","cwd":"/repo/sub","message":{"content":[{"type":"tool_use","id":"call","name":"Bash","input":{"command":"codex exec hi"}}]}}` + "\n"
	missing := `{"type":"assistant","uuid":"a2","parentUuid":"a1","message":{"content":[{"type":"tool_use","id":"missing","name":"Bash","input":{"command":"pwd"}}]}}` + "\n"
	path := createTestFile(t, "claude-cwd.jsonl", user+assistant+missing)
	results, _, err := claudeParseFile(t.Context(), path, "project", "local", claudeParseOptions{captureCallCwd: true})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 3)
	require.Equal(t, "/repo", results[0].Session.Cwd)
	require.Len(t, results[0].Messages[1].ToolCalls, 1)
	require.Equal(t, "/repo/sub", results[0].Messages[1].ToolCalls[0].WorkingDirectory)
	require.Len(t, results[0].Messages[2].ToolCalls, 1)
	require.Empty(t, results[0].Messages[2].ToolCalls[0].WorkingDirectory)
	shared, _, err := claudeParseFile(t.Context(), path, "project", "local", claudeParseOptions{})
	require.NoError(t, err)
	require.Len(t, shared, 1)
	require.Len(t, shared[0].Messages, 3)
	require.Len(t, shared[0].Messages[1].ToolCalls, 1)
	require.Empty(t, shared[0].Messages[1].ToolCalls[0].WorkingDirectory)
	messages, _, _, _, err := claudeParseSessionFrom(t.Context(), path, int64(len(user)), claudeIncrementalScan{startOrdinal: 1, lastEntryUUID: "u1"})
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Len(t, messages[0].ToolCalls, 1)
	require.Equal(t, "/repo/sub", messages[0].ToolCalls[0].WorkingDirectory)
	require.Len(t, messages[1].ToolCalls, 1)
	require.Empty(t, messages[1].ToolCalls[0].WorkingDirectory)
}
