package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexNativeToolBody(t *testing.T) {
	_, msgs := runCodexParserTest(t, "", `{"type":"session_meta","payload":{"id":"native-session","cwd":"/workspace/sample"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"run the command"}]}}
{"type":"response_item","payload":{"type":"function_call","id":"native-call","call_id":"call-1","name":"exec_command","arguments":"{\"cmd\":\"input-needle\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"output-needle"}}`, false)
	require.Len(t, msgs, 2)
	assert.Empty(t, msgs[1].Content)
	assert.Equal(t, "native-call", msgs[1].SourceUUID)
	assert.Equal(t, 21, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_call"}}}, msgs[1].ContentLayout)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "[Bash]\n$ input-needle", msgs[1].ToolCalls[0].Rendering)
	assert.Equal(t, `{"cmd":"input-needle"}`, msgs[1].ToolCalls[0].InputJSON)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "output-needle", msgs[1].ToolCalls[0].ResultEvents[0].Content)
	assert.Empty(t, msgs[1].ToolResultText, "matched output belongs to the call event")
}

func TestCodexNativeNotificationBodyPreservesReservedIdentity(t *testing.T) {
	sess, msgs := runCodexParserTest(t, "", `{"type":"session_meta","payload":{"id":"native-session","cwd":"/workspace/sample"}}
{"type":"response_item","payload":{"type":"message","id":"native-notification","role":"user","content":[{"type":"input_text","text":"<subagent_notification>{\"agent_id\":\"agent-1\",\"status\":{\"completed\":\"notification-output\"}}</subagent_notification>"}]}}
{"type":"response_item","payload":{"type":"message","id":"native-user","role":"user","content":[{"type":"input_text","text":"real request"}]}}
{"type":"response_item","payload":{"type":"message","id":"native-answer","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`, false)
	require.NotNil(t, sess)
	require.Len(t, msgs, 3)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "real request", sess.FirstMessage)
	assert.Equal(t, 0, msgs[0].Ordinal)
	assert.Empty(t, msgs[0].Content)
	assert.Equal(t, "notification-output", msgs[0].ToolResultText)
	assert.Equal(t, 19, msgs[0].ContentLength)
	assert.Equal(t, "native-notification", msgs[0].SourceUUID)
	assert.Equal(t, SourceSubtypeToolResult, msgs[0].SourceSubtype)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 19}}}, msgs[0].ContentLayout)
	assert.Equal(t, 1, msgs[1].Ordinal)
	assert.Equal(t, "real request", msgs[1].Content)
	assert.Equal(t, "native-user", msgs[1].SourceUUID)
	assert.Equal(t, 2, msgs[2].Ordinal)
	assert.Equal(t, "done", msgs[2].Content)
	assert.Equal(t, "native-answer", msgs[2].SourceUUID)
}

func TestCodexNativeReasoningAndOutputBodies(t *testing.T) {
	sess, msgs := runCodexParserTest(t, "", `{"type":"session_meta","payload":{"id":"native-session","cwd":"/workspace/sample"}}
{"type":"response_item","payload":{"type":"message","id":"native-user","role":"user","content":[{"type":"input_text","text":"[Thinking] is literal"}]}}
{"type":"response_item","payload":{"type":"reasoning","id":"native-reasoning","summary":[{"type":"summary_text","text":"summary-plan"}],"content":[{"type":"reasoning_text","text":"raw-plan"}],"encrypted_content":"opaque-needle"}}
{"type":"response_item","payload":{"type":"message","id":"native-answer","role":"assistant","content":[{"type":"output_text","text":"answer"}]}}
{"type":"response_item","payload":{"type":"reasoning","id":"native-redacted","summary":[],"encrypted_content":"encrypted-needle"}}
{"type":"response_item","payload":{"type":"function_call_output","id":"native-orphan","call_id":"absent-call","output":"orphan-output"}}
{"type":"response_item","payload":{"type":"function_call_output","id":"native-no-id","call_id":"","output":[{"type":"input_text","text":"no-id-output"},{"type":"encrypted_content","encrypted_content":"result-opaque-needle"}]}}`, false)
	require.NotNil(t, sess)
	require.Len(t, msgs, 6)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", sess.FirstMessage)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.False(t, msgs[0].HasThinking)
	assert.Equal(t, "native-user", msgs[0].SourceUUID)
	assert.Empty(t, msgs[1].Content)
	assert.Equal(t, "summary-plan\n\nraw-plan", msgs[1].ThinkingText)
	assert.Equal(t, "native-reasoning", msgs[1].SourceUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "thinking", End: 12}, {Kind: "thinking", Start: 14, End: 22},
	}}, msgs[1].ContentLayout)
	assert.Equal(t, "answer", msgs[2].Content)
	assert.Equal(t, "native-answer", msgs[2].SourceUUID)
	assert.Empty(t, msgs[3].Content)
	assert.True(t, msgs[3].HasThinking)
	assert.Empty(t, msgs[3].ThinkingText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[3].ContentLayout)
	assert.Empty(t, msgs[4].Content)
	assert.Equal(t, "orphan-output", msgs[4].ToolResultText)
	assert.Equal(t, "native-orphan", msgs[4].SourceUUID)
	require.Len(t, msgs[4].ToolResults, 1)
	assert.Equal(t, "absent-call", msgs[4].ToolResults[0].ToolUseID)
	assert.Empty(t, msgs[5].Content)
	assert.Equal(t, "no-id-output", msgs[5].ToolResultText)
	assert.Equal(t, "native-no-id", msgs[5].SourceUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 12}}}, msgs[5].ContentLayout)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
		assert.NotContains(t, msg.ThinkingText, "opaque-needle")
		assert.NotContains(t, msg.ThinkingText, "encrypted-needle")
		assert.NotContains(t, msg.ToolResultText, "result-opaque-needle")
	}
}

func TestCodexNativeIncrementalBodiesKeepLateResultCoordinates(t *testing.T) {
	const uuid = "019f0000-0000-7000-8000-000000000041"
	prefix := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"019f0000-0000-7000-8000-000000000041","cwd":"/workspace/sample"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"run the command"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","id":"prefix-call","call_id":"call-1","name":"exec_command","arguments":"{\"cmd\":\"input-needle\"}"}}`,
	}, "\n") + "\n"
	root := t.TempDir()
	path := writeCodexProviderSessionContent(t, root, uuid, prefix)
	provider := newCodexTestProvider(t, root)
	source := requireCodexProviderSource(t, provider, uuid)
	fingerprint, err := provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source, Fingerprint: fingerprint})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	checkpoint := outcome.Results[0].Result.Checkpoint
	require.NotEmpty(t, checkpoint)
	appendCodexProviderContent(t, path, `{"type":"response_item","payload":{"type":"reasoning","id":"tail-reasoning","summary":[{"type":"summary_text","text":"tail-plan"}],"encrypted_content":"opaque-needle"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":"late-output"}}
{"type":"response_item","payload":{"type":"message","id":"tail-answer","role":"assistant","content":[{"type":"output_text","text":"done"}]}}
`)
	fingerprint, err = provider.Fingerprint(t.Context(), source)
	require.NoError(t, err)
	tail, status, err := provider.ParseIncremental(t.Context(), IncrementalRequest{
		Source: source, Fingerprint: fingerprint, SessionID: "codex:" + uuid,
		Offset: int64(len(prefix)), StartOrdinal: 2, Seed: checkpoint,
	})
	require.NoError(t, err)
	assert.Equal(t, IncrementalApplied, status)
	require.Len(t, tail.Messages, 2)
	assert.Equal(t, 2, tail.Messages[0].Ordinal)
	assert.Empty(t, tail.Messages[0].Content)
	assert.Equal(t, "tail-plan", tail.Messages[0].ThinkingText)
	assert.Equal(t, "tail-reasoning", tail.Messages[0].SourceUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking", End: 9}}}, tail.Messages[0].ContentLayout)
	assert.Equal(t, 3, tail.Messages[1].Ordinal)
	assert.Equal(t, "done", tail.Messages[1].Content)
	assert.Equal(t, "tail-answer", tail.Messages[1].SourceUUID)
	require.Len(t, tail.ToolCallUpdates, 1)
	assert.True(t, tail.ToolCallUpdates[0].TargetKnown)
	assert.Equal(t, 1, tail.ToolCallUpdates[0].MessageOrdinal)
	assert.Equal(t, 0, tail.ToolCallUpdates[0].CallIndex)
	require.Len(t, tail.ToolCallUpdates[0].ResultEvents, 1)
	assert.Equal(t, "late-output", tail.ToolCallUpdates[0].ResultEvents[0].Content)
}
