package db

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/parser"
)

func TestApplyParsedSessionIdentity(t *testing.T) {
	sess := Session{}

	ApplyParsedSessionIdentity(&sess, parser.ParsedSession{
		Agent:      parser.AgentClaude,
		AgentLabel: " Claude Code ",
		Entrypoint: " claude-sdk ",
	})

	assert.Equal(t, "claude", sess.Agent)
	assert.Equal(t, " Claude Code ", sess.AgentLabel)
	assert.Equal(t, " claude-sdk ", sess.Entrypoint)
}
