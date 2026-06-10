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
