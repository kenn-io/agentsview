package sync

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
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
	require.Error(t, err, "Codebuff provider should reject non-Codebuff/Freebuff agents")
	assert.Contains(t, err.Error(), "agent mismatch")
}

func TestValidateProviderSessionID_FreebuffPrefix(t *testing.T) {
	def := parser.AgentDef{
		Type:     parser.AgentCodebuff,
		IDPrefix: "codebuff:",
	}

	// Freebuff prefix should be accepted.
	err := validateProviderSessionID(def, "freebuff:2026-07-15T20-01-32.065Z", "session id")
	require.NoError(t, err, "Codebuff provider should accept freebuff: prefixed IDs")

	// Codebuff prefix should also be accepted (normal case).
	err = validateProviderSessionID(def, "codebuff:2026-07-15T20-01-32.065Z", "session id")
	require.NoError(t, err, "Codebuff provider should accept codebuff: prefixed IDs")

	// Gemini prefix should be rejected.
	err = validateProviderSessionID(def, "gemini:sess-id", "session id")
	assert.Error(t, err, "Codebuff provider should reject gemini: prefixed IDs")
}

func TestProviderResultAdmissionKeepsIdentityOfDroppedMembers(t *testing.T) {
	database := openTestDB(t)
	path := filepath.Join(t.TempDir(), "shared.db#member")
	size := int64(10)
	mtime := int64(5678)
	storedHash := "stored-hash"
	require.NoError(t, database.UpsertSession(t.Context(), db.Session{
		ID: "semantic:member", Agent: string(semanticTestAgent),
		Machine: "devbox", FilePath: &path, FileSize: &size,
		FileMtime: &mtime, FileHash: &storedHash,
	}))
	require.NoError(t, database.SetSessionDataVersion(t.Context(),
		"semantic:member", db.CurrentDataVersion(),
	))
	fingerprint := parser.SourceFingerprint{
		Key: path, Size: size, MTimeNS: mtime, Hash: storedHash,
	}
	newAdmission := func() *providerResultAdmission {
		return &providerResultAdmission{
			engine:      &Engine{db: database},
			ctx:         t.Context(),
			def:         parser.AgentDef{Type: semanticTestAgent, IDPrefix: "semantic:"},
			file:        parser.DiscoveredFile{Agent: semanticTestAgent, Path: path},
			source:      parser.SourceRef{Provider: semanticTestAgent, Key: path},
			fingerprint: fingerprint,
			policy:      parser.UnchangedResultMTimeAndHash,
		}
	}

	t.Run("dropped retry result keeps its identity", func(t *testing.T) {
		result := processFixtureResult(
			"semantic:member", semanticTestAgent, "semantic-project", path, fingerprint,
		)
		result.Session.File.Hash = storedHash
		admission := newAdmission()

		require.NoError(t, admission.admit(parser.ParseResultOutcome{
			Result: result, DataVersion: parser.DataVersionNeedsRetry,
		}))

		assert.Empty(t, admission.kept, "an unchanged member must not be held for writing")
		require.Len(t, admission.emitted, 1)
		assert.Equal(t, parser.ParseResult{Session: parser.ParsedSession{
			ID: "semantic:member", File: parser.FileInfo{Path: path},
		}}, admission.emitted[0])
		assert.Equal(t, []string{"semantic:member"}, admission.retryIDs)
		assert.Equal(t, storedHash, admission.firstHash)
		assert.NoError(t, admission.validationErr)
	})

	t.Run("wrong prefix fails validation", func(t *testing.T) {
		result := processFixtureResult(
			"other:member", semanticTestAgent, "semantic-project", path, fingerprint,
		)
		admission := newAdmission()

		err := admission.admit(parser.ParseResultOutcome{
			Result: result, DataVersion: parser.DataVersionCurrent,
		})

		require.Error(t, err)
		assert.Equal(t, err, admission.validationErr)
		assert.Empty(t, admission.emitted)
		assert.Empty(t, admission.kept)
	})
}
