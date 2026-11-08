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
	data, err := EncodeSessionPush(t.Context(), d, "codex:chosen", "codex")
	require.NoError(t, err)
	assert.NotContains(t, string(data), "chosen-child")
	receipt, write, err := DecodeSessionPush(data)
	require.NoError(t, err)
	assert.Equal(t, "codex:chosen", receipt.SessionID)
	assert.Equal(t, receipt.SessionID, write.Session.ID)
	require.Len(t, write.Messages, 1)
	assert.Equal(t, "codex:chosen", write.Messages[0].Content)
	assert.Nil(t, write.Session.FilePath)
	_, err = EncodeSessionPush(t.Context(), d, "codex:cho", "codex")
	require.Error(t, err)
	_, err = EncodeSessionPush(t.Context(), d, "codex:chosen", "claude")
	require.Error(t, err)
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

func TestSessionPushCanceledContext(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{
		ID: "sample", Agent: "claude", Machine: "source",
	}))
	_, err := EncodeSessionPush(t.Context(), d, "sample", "")
	require.NoError(t, err, "the selected session must be encodable before cancellation")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = EncodeSessionPush(ctx, d, "sample", "")
	require.ErrorIs(t, err, context.Canceled)
}

func TestSessionPushRejectMalformed(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "sample", Agent: "claude", Machine: "source"}))
	data, err := EncodeSessionPush(t.Context(), d, "sample", "")
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
