package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

func TestRawContentRevisionRetainsHiddenSemanticFields(t *testing.T) {
	original := ingest.PreparedSession{Session: db.Session{ID: "physical", Machine: "device", SessionName: new("provider title")}, Messages: []db.Message{{Ordinal: 0, Role: "assistant", Content: "same", ToolCalls: []db.ToolCall{{ToolName: "Bash", InputJSON: `{"a":1,"b":2}`, ResultEvents: []db.ToolResultEvent{{Content: "same", RawContentDigest: []byte("transport")}}}}}}}
	digest, err := rawContentRevision(original)
	require.NoError(t, err)
	clone := func() ingest.PreparedSession {
		b, err := encodeRawPayload(original)
		require.NoError(t, err)
		p, err := decodeRawPayload(b)
		require.NoError(t, err)
		return p
	}
	t.Run("transport", func(t *testing.T) {
		p := clone()
		p.Session.ID = "other"
		p.Session.Machine = "other"
		p.Session.FilePath = new("capture.jsonl")
		p.Session.FileMtime = new(int64(99))
		p.Messages[0].ToolCalls[0].ResultEvents[0].RawContentDigest = []byte("other transport")
		p.Messages[0].ToolCalls[0].InputJSON = `{ "b": 2, "a": 1 }`
		got, err := rawContentRevision(p)
		require.NoError(t, err)
		assert.Equal(t, digest, got)
	})
	t.Run("derivation version metadata", func(t *testing.T) {
		p := clone()
		p.Session.DataVersion = 123
		p.Signals.SecretsRulesVersion = "next-rules"
		p.Signals.QualitySignals.Version = 123
		got, err := rawContentRevision(p)
		require.NoError(t, err)
		assert.Equal(t, digest, got)
	})
	t.Run("hidden provider title", func(t *testing.T) {
		p := clone()
		p.Session.SessionName = new("changed")
		got, err := rawContentRevision(p)
		require.NoError(t, err)
		assert.NotEqual(t, digest, got)
	})
	t.Run("explicit zero usage presence", func(t *testing.T) {
		p := clone()
		p.Messages[0].HasOutputTokens = true
		got, err := rawContentRevision(p)
		require.NoError(t, err)
		assert.NotEqual(t, digest, got)
	})
	t.Run("semantic tool path", func(t *testing.T) {
		p := clone()
		p.Messages[0].ToolCalls[0].FilePath = "src/changed.go"
		got, err := rawContentRevision(p)
		require.NoError(t, err)
		assert.NotEqual(t, digest, got)
	})
}

func TestRawContentRevisionIgnoresOnlyRecencyDerivedState(t *testing.T) {
	p := ingest.PreparedSession{Session: db.Session{EndedAt: new("2026-09-11T12:00:00Z"), MessageCount: 3}, Messages: []db.Message{{Ordinal: 0, Role: "user", Content: "task"}, {Ordinal: 1, Role: "assistant", Content: "working"}, {Ordinal: 2, Role: "user", Content: "continue"}}}
	p.Signals.Outcome, p.Signals.OutcomeConfidence = "unknown", "low"
	p.Signals.SignalsPendingSince = new("2026-09-11T12:01:00Z")
	copyRawSignalFields(&p.Session, p.Signals)
	first, err := rawContentRevision(p)
	require.NoError(t, err)
	p.Signals.SignalsPendingSince = new("2026-09-11T12:02:00Z")
	copyRawSignalFields(&p.Session, p.Signals)
	second, err := rawContentRevision(p)
	require.NoError(t, err)
	assert.Equal(t, first, second, "equal content at a second recent instant")
	p.Signals.SignalsPendingSince = nil
	p.Signals.Outcome, p.Signals.OutcomeConfidence = "abandoned", "medium"
	p.Signals.HealthScore, p.Signals.HealthGrade = new(85), new("B")
	copyRawSignalFields(&p.Session, p.Signals)
	settled, err := rawContentRevision(p)
	require.NoError(t, err)
	assert.Equal(t, first, settled, "recency expiry is not new content")
	p.Signals.ToolRetryCount++
	stableSignal, err := rawContentRevision(p)
	require.NoError(t, err)
	assert.NotEqual(t, settled, stableSignal, "stable derived signals remain content")
	p.Messages[2].Content = "different task"
	semantic, err := rawContentRevision(p)
	require.NoError(t, err)
	assert.NotEqual(t, stableSignal, semantic, "actual transcript semantics remain content")
}
