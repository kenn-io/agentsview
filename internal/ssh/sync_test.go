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
			parser.AgentCodex:  {"/remote/.codex/sessions"},
		},
		map[parser.AgentType][]string{
			parser.AgentGemini: {"/remote/gemini/session.json"},
		},
		[]string{"/remote/.codex/session_index.jsonl", "/shared/metadata"},
		nil,
		[]parser.AgentType{parser.AgentGemini, parser.AgentCodex},
	)

	assert.Equal(t,
		map[parser.AgentType][]string{
			parser.AgentClaude: {"/remote/claude"},
		},
		targets.Dirs,
	)
	assert.NotContains(t, targets.Files, parser.AgentGemini)
	assert.Equal(t, []string{"/shared/metadata"}, targets.AllExtraFiles())
	assert.NotContains(t, targets.ProviderExtraFiles, parser.AgentCodex)
}
