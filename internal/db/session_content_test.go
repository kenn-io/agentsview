package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplaceSessionContentAtomic(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	msgs := []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user", Content: "key AKIA7QHWN2DKR4FYPLJM"},
	}
	signals := SessionSignalUpdate{Outcome: "success", SecretLeakCount: 1,
		SecretsRulesVersion: "rulesv1"}
	findings := []SecretFinding{{
		SessionID: "s1", RuleName: "aws-access-key", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0, MatchStart: 4, MatchEnd: 24,
		MatchIndex: 0, RedactedMatch: "AKIA…MPLE", RulesVersion: "rulesv1",
	}}
	require.NoError(t, d.ReplaceSessionContent("s1", msgs, signals, findings))
	got, _ := d.GetAllMessages(context.Background(), "s1")
	require.Len(t, got, 1)
	f, _ := d.SessionSecretFindings(context.Background(), "s1")
	require.Len(t, f, 1)
	s, _ := d.GetSession(context.Background(), "s1")
	assert.Equal(t, 1, s.SecretLeakCount)
}

func TestReplaceSessionContentCanonicalizesSecretFindings(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "canonical-content", "proj")

	zero := 0
	signals := SessionSignalUpdate{
		SecretLeakCount: 7, SecretsRulesVersion: "rules\x00-v2\x1b",
	}
	findings := []SecretFinding{{
		SessionID: "wrong-session", RulesVersion: "wrong-rules",
		RuleName: "api\x00-key", Confidence: "definite\x1b",
		LocationKind: "tool_result\x7f", MessageOrdinal: 3,
		CallIndex: &zero, EventIndex: &zero,
		MatchStart: 4, MatchEnd: 12, MatchIndex: 1,
		RedactedMatch: "tok\x00en",
	}}
	require.NoError(t, d.ReplaceSessionContent(
		"canonical-content", nil, signals, findings,
	))

	got, err := d.SessionSecretFindings(t.Context(), "canonical-content")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "canonical-content", got[0].SessionID)
	assert.Equal(t, "rules-v2", got[0].RulesVersion)
	assert.Equal(t, "api-key", got[0].RuleName)
	assert.Equal(t, "definite", got[0].Confidence)
	assert.Equal(t, "tool_result", got[0].LocationKind)
	assert.Equal(t, "token", got[0].RedactedMatch)
	require.NotNil(t, got[0].CallIndex)
	assert.Zero(t, *got[0].CallIndex)
	require.NotNil(t, got[0].EventIndex)
	assert.Zero(t, *got[0].EventIndex)

	session, err := d.GetSession(t.Context(), "canonical-content")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, 7, session.SecretLeakCount)
	assert.Equal(t, "rules-v2", session.SecretsRulesVersion)
}
