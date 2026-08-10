package ssh

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/parser"
)

func TestRemoteSyncExcludesDisabledProviderBeforeDownload(t *testing.T) {
	targets := filterDisabledResolvedTargets(
		map[parser.AgentType][]string{
			parser.AgentClaude: {"/remote/claude"},
			parser.AgentGemini: {"/remote/gemini"},
		},
		map[parser.AgentType][]string{
			parser.AgentGemini: {"/remote/gemini/session.json"},
		},
		nil,
		nil,
		[]parser.AgentType{parser.AgentGemini},
	)

	assert.Equal(t,
		map[parser.AgentType][]string{
			parser.AgentClaude: {"/remote/claude"},
		},
		targets.Dirs,
	)
	assert.NotContains(t, targets.Files, parser.AgentGemini)
}
