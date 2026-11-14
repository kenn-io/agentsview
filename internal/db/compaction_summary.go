package db

import (
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// RooCode and legacy Kilo mark context-management events as boundaries.
// Evener uses these placeholders for empty turns. Add new parsers with the
// same shapes here so selection and reads agree.
var compactionEventAgents = [...]string{string(parser.AgentRooCode), string(parser.AgentKiloLegacy)}
var compactionPlaceholders = [...]struct{ Subtype, Content string }{
	{"CHECKPOINT", "[CHECKPOINT]"},
	{"SUMMARY", "[SUMMARY]"},
}

func IsCompactionEventAgent(agent string) bool {
	return slices.Contains(compactionEventAgents[:], agent)
}

func IsCompactionPlaceholder(subtype, content string) bool {
	return slices.Contains(compactionPlaceholders[:], struct{ Subtype, Content string }{subtype, content})
}

func compactionSummaryMembership(b *QueryBuilder) string {
	preds := []string{"m.is_compact_boundary = " + b.dialect.trueLiteral, "m.content <> ''"}
	for _, p := range compactionPlaceholders {
		preds = append(preds, "(m.content <> "+b.Add(p.Content)+" OR COALESCE(m.source_subtype, '') <> "+b.Add(p.Subtype)+")")
	}
	return strings.Join(preds, " AND ")
}
