package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

func TestRawTranscriptPrefixRequiresCompleteRowAgreement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*ingest.PreparedSession)
		matches bool
	}{
		{"append", func(p *ingest.PreparedSession) { p.Session.MessageCount = 2; p.Session.EndedAt = new("later") }, true},
		{"changed message", func(p *ingest.PreparedSession) { p.Messages[0].Content = "different" }, false},
		{"changed usage", func(p *ingest.PreparedSession) { p.Messages[0].OutputTokens = 3; p.Messages[0].HasOutputTokens = true }, false},
		{"changed title", func(p *ingest.PreparedSession) { p.Session.SessionName = new("renamed") }, false},
		{"changed tool", func(p *ingest.PreparedSession) { p.Messages[0].ToolCalls[0].InputJSON = `{"command":"false"}` }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			short := ingest.PreparedSession{Session: db.Session{MessageCount: 1}, Messages: []db.Message{{Ordinal: 0, Content: "original", ToolCalls: []db.ToolCall{{InputJSON: `{"command":"true"}`}}}}}
			encoded, err := encodeRawPayload(short)
			require.NoError(t, err)
			long, err := decodeRawPayload(encoded)
			require.NoError(t, err)
			long.Messages = append(long.Messages, db.Message{Ordinal: 1, Content: "appended"})
			tc.change(&long)
			matches, err := rawTranscriptPrefix(short, long)
			require.NoError(t, err)
			assert.Equal(t, tc.matches, matches)
		})
	}
}
