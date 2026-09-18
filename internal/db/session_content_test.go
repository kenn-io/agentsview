package db

import (
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
	signals := SessionSignalUpdate{
		Outcome: "success", SecretLeakCount: 1,
		SecretsRulesVersion: "rulesv1",
	}
	findings := []SecretFinding{{
		SessionID: "s1", RuleName: "aws-access-key", Confidence: "definite",
		LocationKind: "message", MessageOrdinal: 0, MatchStart: 4, MatchEnd: 24,
		MatchIndex: 0, RedactedMatch: "AKIA…MPLE", RulesVersion: "rulesv1",
	}}
	require.NoError(t, d.ReplaceSessionContent(t.Context(), "s1", msgs, signals, findings))
	got, _ := d.GetAllMessages(t.Context(), "s1")
	require.Len(t, got, 1)
	f, _ := d.SessionSecretFindings(t.Context(), "s1")
	require.Len(t, f, 1)
	s, _ := d.GetSession(t.Context(), "s1")
	assert.Equal(t, 1, s.SecretLeakCount)
}
