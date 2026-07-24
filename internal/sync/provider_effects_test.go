package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/parser"
)

func TestValidateProviderOutcome_CodebuffAccepted(t *testing.T) {
	// The Codebuff provider emits sessions with agent = AgentCodebuff,
	// including both paid Codebuff and free Freebuff sessions (distinguished
	// by AgentLabel, not agent type).
	def := parser.AgentDef{
		Type:      parser.AgentCodebuff,
		IDPrefix:  "codebuff:",
		FileBased: true,
	}
	source := parser.SourceRef{
		Provider: parser.AgentCodebuff,
		Key:      "test-source",
	}
	fingerprint := parser.SourceFingerprint{Key: "test-source"}

	outcome := parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{
			{
				Result: parser.ParseResult{
					Session: parser.ParsedSession{
						ID:    "codebuff:2026-07-15T20-01-32.065Z",
						Agent: parser.AgentCodebuff,
					},
				},
			},
		},
	}

	err := validateProviderOutcome(def, source, fingerprint, outcome)
	assert.NoError(t, err, "Codebuff provider should accept Codebuff sessions")
}

func TestValidateProviderOutcome_RejectsWrongAgent(t *testing.T) {
	// Non-Codebuff agents from the Codebuff provider should be rejected.
	def := parser.AgentDef{
		Type:      parser.AgentCodebuff,
		IDPrefix:  "codebuff:",
		FileBased: true,
	}
	source := parser.SourceRef{
		Provider: parser.AgentCodebuff,
		Key:      "test-source",
	}
	fingerprint := parser.SourceFingerprint{Key: "test-source"}

	outcome := parser.ParseOutcome{
		Results: []parser.ParseResultOutcome{
			{
				Result: parser.ParseResult{
					Session: parser.ParsedSession{
						ID:    "codebuff:2026-07-15T20-01-32.065Z",
						Agent: parser.AgentGemini,
					},
				},
			},
		},
	}

	err := validateProviderOutcome(def, source, fingerprint, outcome)
	assert.Error(t, err, "Codebuff provider should reject non-Codebuff agents")
	assert.Contains(t, err.Error(), "agent mismatch")
}

func TestValidateProviderSessionID_CodebuffPrefix(t *testing.T) {
	def := parser.AgentDef{
		Type:     parser.AgentCodebuff,
		IDPrefix: "codebuff:",
	}

	// Codebuff prefix should be accepted.
	err := validateProviderSessionID(def, "codebuff:2026-07-15T20-01-32.065Z", "session id")
	assert.NoError(t, err, "Codebuff provider should accept codebuff: prefixed IDs")

	// Gemini prefix should be rejected.
	err = validateProviderSessionID(def, "gemini:sess-id", "session id")
	assert.Error(t, err, "Codebuff provider should reject gemini: prefixed IDs")
}
