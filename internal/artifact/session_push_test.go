package artifact

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"hegel.dev/go/hegel"
)

func TestSessionPushExactSelection(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"codex:chosen", "codex:chosen-child", "other"} {
		_, err := d.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
			Session:  db.Session{ID: id, Agent: "codex", Machine: "source", Project: "example", MessageCount: 1, UserMessageCount: 1},
			Messages: []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: id}}, ReplaceMessages: true,
		}})
		require.NoError(t, err)
	}
	data, _, err := EncodeSessionPush(t.Context(), d, "codex:chosen", "codex")
	require.NoError(t, err)
	assert.NotContains(t, string(data), "chosen-child")
	receipt, write, err := DecodeSessionPush(data)
	require.NoError(t, err)
	assert.Equal(t, "codex:chosen", receipt.SessionID)
	assert.Equal(t, receipt.SessionID, write.Session.ID)
	require.Len(t, write.Messages, 1)
	assert.Equal(t, "codex:chosen", write.Messages[0].Content)
	assert.Nil(t, write.Session.FilePath)
	_, _, err = EncodeSessionPush(t.Context(), d, "codex:cho", "codex")
	require.Error(t, err)
	_, _, err = EncodeSessionPush(t.Context(), d, "codex:chosen", "claude")
	require.Error(t, err)
}

func TestSessionPushRetryAfterRename(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "sample", Agent: "claude", Machine: "source"}))
	first, _, err := EncodeSessionPush(t.Context(), d, "sample", "")
	require.NoError(t, err)
	name := "renamed"
	require.NoError(t, d.RenameSession(t.Context(), "sample", &name))
	second, _, err := EncodeSessionPush(t.Context(), d, "sample", "")
	require.NoError(t, err)
	receipt1, _, err := DecodeSessionPush(first)
	require.NoError(t, err)
	receipt2, _, err := DecodeSessionPush(second)
	require.NoError(t, err)
	assert.Equal(t, receipt1.PayloadHash, receipt2.PayloadHash)
}

func TestSessionPushRetryAfterUsageChanges(t *testing.T) {
	session := db.Session{ID: "sample", Agent: "claude", Machine: "source", MessageCount: 1, UserMessageCount: 1}
	data := db.ArtifactExportData{Messages: []db.Message{{Ordinal: 0, Role: "user", Content: "hello"}}}
	first, err := encodeSessionPushData(t.Context(), "source", session, data)
	require.NoError(t, err)
	data.Messages[0].TokenUsage = []byte(`{"input_tokens":100}`)
	data.Messages[0].ContextTokens = 100
	data.Messages[0].OutputTokens = 20
	data.Messages[0].HasContextTokens = true
	data.Messages[0].HasOutputTokens = true
	data.Messages[0].Model = "updated-model"
	data.Messages[0].Timestamp = "2026-01-02T00:00:00Z"
	second, err := encodeSessionPushData(t.Context(), "source", session, data)
	require.NoError(t, err)
	receipt1, write1, err := DecodeSessionPush(first)
	require.NoError(t, err)
	receipt2, write2, err := DecodeSessionPush(second)
	require.NoError(t, err)
	assert.Equal(t, receipt1.PayloadHash, receipt2.PayloadHash)
	receiver := testDB(t)
	created, err := receiver.ApplySessionPush(t.Context(), receipt1, write1)
	require.NoError(t, err)
	require.True(t, created)
	created, err = receiver.ApplySessionPush(t.Context(), receipt2, write2)
	require.NoError(t, err)
	assert.False(t, created)
}

func TestSessionPushPreservesLauncherParent(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"parent", "child"} {
		require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: id, Agent: "claude", Machine: "source"}))
	}
	_, err := d.SetSessionExternalParent(t.Context(), "child", "parent")
	require.NoError(t, err)
	packet, _, err := EncodeSessionPush(t.Context(), d, "child", "")
	require.NoError(t, err)
	_, write, err := DecodeSessionPush(packet)
	require.NoError(t, err)
	require.NotNil(t, write.Session.ParentSessionID)
	assert.Equal(t, "parent", *write.Session.ParentSessionID)
}

func TestSessionPushRejectsFutureParserVersion(t *testing.T) {
	packet, err := encodeSessionPushData(t.Context(), "source", db.Session{ID: "sample", Agent: "claude", Machine: "source"}, db.ArtifactExportData{})
	require.NoError(t, err)
	var envelope SessionPushEnvelope
	require.NoError(t, json.Unmarshal(packet, &envelope))
	var m manifest
	require.NoError(t, json.Unmarshal(envelope.Manifest, &m))
	m.DataVersion = db.CurrentDataVersion() + 1
	envelope.Manifest, err = json.Marshal(m)
	require.NoError(t, err)
	packet, err = json.Marshal(envelope)
	require.NoError(t, err)
	_, _, err = DecodeSessionPush(packet)
	require.ErrorIs(t, err, ErrSessionPushFutureDataVersion)
}

func TestSessionPushReceiptIgnoresSegmentBoundaries(t *testing.T) {
	session := db.Session{ID: "sample", Agent: "claude", Machine: "source", MessageCount: 2, UserMessageCount: 1}
	data := db.ArtifactExportData{Messages: []db.Message{{Ordinal: 0, Role: "user", Content: "hello"}, {Ordinal: 1, Role: "assistant", Content: "world"}}}
	packet, err := encodeSessionPushData(t.Context(), "source", session, data)
	require.NoError(t, err)
	original, _, err := DecodeSessionPush(packet)
	require.NoError(t, err)
	var envelope SessionPushEnvelope
	require.NoError(t, json.Unmarshal(packet, &envelope))
	var segments []sessionPushSegment
	require.NoError(t, json.Unmarshal(envelope.Segments, &segments))
	require.Len(t, segments, 1)
	lines := strings.SplitAfter(segments[0].NDJSON, "\n")
	require.GreaterOrEqual(t, len(lines), 2)
	first, second := lines[0], strings.Join(lines[1:], "")
	segments = []sessionPushSegment{{SHA256: hashHex([]byte(first)), NDJSON: first}, {SHA256: hashHex([]byte(second)), NDJSON: second}}
	var m manifest
	require.NoError(t, json.Unmarshal(envelope.Manifest, &m))
	m.Segments = []string{segments[0].SHA256, segments[1].SHA256}
	envelope.Manifest, err = json.Marshal(m)
	require.NoError(t, err)
	envelope.Segments, err = json.Marshal(segments)
	require.NoError(t, err)
	packet, err = json.Marshal(envelope)
	require.NoError(t, err)
	resegmented, _, err := DecodeSessionPush(packet)
	require.NoError(t, err)
	assert.Equal(t, original.PayloadHash, resegmented.PayloadHash)
}

func TestSessionPushPreservesCapabilities(t *testing.T) {
	data, err := encodeSessionPushData(t.Context(), "source", db.Session{ID: "sample", Agent: "claude", Machine: "source", HasToolCalls: true, HasContextData: true}, db.ArtifactExportData{})
	require.NoError(t, err)
	_, write, err := DecodeSessionPush(data)
	require.NoError(t, err)
	assert.True(t, write.Session.HasToolCalls)
	assert.True(t, write.Session.HasContextData)
}

func TestSessionPushSparseOrdinalsAndToolResultCarrier(t *testing.T) {
	ordinal := 5
	session := db.Session{ID: "cortex:sample", Agent: "cortex", Machine: "source", MessageCount: 3, UserMessageCount: 1}
	data := db.ArtifactExportData{
		Messages: []db.Message{
			{Ordinal: 0, Role: "user", Content: "question"},
			{Ordinal: 3, Role: "user", SourceSubtype: "tool_result", Content: "tool output"},
			{Ordinal: 5, Role: "assistant", Content: "answer"},
		},
		UsageEvents: []db.UsageEvent{{SessionID: session.ID, MessageOrdinal: &ordinal, Source: "session", DedupKey: "turn"}},
	}
	packet, err := encodeSessionPushData(t.Context(), "source", session, data)
	require.NoError(t, err)
	_, write, err := DecodeSessionPush(packet)
	require.NoError(t, err)
	require.Len(t, write.Messages, 3)
	assert.Equal(t, []int{0, 3, 5}, []int{write.Messages[0].Ordinal, write.Messages[1].Ordinal, write.Messages[2].Ordinal})
	assert.Equal(t, "tool_result", write.Messages[1].SourceSubtype)
	assert.Equal(t, 1, write.Session.UserMessageCount)
	require.Len(t, write.UsageEvents, 1)
	require.NotNil(t, write.UsageEvents[0].MessageOrdinal)
	assert.Equal(t, 5, *write.UsageEvents[0].MessageOrdinal)
}

func TestSessionPushGooseAuthoritativeCounts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		count    int
		messages []db.Message
	}{
		{"retained carrier", 3, []db.Message{{Ordinal: 0, Role: "user", Content: "question"}, {Ordinal: 1, Role: "user", Content: "approval required"}, {Ordinal: 2, Role: "assistant", Content: "answer"}}},
		{"removed carrier", 3, []db.Message{{Ordinal: 0, Role: "user", Content: "question"}, {Ordinal: 2, Role: "assistant", Content: "answer"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := db.Session{ID: "goose:sample", Agent: "goose", Machine: "source", MessageCount: tc.count, UserMessageCount: 1}
			packet, err := encodeSessionPushData(t.Context(), "source", session, db.ArtifactExportData{Messages: tc.messages})
			require.NoError(t, err)
			_, write, err := DecodeSessionPush(packet)
			require.NoError(t, err)
			assert.Equal(t, 3, write.Session.MessageCount)
			assert.Equal(t, 1, write.Session.UserMessageCount)
			require.Len(t, write.Messages, len(tc.messages))
			assert.Equal(t, "answer", write.Messages[len(write.Messages)-1].Content)
		})
	}
}

func TestSessionPushPreservesUsageForFilteredCarrier(t *testing.T) {
	ordinal := 1
	session := db.Session{ID: "zcode:sample", Agent: "zcode", Machine: "source", MessageCount: 2, UserMessageCount: 1}
	data := db.ArtifactExportData{
		Messages:    []db.Message{{Ordinal: 0, Role: "user", Content: "question"}, {Ordinal: 2, Role: "assistant", Content: "answer"}},
		UsageEvents: []db.UsageEvent{{SessionID: session.ID, MessageOrdinal: &ordinal, Source: "session", DedupKey: "carrier", InputTokens: 42}},
	}
	packet, err := encodeSessionPushData(t.Context(), "source", session, data)
	require.NoError(t, err)
	_, write, err := DecodeSessionPush(packet)
	require.NoError(t, err)
	require.Len(t, write.UsageEvents, 1)
	require.NotNil(t, write.UsageEvents[0].MessageOrdinal)
	assert.Equal(t, 1, *write.UsageEvents[0].MessageOrdinal)
	assert.Equal(t, 42, write.UsageEvents[0].InputTokens)
}

func TestSessionPushCanceledContext(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{
		ID: "sample", Agent: "claude", Machine: "source",
	}))
	_, _, err := EncodeSessionPush(t.Context(), d, "sample", "")
	require.NoError(t, err, "the selected session must be encodable before cancellation")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err = EncodeSessionPush(ctx, d, "sample", "")
	require.ErrorIs(t, err, context.Canceled)
}

func TestSessionPushRejectMalformed(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "sample", Agent: "claude", Machine: "source"}))
	data, _, err := EncodeSessionPush(t.Context(), d, "sample", "")
	require.NoError(t, err)
	for _, mutate := range []func(map[string]any){
		func(v map[string]any) { v["v"] = 99 },
		func(v map[string]any) { v["session_id"] = "other" },
		func(v map[string]any) { v["segments"] = []any{} },
		func(v map[string]any) { v["extra"] = "unrequested" },
		func(v map[string]any) { v["segments"].([]any)[0].(map[string]any)["ndjson"] = "{}" },
	} {
		var value map[string]any
		require.NoError(t, json.Unmarshal(data, &value))
		mutate(value)
		bad, err := json.Marshal(value)
		require.NoError(t, err)
		_, _, err = DecodeSessionPush(bad)
		require.Error(t, err)
	}
	_, _, err = DecodeSessionPush([]byte(strings.Repeat(" ", int(MaxSessionPushBytes)+1)))
	require.Error(t, err)
}

func TestSessionPushUnicodeRoundTrip(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		content := hegel.Draw(ht, hegel.Text())
		// Bound materialization while drawing from the complete Unicode domain.
		if len(content) > 4096 {
			content = content[:4096]
			for !utf8.ValidString(content) {
				content = content[:len(content)-1]
			}
		}
		data, err := encodeSessionPushData(t.Context(), "source", db.Session{ID: "codex:sample", Agent: "codex", Machine: "source", MessageCount: 1, UserMessageCount: 1}, db.ArtifactExportData{Messages: []db.Message{{Ordinal: 0, Role: "user", Content: content}}})
		require.NoError(ht, err)
		_, write, err := DecodeSessionPush(data)
		require.NoError(ht, err)
		require.Len(ht, write.Messages, 1)
		assert.Equal(ht, content, write.Messages[0].Content)
	}, hegel.WithTestCases(2000))
}
