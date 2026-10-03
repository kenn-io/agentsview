package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestCodexStagedNativeMessageBodies(t *testing.T) {
	const uuid = "019f0000-0000-7000-8000-000000000042"
	root := t.TempDir()
	day := filepath.Join(root, "2026", "10", "02")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(day, "rollout-2026-10-02T09-00-00-"+uuid+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"019f0000-0000-7000-8000-000000000042","cwd":"/workspace/sample"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"run the command"}]}}
{"type":"response_item","payload":{"type":"reasoning","id":"staged-reasoning","summary":[{"type":"summary_text","text":"staged-plan"}],"encrypted_content":"opaque-needle"}}
{"type":"response_item","payload":{"type":"function_call","id":"staged-call","call_id":"staged-call-1","name":"exec_command","arguments":"{\"cmd\":\"staged-input\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"staged-call-1","output":"staged-output"}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}
`), 0o644))
	cfg := parser.ProviderConfig{Roots: []string{root}, Machine: "local"}
	provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), parser.FindSourceRequest{FullSessionID: "codex:" + uuid})
	require.NoError(t, err)
	require.True(t, found)
	sink, err := newCodexStagingSink(t.Context(), t.TempDir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sink.Close()) })
	sess, msgs, _, _, _, _, err := parser.ParseCodexSessionStreaming(t.Context(), cfg, source, sink)
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 4)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Empty(t, msgs[1].Content)
	assert.Equal(t, "staged-plan", msgs[1].ThinkingText)
	assert.Equal(t, "staged-reasoning", msgs[1].SourceUUID)
	assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "thinking", End: 11}}}, msgs[1].ContentLayout)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "staged-call", msgs[2].SourceUUID)
	assert.Equal(t, &parser.ContentLayout{Version: 1, Blocks: []parser.ContentBlock{{Kind: "tool_call"}}}, msgs[2].ContentLayout)
	require.Len(t, msgs[2].ToolCalls, 1)
	assert.Equal(t, "[Bash]\n$ staged-input", msgs[2].ToolCalls[0].Rendering)
	require.Len(t, msgs[2].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "staged-call-1", msgs[2].ToolCalls[0].ResultEvents[0].ToolUseID)
	assert.Equal(t, "function_call_output", msgs[2].ToolCalls[0].ResultEvents[0].Source)
	assert.NotEqual(t, "staged-output", msgs[2].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, "done", msgs[3].Content)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}
