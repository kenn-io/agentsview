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

func compactionSummaryMembership(b *QueryBuilder) string {
	return "m.is_compact_boundary = " + b.dialect.trueLiteral + " AND m.content <> '' AND COALESCE(m.source_type, '') <> " + b.Add(parser.EvenerEmptyCompactionSourceType)
}
