package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestSessionBatchWriteFromParsedPreservesSessionName(t *testing.T) {
	sess := parser.ParsedSession{
		ID:          "test-session",
		SessionName: "My Renamed Session",
	}
	result := sessionBatchWriteFromParsed(sess, nil)
	require.NotNil(t, result.Session.SessionName,
		"SessionName must be persisted on upload")
	require.Equal(t, "My Renamed Session", *result.Session.SessionName)
	// DisplayName must NOT be set by the converter — only RenameSession sets it.
	assert.Nil(t, result.Session.DisplayName,
		"converter must not set DisplayName")
}

func TestSessionBatchWriteFromParsedNoSessionName(t *testing.T) {
	sess := parser.ParsedSession{
		ID: "test-session-no-name",
	}
	result := sessionBatchWriteFromParsed(sess, nil)
	require.Nil(t, result.Session.SessionName,
		"SessionName must be nil when not set")
	assert.Nil(t, result.Session.DisplayName,
		"DisplayName must be nil when not set")
}

func TestSessionBatchWriteFromParsedPreservesSessionIdentity(t *testing.T) {
	sess := parser.ParsedSession{
		ID:         "test-session-identity",
		Agent:      parser.AgentClaude,
		AgentLabel: "Claude Code",
		Entrypoint: "claude-sdk",
	}

	result := sessionBatchWriteFromParsed(sess, nil)

	assert.Equal(t, "claude", result.Session.Agent)
	assert.Equal(t, "Claude Code", result.Session.AgentLabel)
	assert.Equal(t, "claude-sdk", result.Session.Entrypoint)
}

func TestSessionBatchWriteFromParsedPreservesClaudeProvenance(t *testing.T) {
	sess := parser.ParsedSession{
		ID:          "test-claude-provenance",
		Agent:       parser.AgentClaude,
		SessionKind: "bg",
	}
	msgs := []parser.ParsedMessage{{
		Ordinal:      1,
		Role:         parser.RoleUser,
		Content:      "queued prompt",
		PromptSource: "queued",
	}}

	result := sessionBatchWriteFromParsed(sess, msgs)

	assert.Equal(t, "bg", result.Session.SessionKind)
	require.Len(t, result.Messages, 1)
	assert.Equal(t, "queued", result.Messages[0].PromptSource)
}

func TestSessionBatchWriteFromParsedPreservesMessageIdentity(t *testing.T) {
	sess := parser.ParsedSession{ID: "test-message-identity"}
	msgs := []parser.ParsedMessage{{
		Ordinal:          1,
		Role:             parser.RoleUser,
		Content:          "hidden context",
		IsSystem:         true,
		SourceType:       "system",
		SourceSubtype:    "ide_opened_file",
		SourceUUID:       "entry-1:ide-context",
		SourceParentUUID: "parent-1",
	}}

	result := sessionBatchWriteFromParsed(sess, msgs)

	require.Len(t, result.Messages, 1)
	assert.True(t, result.Messages[0].IsSystem)
	assert.Equal(t, "system", result.Messages[0].SourceType)
	assert.Equal(t, "ide_opened_file", result.Messages[0].SourceSubtype)
	assert.Equal(t, "entry-1:ide-context", result.Messages[0].SourceUUID)
	assert.Equal(t, "parent-1", result.Messages[0].SourceParentUUID)
	assert.True(t, result.PreserveLegacyPinsByOrdinal,
		"explicit re-uploads preserve existing UUID-less pins by ordinal")
}
