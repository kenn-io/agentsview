// Package parsertest provides shared test helpers for stubbing
// agent definitions in the parser registry across test packages.
package parsertest

import (
	"slices"
	"testing"

	"go.kenn.io/agentsview/internal/parser"
)

// StubAgentDefs appends defs to the parser registry for the duration
// of the test and restores the original registry on cleanup. The
// registry is a package-level variable, so tests that stub it must not
// run in parallel with tests that read it.
func StubAgentDefs(t testing.TB, defs ...parser.AgentDef) {
	t.Helper()
	orig := slices.Clone(parser.Registry)
	parser.Registry = append(parser.Registry, defs...)
	t.Cleanup(func() {
		parser.Registry = orig
	})
}
