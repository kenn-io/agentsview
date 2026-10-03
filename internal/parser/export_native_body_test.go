package parser

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeAIExportNativeMessageBodies(t *testing.T) {
	input := `[{"uuid":"native-export","created_at":"2026-04-03T15:00:00Z","updated_at":"2026-04-03T15:01:00Z","chat_messages":[
{"uuid":"literal","sender":"human","text":"[Thinking] is literal"},
{"uuid":"ordered","sender":"assistant","content":[{"type":"text","text":"first"},{"type":"thinking","thinking":"plan"},{"type":"text","text":"after"}]},
{"uuid":"empty-reasoning","sender":"assistant","content":[{"type":"thinking","thinking":""}]},
{"uuid":"attachment","sender":"human","text":"fallback","attachments":[{"file_name":"note.txt","extracted_content":"attachment-text"}]}]}]`
	var results []ParseResult
	require.NoError(t, parseClaudeAIExport(strings.NewReader(input), func(result ParseResult) error {
		results = append(results, result)
		return nil
	}))
	require.Len(t, results, 1)
	msgs := results[0].Messages
	require.Len(t, msgs, 4)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.False(t, msgs[0].HasThinking)
	assert.Equal(t, "first\n\nafter", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 41, msgs[1].ContentLength)
	assert.Equal(t, "ordered", msgs[1].SourceUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "text", End: 5}, {Kind: "thinking", End: 4}, {Kind: "text", Start: 7, End: 12},
	}}, msgs[1].ContentLayout)
	assert.True(t, msgs[2].HasThinking)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[2].ContentLayout)
	assert.Equal(t, "fallback\n\n[Attachment: note.txt]\nattachment-text", msgs[3].Content)
	assert.Equal(t, 2, results[0].Session.UserMessageCount)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}

func TestChatGPTExportNativeMessageBodies(t *testing.T) {
	dir := t.TempDir()
	writeChatGPTFixture(t, dir, "conversations.json", `[{"conversation_id":"native-export","current_node":"n8","mapping":{
"n0":{"parent":null,"message":{"id":"orphan","author":{"role":"tool"},"content":{"content_type":"execution_output","text":"unmatched-output"}}},
"n1":{"parent":"n0","message":{"id":"literal","author":{"role":"user"},"content":{"content_type":"text","parts":["[Thinking] is literal"]}}},
"n2":{"parent":"n1","message":{"id":"reasoning","author":{"role":"assistant"},"content":{"content_type":"thoughts","thoughts":[{"content":"first-plan"},{"content":"second-plan"}]}}},
"n3":{"parent":"n2","message":{"id":"answer","author":{"role":"assistant"},"content":{"content_type":"text","parts":["answer"]}}},
"n4":{"parent":"n3","message":{"id":"code","author":{"role":"tool","name":"python"},"content":{"content_type":"code","language":"python","text":"print('input-needle')"}}},
"n5":{"parent":"n4","message":{"id":"result","author":{"role":"tool","name":"python"},"content":{"content_type":"execution_output","text":"output-needle"}}},
"n6":{"parent":"n5","message":{"id":"generated","author":{"role":"tool"},"content":{"content_type":"multimodal_text","parts":["generated-output"]}}},
"n7":{"parent":"n6","message":{"id":"system","author":{"role":"system"},"content":{"content_type":"text","parts":["system-note"]}}},
"n8":{"parent":"n7","message":{"id":"empty-reasoning","author":{"role":"assistant"},"content":{"content_type":"thoughts","thoughts":[]}}}
}}]`)
	var results []ParseResult
	require.NoError(t, parseChatGPTExport(dir, nil, func(result ParseResult) error {
		results = append(results, result)
		return nil
	}))
	require.Len(t, results, 1)
	msgs := results[0].Messages
	require.Len(t, msgs, 6)
	assert.Empty(t, msgs[0].Content)
	assert.Equal(t, "```\nunmatched-output\n```", msgs[0].ToolResultText)
	assert.Equal(t, "orphan", msgs[0].SourceUUID)
	assert.Equal(t, SourceSubtypeToolResult, msgs[0].SourceSubtype)
	assert.Equal(t, 1, results[0].Session.UserMessageCount)
	assert.Equal(t, "[Thinking] is literal", results[0].Session.FirstMessage)
	assert.Equal(t, "[Thinking] is literal", msgs[1].Content)
	assert.False(t, msgs[1].HasThinking)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "first-plan\nsecond-plan", msgs[2].ThinkingText)
	assert.Equal(t, 45, msgs[2].ContentLength)
	assert.Equal(t, "reasoning", msgs[2].SourceUUID)
	assert.Equal(t, "answer", msgs[3].Content)
	require.Len(t, msgs[3].ToolCalls, 1)
	assert.Equal(t, "```python\nprint('input-needle')\n```", msgs[3].ToolCalls[0].Rendering)
	assert.Contains(t, msgs[3].ToolCalls[0].InputJSON, "input-needle")
	require.Len(t, msgs[3].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "```\noutput-needle\n```", msgs[3].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, "generated-output", msgs[3].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "text", End: 6}, {Kind: "tool_call"}, {Kind: "tool_result", End: 16},
	}}, msgs[3].ContentLayout)
	assert.Equal(t, 24, msgs[3].ContentLength)
	assert.True(t, msgs[4].IsSystem)
	assert.True(t, msgs[5].HasThinking)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[5].ContentLayout)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}

func TestChatGPTExportGeneratedAssetsStayOutput(t *testing.T) {
	dir := t.TempDir()
	writeChatGPTFixture(t, dir, "conversations.json", `[{"conversation_id":"image-export","current_node":"image","mapping":{
"answer":{"parent":null,"message":{"id":"answer-id","author":{"role":"assistant"},"content":{"content_type":"text","parts":["answer"]}}},
"image":{"parent":"answer","message":{"id":"image-id","author":{"role":"tool"},"content":{"content_type":"multimodal_text","parts":[{"content_type":"image_asset_pointer","asset_pointer":"file-service://image-1"}]}}}
}}]`)
	assets := &nativeExportAssets{t: t}
	var results []ParseResult
	require.NoError(t, parseChatGPTExport(dir, assets, func(result ParseResult) error {
		results = append(results, result)
		return nil
	}))
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 1)
	msg := results[0].Messages[0]
	assert.Equal(t, "answer", msg.Content)
	assert.Equal(t, "![image](asset://image-1.png)", msg.ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "text", End: 6}, {Kind: "tool_result", End: 29},
	}}, msg.ContentLayout)
	assert.Equal(t, 1, assets.resolveCount)
	assert.Equal(t, 1, assets.copyCount)
}

type nativeExportAssets struct {
	t            *testing.T
	resolveCount int
	copyCount    int
}

func (a *nativeExportAssets) Resolve(pointer string) (string, bool) {
	a.t.Helper()
	assert.Equal(a.t, "file-service://image-1", pointer)
	a.resolveCount++
	return "image-1.png", true
}

func (a *nativeExportAssets) Copy(path string) (string, error) {
	a.t.Helper()
	assert.Equal(a.t, "image-1.png", path)
	a.copyCount++
	return "asset://image-1.png", nil
}

func TestChatGPTExportExecutionOutputRequiresCodeOwner(t *testing.T) {
	dir := t.TempDir()
	writeChatGPTFixture(t, dir, "conversations.json", `[{"conversation_id":"output-export","current_node":"result","mapping":{
"answer":{"parent":null,"message":{"id":"answer-id","author":{"role":"assistant"},"content":{"content_type":"text","parts":["answer"]}}},
"quote":{"parent":"answer","message":{"id":"quote-id","author":{"role":"tool"},"content":{"content_type":"tether_quote","text":"web-output"}}},
"result":{"parent":"quote","message":{"id":"result-id","author":{"role":"tool"},"content":{"content_type":"execution_output","text":"unmatched-output"}}}
}}]`)
	var results []ParseResult
	require.NoError(t, parseChatGPTExport(dir, nil, func(result ParseResult) error {
		results = append(results, result)
		return nil
	}))
	require.Len(t, results, 1)
	msgs := results[0].Messages
	require.Len(t, msgs, 2)
	require.Len(t, msgs[0].ToolCalls, 1)
	require.Len(t, msgs[0].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "> web-output", msgs[0].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, "```\nunmatched-output\n```", msgs[1].ToolResultText)
	assert.Equal(t, "result-id", msgs[1].SourceUUID)
	assert.Equal(t, SourceSubtypeToolResult, msgs[1].SourceSubtype)
}
