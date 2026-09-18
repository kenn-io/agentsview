package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportOnlyProviderExportCapabilitiesAreAgentSpecific(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	chatGPTProvider, ok := NewProvider(AgentChatGPT, ProviderConfig{})
	require.True(ok)
	assert.Implements((*ChatGPTExportParser)(nil), chatGPTProvider)
	assert.NotImplements((*ClaudeAIExportParser)(nil), chatGPTProvider)
	assert.NotImplements((*GeminiAppsExportParser)(nil), chatGPTProvider)

	claudeAIProvider, ok := NewProvider(AgentClaudeAI, ProviderConfig{})
	require.True(ok)
	assert.Implements((*ClaudeAIExportParser)(nil), claudeAIProvider)
	assert.NotImplements((*ChatGPTExportParser)(nil), claudeAIProvider)
	assert.NotImplements((*GeminiAppsExportParser)(nil), claudeAIProvider)

	geminiAppsProvider, ok := NewProvider(AgentGeminiApps, ProviderConfig{})
	require.True(ok)
	assert.Implements((*GeminiAppsExportParser)(nil), geminiAppsProvider)
	assert.NotImplements((*ClaudeAIExportParser)(nil), geminiAppsProvider)
	assert.NotImplements((*ChatGPTExportParser)(nil), geminiAppsProvider)
}
