package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportOnlyProviderExportCapabilitiesAreAgentSpecific(t *testing.T) {
	chatGPTProvider, ok := NewProvider(AgentChatGPT, ProviderConfig{})
	require.True(t, ok)
	assert.Implements(t, (*ChatGPTExportParser)(nil), chatGPTProvider)
	assert.NotImplements(t, (*ClaudeAIExportParser)(nil), chatGPTProvider)
	assert.NotImplements(t, (*GeminiAppsExportParser)(nil), chatGPTProvider)

	claudeAIProvider, ok := NewProvider(AgentClaudeAI, ProviderConfig{})
	require.True(t, ok)
	assert.Implements(t, (*ClaudeAIExportParser)(nil), claudeAIProvider)
	assert.NotImplements(t, (*ChatGPTExportParser)(nil), claudeAIProvider)
	assert.NotImplements(t, (*GeminiAppsExportParser)(nil), claudeAIProvider)

	geminiAppsProvider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(t, ok)
	assert.Implements(t, (*GeminiAppsExportParser)(nil), geminiAppsProvider)
	assert.NotImplements(t, (*ClaudeAIExportParser)(nil), geminiAppsProvider)
	assert.NotImplements(t, (*ChatGPTExportParser)(nil), geminiAppsProvider)
}

// TestImportOnlyProviderContentCapabilitiesMatchParsedExports parses an export
// that uses every feature each format can carry, so a declaration that claims
// a field the parser never fills, or misses one it does, fails here.
func TestImportOnlyProviderContentCapabilitiesMatchParsedExports(t *testing.T) {
	dir := t.TempDir()
	writeChatGPTFixture(t, dir, "conversations-001.json", `[{
  "conversation_id": "caps", "title": "Caps", "current_node": "n4",
  "mapping": {
    "root": {"id": "root", "parent": null, "children": ["n1"], "message": null},
    "n1": {"id": "n1", "parent": "root", "children": ["n2"], "message": {
      "id": "m1", "create_time": 1700000010.0, "author": {"role": "user"},
      "content": {"content_type": "text", "parts": ["Run print(42)"]}, "metadata": {}}},
    "n2": {"id": "n2", "parent": "n1", "children": ["n3"], "message": {
      "id": "m2", "create_time": 1700000020.0, "author": {"role": "assistant"},
      "content": {"content_type": "thoughts", "thoughts": [{"content": "Run it."}]},
      "metadata": {"model_slug": "o1"}}},
    "n3": {"id": "n3", "parent": "n2", "children": ["n4"], "message": {
      "id": "m3", "create_time": 1700000030.0, "author": {"role": "tool", "name": "python"},
      "content": {"content_type": "code", "text": "print(42)"}, "metadata": {}}},
    "n4": {"id": "n4", "parent": "n3", "children": [], "message": {
      "id": "m4", "create_time": 1700000035.0, "author": {"role": "tool", "name": "python"},
      "content": {"content_type": "execution_output", "text": "42"}, "metadata": {}}}
  }
}]`)
	claudeAIExport := `[{
		"uuid": "caps", "name": "Caps",
		"created_at": "2026-01-20T10:00:00.000000Z",
		"updated_at": "2026-01-20T10:05:00.000000Z",
		"chat_messages": [
			{"uuid": "m1", "text": "Why?", "sender": "human",
			 "created_at": "2026-01-20T10:00:00.000000Z"},
			{"uuid": "m2", "text": "", "sender": "assistant",
			 "created_at": "2026-01-20T10:01:00.000000Z",
			 "content": [{"type": "thinking", "thinking": "deep thought"},
			             {"type": "text", "text": "Because."}]}
		]
	}]`
	geminiPath := filepath.Join(t.TempDir(), "activity.html")
	require.NoError(t, os.WriteFile(geminiPath, []byte(geminiAppsPromptedDocument(
		geminiAppsPromptedCellHTML("Jan 2, 2025, 3:04:05 PM GMT", "first prompt", "<p>answer</p>"),
	)), 0o644))

	parse := map[AgentType]func(Provider, func(ParseResult) error) error{
		AgentChatGPT: func(p Provider, fn func(ParseResult) error) error {
			return p.(ChatGPTExportParser).ParseChatGPTExport(dir, nil, fn)
		},
		AgentClaudeAI: func(p Provider, fn func(ParseResult) error) error {
			return p.(ClaudeAIExportParser).ParseClaudeAIExport(
				strings.NewReader(claudeAIExport), fn)
		},
		AgentGeminiApps: func(p Provider, fn func(ParseResult) error) error {
			_, err := p.(GeminiAppsExportParser).ParseGeminiAppsExport(geminiPath, fn)
			return err
		},
	}
	for agent, parseExport := range parse {
		t.Run(string(agent), func(t *testing.T) {
			provider, ok := NewProvider(agent, ProviderConfig{})
			require.True(t, ok)
			var results []ParseResult
			require.NoError(t, parseExport(provider, func(r ParseResult) error {
				results = append(results, r)
				return nil
			}))
			require.Len(t, results, 1)

			content := provider.Capabilities().Content
			declared := map[string]bool{
				"first_message":      content.FirstMessage == CapabilitySupported,
				"session_name":       content.SessionName == CapabilitySupported,
				"thinking":           content.Thinking == CapabilitySupported,
				"tool_calls":         content.ToolCalls == CapabilitySupported,
				"tool_result_events": content.ToolResultEvents == CapabilitySupported,
				"model":              content.Model == CapabilitySupported,
			}
			assert.Equal(t, declared, observedImportContent(results[0]))
		})
	}
}

func observedImportContent(r ParseResult) map[string]bool {
	seen := map[string]bool{
		"first_message":      r.Session.FirstMessage != "",
		"session_name":       r.Session.SessionName != "",
		"thinking":           false,
		"tool_calls":         false,
		"tool_result_events": false,
		"model":              false,
	}
	for _, m := range r.Messages {
		seen["thinking"] = seen["thinking"] || m.HasThinking
		seen["tool_calls"] = seen["tool_calls"] || len(m.ToolCalls) > 0
		seen["model"] = seen["model"] || m.Model != ""
		for _, tc := range m.ToolCalls {
			seen["tool_result_events"] = seen["tool_result_events"] || len(tc.ResultEvents) > 0
		}
	}
	return seen
}
