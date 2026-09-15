package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBunStoreListSecretFindingsHydratesCanonicalRows(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "bun-secret", "alpha", func(session *Session) {
		session.Agent = "claude"
	})
	require.NoError(t, database.ReplaceSessionMessages("bun-secret", []Message{{
		SessionID: "bun-secret", Ordinal: 0, Role: "user",
		Content: "contains a secret", Timestamp: "2026-05-20T12:00:00Z",
	}}))
	require.NoError(t, database.ReplaceSessionSecretFindings(
		"bun-secret",
		[]SecretFinding{{
			RuleName: "token", Confidence: "definite", LocationKind: "message",
			MessageOrdinal: 0, MatchStart: 11, MatchEnd: 17,
			RedactedMatch: "se…et",
		}},
		1, "rules-v1",
	))

	page, err := database.ListSecretFindings(t.Context(), SecretFindingFilter{
		Project: "alpha", Agent: "claude", RulesVersions: []string{"rules-v1"},
		Limit: 10,
	})

	require.NoError(t, err)
	require.Len(t, page.Findings, 1)
	assert.Equal(t, SecretFindingRow{
		SecretFinding: SecretFinding{
			SessionID: "bun-secret", RuleName: "token", Confidence: "definite",
			LocationKind: "message", MessageOrdinal: 0,
			MatchStart: 11, MatchEnd: 17, RedactedMatch: "se…et",
			RulesVersion: "rules-v1",
		},
		Project: "alpha", Agent: "claude",
	}, page.Findings[0])
}

func TestBunStoreSecretFindingSourceUsesExactEventCoordinates(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "bun-source", "alpha")
	require.NoError(t, database.ReplaceSessionMessages("bun-source", []Message{{
		SessionID: "bun-source", Ordinal: 3, Role: "assistant",
		Content: "assistant message", Timestamp: "2026-05-20T12:00:00Z",
		ToolCalls: []ToolCall{
			{ToolUseID: "first", ResultEvents: []ToolResultEvent{{
				Content: "wrong event",
			}}},
			{ToolUseID: "second", ResultEvents: []ToolResultEvent{{
				Content: "target event secret",
			}}},
		},
	}}))

	source, ok, err := database.SecretFindingSource(t.Context(), SecretFinding{
		SessionID: "bun-source", LocationKind: "tool_result_event",
		MessageOrdinal: 3, CallIndex: Ptr(1), EventIndex: Ptr(0),
	})

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "target event secret", source)
}
