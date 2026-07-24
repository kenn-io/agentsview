package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/parser"
)

func TestValidateProviderOutcome_FreebuffException(t *testing.T) {
	// The Codebuff provider may emit Freebuff sessions based on the
	// agentType field in run-state.json. Both agents share the same
	// on-disk layout and are discovered by one provider.
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
						ID:    "freebuff:2026-07-15T20-01-32.065Z",
						Agent: parser.AgentFreebuff,
					},
				},
			},
		},
	}

	err := validateProviderOutcome(def, source, fingerprint, outcome)
	assert.NoError(t, err, "Codebuff provider should accept Freebuff sessions")
}

func TestValidateProviderOutcome_RejectsWrongAgent(t *testing.T) {
	// Non-Freebuff agents from the Codebuff provider should be rejected.
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
	assert.Error(t, err, "Codebuff provider should reject non-Codebuff/Freebuff agents")
	assert.Contains(t, err.Error(), "agent mismatch")
}

func TestValidateProviderSessionID_FreebuffPrefix(t *testing.T) {
	def := parser.AgentDef{
		Type:     parser.AgentCodebuff,
		IDPrefix: "codebuff:",
	}

	// Freebuff prefix should be accepted.
	err := validateProviderSessionID(def, "freebuff:2026-07-15T20-01-32.065Z", "session id")
	assert.NoError(t, err, "Codebuff provider should accept freebuff: prefixed IDs")

	// Codebuff prefix should also be accepted (normal case).
	err = validateProviderSessionID(def, "codebuff:2026-07-15T20-01-32.065Z", "session id")
	assert.NoError(t, err, "Codebuff provider should accept codebuff: prefixed IDs")

	// Gemini prefix should be rejected.
	err = validateProviderSessionID(def, "gemini:sess-id", "session id")
	assert.Error(t, err, "Codebuff provider should reject gemini: prefixed IDs")
}
