package db

import (
	"slices"

	"go.kenn.io/agentsview/internal/parser"
)

// RooCode and legacy Kilo mark context-management events as boundaries.
var compactionEventAgents = [...]string{string(parser.AgentRooCode), string(parser.AgentKiloLegacy)}

func IsCompactionEventAgent(agent string) bool {
	return slices.Contains(compactionEventAgents[:], agent)
}

func IsCompactionPlaceholder(sourceType string) bool {
	return sourceType == parser.EvenerEmptyCompactionSourceType
}
