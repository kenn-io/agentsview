package server

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/config"
)

func TestInsightAgentConfigMapsBinaryOverrides(t *testing.T) {
	assert := assert.New(t)

	got := insightAgentConfig(map[string]config.AgentConfig{
		"claude": {Binary: "/opt/claude"},
		"gemini": {
			Binary:      "/opt/gemini",
			Sandbox:     "sandbox-exec",
			AllowUnsafe: true,
		},
	})

	assert.Equal("/opt/claude", got["claude"].Binary)
	assert.Equal("/opt/gemini", got["gemini"].Binary)
	assert.Equal("sandbox-exec", got["gemini"].Sandbox)
	assert.True(got["gemini"].AllowUnsafe)
}
