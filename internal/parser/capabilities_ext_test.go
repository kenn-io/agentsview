package parser_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/parsertest"
)

func TestAgentUsageCapabilityHelpersFailClosedAndDiverge(t *testing.T) {
	parsertest.StubAgentDefs(t,
		parser.AgentDef{
			Type:        parser.AgentType("no-token-only"),
			DisplayName: "No Token Only",
			Usage: parser.UsageCapabilities{
				NoPerMessageTokenData: true,
			},
		},
		parser.AgentDef{
			Type:        parser.AgentType("plain-usage"),
			DisplayName: "Plain Usage",
		},
	)

	// Names match registry types exactly; only the CSV filter parser
	// trims, so a padded name fails closed at the name level.
	assert.False(t, parser.AgentNameLacksPerMessageTokenData(" no-token-only "))
	assert.True(t, parser.AgentNameLacksPerMessageTokenData("no-token-only"))
	assert.False(t, parser.AgentNameLacksPerMessageTokenData("plain-usage"))
	assert.False(t, parser.AgentNameLacksPerMessageTokenData(""))
	assert.False(t, parser.AgentNameLacksPerMessageTokenData("unknown-agent"))

	assert.True(t, parser.AgentFilterLacksPerMessageTokenData(
		"copilot, vscode-copilot,no-token-only,",
	))
	assert.False(t, parser.AgentFilterLacksPerMessageTokenData(""))
	assert.False(t, parser.AgentFilterLacksPerMessageTokenData(","))
	assert.False(t, parser.AgentFilterLacksPerMessageTokenData("copilot,claude"))
	assert.False(t, parser.AgentFilterLacksPerMessageTokenData("unknown-agent"))
}
