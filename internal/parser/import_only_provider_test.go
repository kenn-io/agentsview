package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportOnlyProviderExportCapabilitiesAreAgentSpecific(t *testing.T) {
	chatGPTProvider, ok := NewProvider(AgentChatGPT, ProviderConfig{})
	require.True(t, ok)
	assert.Implements(t, (*ChatGPTExportParser)(nil), chatGPTProvider)
	assert.NotImplements(t, (*ClaudeAIExportParser)(nil), chatGPTProvider)

	claudeAIProvider, ok := NewProvider(AgentClaudeAI, ProviderConfig{})
	require.True(t, ok)
	assert.Implements(t, (*ClaudeAIExportParser)(nil), claudeAIProvider)
	assert.NotImplements(t, (*ChatGPTExportParser)(nil), claudeAIProvider)
}
