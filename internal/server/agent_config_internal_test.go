package server

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/config"
)

func TestInsightAgentConfigMapsBinaryOverrides(t *testing.T) {
	got := insightAgentConfig(map[string]config.AgentConfig{
		"claude": {Binary: "/opt/claude"},
		"gemini": {
			Binary:      "/opt/gemini",
			Sandbox:     "sandbox-exec",
			AllowUnsafe: true,
		},
	})

	assert.Equal(t, "/opt/claude", got["claude"].Binary)
	assert.Equal(t, "/opt/gemini", got["gemini"].Binary)
	assert.Equal(t, "sandbox-exec", got["gemini"].Sandbox)
	assert.True(t, got["gemini"].AllowUnsafe)
}
