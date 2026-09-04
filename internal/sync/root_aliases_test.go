package sync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/parser"
)

func TestNewEngineKeepsCodexAliasesWhenAnotherEngineHasNone(t *testing.T) {
	t.Cleanup(func() { parser.SetCodexRootAliases(nil) })
	primary := filepath.Join(t.TempDir(), "codex", "sessions")
	alias := filepath.Join(t.TempDir(), "codex-alt", "sessions")

	local := openTestDB(t)
	NewEngine(local, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {primary}},
		RootAliases: map[parser.AgentType]map[string][]string{
			parser.AgentCodex: {primary: {alias}},
		},
	})
	assert.Equal(t, map[string][]string{primary: {alias}},
		parser.CodexRootAliases())

	// An import or rebuild engine has no local root configuration and must
	// not clear the aliases the running local engine depends on.
	other := openTestDB(t)
	NewEngine(other, EngineConfig{})
	assert.Equal(t, map[string][]string{primary: {alias}},
		parser.CodexRootAliases())
}
