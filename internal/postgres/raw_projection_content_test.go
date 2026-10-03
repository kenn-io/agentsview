package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
)

func TestRawProjectionRejectsImageOffload(t *testing.T) {
	_, err := NewRawProjectionStore(nil, RawProjectionOptions{
		Content: ingest.ContentOptions{ToolResultImages: config.ToolResultImagesOffload},
	})
	require.ErrorContains(t, err, "tool_result_images = offload is not supported for hosted raw derivation")
}

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

func TestRawContentRevisionIgnoresDerivedFriction(t *testing.T) {
	p := ingest.PreparedSession{
		Session: db.Session{
			FrictionCount: 1, FrictionRulesVersion: "friction-v1",
			FrictionHash: "friction-hash",
		},
		Friction: db.SessionFrictionUpdate{
			RulesVersion: "friction-v1",
			Hash:         "friction-hash",
			Findings: []db.FrictionFinding{{
				SessionID: "captured-session", Kind: "error", Detector: "tool-error",
				Text: "command failed", Fingerprint: "finding-fingerprint",
			}},
		},
	}
	first, err := rawContentRevision(p)
	require.NoError(t, err)

	changedRows := p
	changedRows.Friction.Findings = append([]db.FrictionFinding(nil), p.Friction.Findings...)
	changedRows.Friction.Findings[0].Text = "different projected row text"
	rowsRevision, err := rawContentRevision(changedRows)
	require.NoError(t, err)
	assert.Equal(t, first, rowsRevision, "derived findings do not change source content")

	changed := p
	changed.Session.FrictionCount++
	changed.Session.FrictionRulesVersion = "updated-rules"
	changed.Session.FrictionHash = "updated-hash"
	second, err := rawContentRevision(changed)
	require.NoError(t, err)
	assert.Equal(t, first, second, "derived summaries do not change source content")

	changed = p
	changed.Friction.Findings = append([]db.FrictionFinding(nil), p.Friction.Findings...)
	changed.Friction.Findings[0].SessionID = "other-captured-session"
	third, err := rawContentRevision(changed)
	require.NoError(t, err)
	assert.Equal(t, first, third, "source session IDs are transport metadata")
}

func TestRawContentRevisionIgnoresFrictionSubjectIDs(t *testing.T) {
	messages := []db.Message{
		{Ordinal: 0, Role: "assistant", Content: "Here is the first answer."},
		{Ordinal: 1, Role: "user", Content: "no, you misunderstood the goal here"},
		{Ordinal: 2, Role: "assistant", Content: "Here is the revised answer."},
	}
	prepared := func(id string) ingest.PreparedSession {
		return ingest.PreparedSession{
			Session:  db.Session{ID: id, Agent: "codex", Machine: "hosted"},
			Messages: append([]db.Message(nil), messages...),
		}
	}
	first := prepared("captured-private-session-a")
	second := prepared("captured-private-session-b")

	require.NoError(t, ingest.RefreshFriction(&first, ingest.ContentOptions{}))
	require.NoError(t, ingest.RefreshFriction(&second, ingest.ContentOptions{}))
	require.Len(t, first.Friction.Findings, 1)
	require.Len(t, second.Friction.Findings, 1)
	assert.NotEqual(t, first.Friction.Findings[0].Fingerprint, second.Friction.Findings[0].Fingerprint)
	firstRevision, err := rawContentRevision(first)
	require.NoError(t, err)
	secondRevision, err := rawContentRevision(second)
	require.NoError(t, err)
	assert.Equal(t, firstRevision, secondRevision, "capture IDs do not change source content through derived findings")
}

func TestRawContentRevisionJSONRepresentation(t *testing.T) {
	p := ingest.PreparedSession{Messages: []db.Message{{
		Role:       "assistant",
		Content:    "<>&\u2028\u2029",
		TokenUsage: []byte(`{"input_tokens":9007199254740993,"output_tokens":0}`),
		ToolCalls:  []db.ToolCall{{InputJSON: `{"large":9007199254740993,"fraction":1.2500,"exponent":1e+03,"negative_zero":-0,"text":"<>&\u2028\u2029","duplicate":0,"duplicate":1,"null":null}`}},
	}}}
	got, err := rawContentRevision(p)
	require.NoError(t, err)
	// Recorded with the original normalized-content-v1 encoder. Existing
	// content identities must survive a change of JSON implementation.
	assert.Equal(t, "d5c710716bc7b8a68d60e43d58192489be71935625e141f9eb6c9dae723c3a29", got)
}
