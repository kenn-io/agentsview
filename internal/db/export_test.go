package db

import "go.kenn.io/agentsview/internal/parser"

func InstallDataVersionScopesForTest(scopes map[int][]parser.AgentType) func() {
	previous := dataVersionScopes
	dataVersionScopes = scopes
	return func() { dataVersionScopes = previous }
}
