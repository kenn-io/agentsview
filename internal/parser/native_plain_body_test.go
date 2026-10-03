package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAiderNativePlainBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".aider.chat.history.md")
	writeSourceFile(t, path, "# aider chat started at 2026-10-01 00:00:00\n#### [Thinking] is literal\nanswer\n> output-needle\n")
	results, err := parseAiderRuns(path, "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	msgs := results[0].Messages
	require.Len(t, msgs, 3)
	require.NotNil(t, msgs[0].ContentLayout)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 6}}}, msgs[1].ContentLayout)
	assert.Equal(t, RoleAssistant, msgs[2].Role)
	assert.Equal(t, SourceSubtypeToolResult, msgs[2].SourceSubtype)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, 13, msgs[2].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[2].ContentLayout)
}

func TestGeminiAppsNativePlainBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.html")
	writeSourceFile(t, path, geminiAppsSingleCellHTML("", "My Activity History", "Prompted", "Jan 2, 2025, 3:04:05 PM EDT", "<p>[Thinking] is literal</p>"))
	provider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	var results []ParseResult
	_, err := provider.(GeminiAppsExportParser).ParseGeminiAppsExport(path, func(result ParseResult) error { results = append(results, result); return nil })
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Messages, 1)
	msg := results[0].Messages[0]
	assert.Equal(t, "[Thinking] is literal", msg.Content)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 21}}}, msg.ContentLayout)
}

func TestTraeNativePlainBodies(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "globalStorage", traeStateDBName)
	writeTraeDB(t, path, `{"list":[{"sessionId":"native","createdAt":1715340600000,"updatedAt":1715340900000,"messages":[{"role":"user","content":"[Thinking] is literal"},{"role":"assistant","content":"","agentTaskContent":{"proposal":"answer"}}]}]}`, "unrelated")
	provider, ok := NewProvider(AgentTrae, ProviderConfig{Roots: []string{root}})
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
	assert.Equal(t, "answer", msgs[1].Content)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 6}}}, msgs[1].ContentLayout)
	assert.Equal(t, 6, msgs[1].ContentLength)
}

func TestGooseNativeThinkingNotification(t *testing.T) {
	fixture := newGooseTestFixture(t)
	fixture.insertSession(t, "native", "native", "user", "")
	fixture.insertMessage(t, "native", "user", `[{"type":"text","text":"literal"}]`, 1700000000)
	fixture.insertMessage(t, "native", "assistant", `[{"type":"systemNotification","notificationType":"thinkingMessage","msg":"plan"}]`, 1700000001)
	provider, ok := NewProvider(AgentGoose, ProviderConfig{Roots: []string{fixture.pathRoot}})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	msgs := outcome.Results[0].Result.Messages
	require.Len(t, msgs, 2)
	assert.True(t, msgs[1].HasThinking)
	assert.Empty(t, msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 0, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking", End: 4}}}, msgs[1].ContentLayout)
}

func TestReasonixNativeOpaquePresence(t *testing.T) {
	path := writeReasonixJSONL(t, `{"role":"user","content":"literal"}`, `{"role":"assistant","content":"","reasoning_signature":"opaque-signature"}`)
	sess, msgs, _, err := parseReasonixSession(path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 2)
	assert.True(t, msgs[1].HasThinking)
	assert.Empty(t, msgs[1].ThinkingText)
	assert.Empty(t, msgs[1].Content)
	assert.Equal(t, 0, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[1].ContentLayout)
}
