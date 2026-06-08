package server

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestSessionBatchWriteFromParsedPreservesDisplayName(t *testing.T) {
	sess := parser.ParsedSession{
		ID:          "test-session",
		DisplayName: "My Renamed Session",
	}
	result := sessionBatchWriteFromParsed(sess, nil)
	require.NotNil(t, result.Session.DisplayName,
		"DisplayName must be persisted on upload")
	require.Equal(t, "My Renamed Session", *result.Session.DisplayName)
	require.NotNil(t, result.Session.NameSource)
	require.Equal(t, "agent", *result.Session.NameSource)
}

func TestSessionBatchWriteFromParsedNoDisplayName(t *testing.T) {
	sess := parser.ParsedSession{
		ID: "test-session-no-name",
	}
	result := sessionBatchWriteFromParsed(sess, nil)
	require.Nil(t, result.Session.DisplayName,
		"DisplayName must be nil when not set")
	require.Nil(t, result.Session.NameSource,
		"NameSource must be nil when not set")
}
