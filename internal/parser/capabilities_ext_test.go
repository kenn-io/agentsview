package parser_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/parsertest"
)

func TestAgentUsageCapabilityHelpersFailClosedAndDiverge(t *testing.T) {
	assert := assert.New(t)

	parsertest.StubAgentDefs(t,
		parser.AgentDef{
			Type:        parser.AgentType("no-token-only"),
			DisplayName: "No Token Only",
			Usage: parser.UsageCapabilities{
				NoPerMessageTokenData: true,
			},
		},
		parser.AgentDef{
			Type:        parser.AgentType("ai-credit-only"),
			DisplayName: "AI Credit Only",
			Usage: parser.UsageCapabilities{
				AICreditsDenominated: true,
			},
		},
	)

	// Names match registry types exactly; only the CSV filter parser
	// trims, so a padded name fails closed at the name level.
	assert.False(parser.AgentNameLacksPerMessageTokenData(" no-token-only "))
	assert.True(parser.AgentNameLacksPerMessageTokenData("no-token-only"))
	assert.False(parser.AgentNameUsesAICredits("no-token-only"))
	assert.False(parser.AgentNameLacksPerMessageTokenData("ai-credit-only"))
	assert.True(parser.AgentNameUsesAICredits("ai-credit-only"))
	assert.False(parser.AgentNameLacksPerMessageTokenData(""))
	assert.False(parser.AgentNameUsesAICredits(""))
	assert.False(parser.AgentNameLacksPerMessageTokenData("unknown-agent"))
	assert.False(parser.AgentNameUsesAICredits("unknown-agent"))

	assert.True(parser.AgentFilterLacksPerMessageTokenData(
		"copilot, vscode-copilot,no-token-only,",
	))
	assert.False(parser.AgentFilterLacksPerMessageTokenData(""))
	assert.False(parser.AgentFilterLacksPerMessageTokenData(","))
	assert.False(parser.AgentFilterLacksPerMessageTokenData("copilot,claude"))
	assert.False(parser.AgentFilterLacksPerMessageTokenData("unknown-agent"))
}
