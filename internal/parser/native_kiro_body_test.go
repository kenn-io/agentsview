package parser

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKiroNativeLegacyBodies(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "native-session.jsonl")
	writeSourceFile(t, path, `{"kind":"Prompt","data":{"content":[{"kind":"text","data":"[Thinking] is literal"}]}}
{"kind":"AssistantMessage","data":{"content":[{"kind":"text","data":"first"},{"kind":"toolUse","data":{"toolUseId":"call-x","name":"read","input":{"path":"input-needle"}}},{"kind":"text","data":"second"}]}}
{"kind":"ToolResults","data":{"content":[{"kind":"toolResult","data":{"toolUseId":"","content":"output-needle"}}]}}
`)
	writeSourceFile(t, filepath.Join(root, "native-session.json"), kiroProviderMetaFixture("native-session", ""))
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "native-session"})
	require.NoError(t, err)
	require.True(t, found)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 3)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "first\n\nsecond", msgs[1].Content)
	assert.Equal(t, 13, msgs[1].ContentLength)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "[Read: read]", msgs[1].ToolCalls[0].Rendering)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 5}, {Kind: "tool_call"}, {Kind: "text", Start: 7, End: 13}}}, msgs[1].ContentLayout)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[2].ContentLayout)
}

func TestKiroNativeSQLiteToolOnlyBody(t *testing.T) {
	path, conn := newKiroSQLiteTestDB(t)
	seedKiroSQLiteSession(t, conn, "", "native-session", `{"history":[{"user":{"content":{"Prompt":{"prompt":"[Thinking] is literal"}}},"assistant":{"ToolUse":{"content":"","tool_uses":[{"id":"call-x","name":"read","args":{"path":"input-needle"}}]}}}]}`, 1, 2)
	_, msgs, err := parseKiroSQLiteSession(t.Context(), path, "native-session", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Empty(t, msgs[1].Content)
	assert.Equal(t, 12, msgs[1].ContentLength)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "[Read: read]", msgs[1].ToolCalls[0].Rendering)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_call"}}}, msgs[1].ContentLayout)
}

func TestKiroNativeCurrentBodies(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sess_native", "messages.jsonl")
	writeSourceFile(t, path, `{"payload":{"type":"user","content":"[Thinking] is literal"}}
{"payload":{"type":"tool_call","toolCallId":"call-x","toolName":"read","args":{"path":"input-needle"}}}
{"payload":{"type":"tool_result","toolCallId":"","content":"output-needle"}}
`)
	provider, ok := NewProvider(AgentKiro, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: "sess_native"})
	require.NoError(t, err)
	require.True(t, found)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 3)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Empty(t, msgs[1].Content)
	assert.Equal(t, 12, msgs[1].ContentLength)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "[Read: read]", msgs[1].ToolCalls[0].Rendering)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_call"}}}, msgs[1].ContentLayout)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[2].ContentLayout)
}

func TestKiroIDENativeExecutionBodyOrder(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "workspace-sessions", "workspace", "native-session.json")
	writeSourceFile(t, path, `{"sessionId":"native-session","history":[{"message":{"role":"user","content":"[Thinking] is literal"}},{"message":{"role":"assistant","content":""},"executionId":"exec-1"},{"message":{"role":"tool","content":[{"type":"text","text":"output-needle"}]}}]}`)
	writeSourceFile(t, filepath.Join(filepath.Dir(path), "sessions.json"), `[{"workspaceDirectory":"/workspace/project"}]`)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte("/workspace/project")))[:32]
	writeSourceFile(t, filepath.Join(root, hash, kiroIDEExecSubdir, "exec.json"), `{"executionId":"exec-1","actions":[{"actionType":"say","output":{"message":"first"}},{"actionId":"call-x","actionType":"create","input":{"file":"sample.txt","modifiedContent":"input-needle"}},{"actionType":"say","output":{"message":"second"}}]}`)
	_, msgs, err := parseKiroIDESession(path, "local")
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "first\n\nsecond", msgs[1].Content)
	assert.Equal(t, 13, msgs[1].ContentLength)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "call-x", msgs[1].ToolCalls[0].ToolUseID)
	assert.Equal(t, "Write", msgs[1].ToolCalls[0].ToolName)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 5}, {Kind: "tool_call"}, {Kind: "text", Start: 7, End: 13}}}, msgs[1].ContentLayout)
	assert.Equal(t, RoleTool, msgs[2].Role)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[2].ContentLayout)
}
