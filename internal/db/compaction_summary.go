package db

import (
	"slices"

	"go.kenn.io/agentsview/internal/parser"
)

// These agents do not reliably distinguish summaries from boundary labels.
var compactionExcludedAgents = [...]string{string(parser.AgentRooCode), string(parser.AgentKiloLegacy), string(parser.AgentEvener)}

func IsCompactionSummaryExcludedAgent(agent string) bool {
	return slices.Contains(compactionExcludedAgents[:], agent)
}
