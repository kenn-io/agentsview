package db

import (
	"slices"

	"go.kenn.io/agentsview/internal/parser"
)

// RooCode and legacy Kilo mark context-management events as boundaries.
var compactionExcludedAgents = [...]string{string(parser.AgentRooCode), string(parser.AgentKiloLegacy)}

func IsCompactionSummaryExcludedAgent(agent string) bool {
	return slices.Contains(compactionExcludedAgents[:], agent)
}
