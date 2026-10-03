package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAntigravityCLINativeTrajectoryBodies(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "conversations", "22222222-3333-4444-5555-666666666666.pb")
	writeSourceFile(t, path, "pb-stub")
	writeSourceFile(t, filepath.Join(filepath.Dir(path), "22222222-3333-4444-5555-666666666666.trajectory.json"), `{"steps":[{"type":"CORTEX_STEP_TYPE_USER_INPUT","metadata":{"createdAt":"2026-10-01T00:00:01Z"},"userInput":{"userResponse":"[Thinking] is literal"}},{"type":"CORTEX_STEP_TYPE_PLANNER_RESPONSE","metadata":{"createdAt":"2026-10-01T00:00:02Z"},"plannerResponse":{"thinking":"plan","response":"","toolCalls":[{"name":"view_file","id":"call-1","argumentsJson":"{\"path\":\"input-needle\"}"}]}},{"type":"CORTEX_STEP_TYPE_VIEW_FILE","metadata":{"createdAt":"2026-10-01T00:00:03Z"},"viewFile":{"content":"output-needle"}},{"type":"CORTEX_STEP_TYPE_PLANNER_RESPONSE","metadata":{"createdAt":"2026-10-01T00:00:04Z"},"plannerResponse":{"thinking":"","response":""}}]}`)
	sess, msgs, err := parseAntigravityCLITestSession(t, path, "project", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 4)
	assert.Equal(t, 1, sess.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Empty(t, msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 20, msgs[1].ContentLength)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "[Read: input-needle]", msgs[1].ToolCalls[0].Rendering)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking", End: 4}, {Kind: "tool_call"}}}, msgs[1].ContentLayout)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[2].ContentLayout)
	assert.True(t, msgs[3].HasThinking)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[3].ContentLayout)
}

func TestAntigravityCLINativeOutputOnlySidecar(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "conversations", "22222222-3333-4444-5555-666666666666.pb")
	writeSourceFile(t, path, "pb-stub")
	writeSourceFile(t, filepath.Join(filepath.Dir(path), "22222222-3333-4444-5555-666666666666.trajectory.json"), `{"steps":[{"type":"CORTEX_STEP_TYPE_VIEW_FILE","viewFile":{"content":"output-needle"}}]}`)
	sess, msgs, err := parseAntigravityCLITestSession(t, path, "project", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 1)
	assert.Equal(t, 0, sess.UserMessageCount)
	assert.Equal(t, "output-needle", msgs[0].ToolResultText)
	assert.Equal(t, TranscriptFidelityFull, sess.TranscriptFidelity)
}

func TestCursorIDENativeInlineBody(t *testing.T) {
	path := createCursorIDEDB(t, []cursorIDETestComposer{{id: "native-body", bubbles: []cursorIDETestBubble{
		{id: "u1", bubbleType: 1, text: "[Thinking] is literal"},
		{id: "a1", bubbleType: 2, raw: []byte(`{"type":2,"text":"first","toolFormerData":{"toolCallId":"","name":"read_file","rawArgs":"{\"path\":\"input-needle\"}","result":"output-needle"}}`)},
	}}})
	provider, ok := NewProvider(AgentCursorIDE, ProviderConfig{Roots: []string{filepath.Dir(path)}, Machine: "local"})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 2)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "first", msgs[1].Content)
	assert.Equal(t, "output-needle", msgs[1].ToolResultText)
	assert.Equal(t, "a1", msgs[1].SourceUUID)
	assert.Equal(t, 5, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 5}, {Kind: "tool_call"}, {Kind: "tool_result", End: 13}}}, msgs[1].ContentLayout)
}
