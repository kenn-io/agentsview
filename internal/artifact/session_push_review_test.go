package artifact

import (
	"encoding/json/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"hegel.dev/go/hegel"
)

func TestSessionPushNormalizedAssetReferenceDowngrade(t *testing.T) {
	content := "\x00" + `[{"byte_size":3,"image_ref":"asset://abc.png","media_type":"image/png","sha256":"abc","text":"![Image: image/png, 3 bytes](asset://abc.png)","type":"agentsview_image","version":1}]`
	packet := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", MessageCount: 1}, []db.Message{{Ordinal: 0, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content, ResultEvents: []db.ToolResultEvent{{EventIndex: 0, Content: content}}}}}})
	_, write, err := DecodeSessionPush(packet)
	require.NoError(t, err)
	require.Len(t, write.Messages, 1)
	require.Len(t, write.Messages[0].ToolCalls, 1)
	call := write.Messages[0].ToolCalls[0]
	assert.NotContains(t, call.ResultContent, "asset://")
	assert.NotContains(t, call.ResultEvents[0].Content, "asset://")
	assert.NotContains(t, call.ResultContent, "\x00")
}

func TestSessionPushUsageRoundTripAndReceipt(t *testing.T) {
	session := db.Session{ID: "codex:selected", Agent: "codex", Machine: "source", MessageCount: 1, UserMessageCount: 1}
	ordinal, weekly, credits, cost := 0, 12.5, "3.25", money.Money{Microdollars: 12345}
	usage := []db.UsageEvent{{SessionID: session.ID, MessageOrdinal: &ordinal, Source: "session", Model: "fixture-model", ProviderID: "fixture-provider", InputTokens: 100, OutputTokens: 20, CacheCreationInputTokens: 3, CacheReadInputTokens: 4, ReasoningTokens: 5, Cost: &cost, CostStatus: "available", CostSource: "provider", OccurredAt: "2026-01-01T00:00:00Z", DedupKey: "turn-1"}}
	plan := &db.SessionPlanUsage{HasPlanUsage: true, DataAsOf: "2026-01-01T00:01:00Z", DataStatus: "available", UsageSource: "fixture", WeeklyPercent: &weekly, Credits: &credits, Groups: []db.SessionPlanUsageGroup{{Model: "fixture-model", WeeklyPercent: &weekly, Credits: &credits}}, Final: true, OutcomeKind: "counted_under", OutcomeAt: "2026-01-01T00:02:00Z", CountedUnder: "codex:historical", HasCoverage: true, LastCaptureAt: "2026-01-01T00:00:00Z", UncapturedTurns: 2}
	data := db.ArtifactExportData{Messages: []db.Message{{Ordinal: 0, Role: "user", Content: "fixture"}}, UsageEvents: usage, PlanUsage: plan}
	packet, err := encodeSessionPushData(t.Context(), "source", session, data)
	require.NoError(t, err)
	receipt, write, err := DecodeSessionPush(packet)
	require.NoError(t, err)
	assert.Equal(t, usage, write.UsageEvents, "keep native IDs, message ordinals and every accounting field")
	assert.Equal(t, plan, write.PlanUsage, "the root reference remains text, without origin prefix or fetching its session")
	repeated, err := encodeSessionPushData(t.Context(), "source", session, data)
	require.NoError(t, err)
	second, _, err := DecodeSessionPush(repeated)
	require.NoError(t, err)
	assert.Equal(t, receipt, second, "unchanged usage-bearing snapshots have stable receipt hashes")

	// Mutate the wire manifest directly: the encoder must not reject the test
	// input before the receiver's closure guard gets to inspect it.
	var envelope SessionPushEnvelope
	require.NoError(t, json.Unmarshal(packet, &envelope))
	var wire manifest
	require.NoError(t, json.Unmarshal(envelope.Manifest, &wire))
	for _, invalid := range []int{-1} {
		t.Run("ordinal="+strconv.Itoa(invalid), func(t *testing.T) {
			wire.UsageEvents[0].MessageOrdinal = &invalid
			envelope.Manifest, err = json.Marshal(wire)
			require.NoError(t, err)
			bad, err := json.Marshal(envelope)
			require.NoError(t, err)
			receipt, write, err := DecodeSessionPush(bad)
			require.ErrorContains(t, err, "usage has negative message ordinal")
			assert.Empty(t, receipt.SessionID)
			assert.Empty(t, write.Session.ID)
		})
	}
}

func TestSessionPushRichRoundTripProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := db.SanitizeUTF8(hegel.Draw(ht, hegel.Text()))
		// Model storage-valid text; distinct prefixes keep events separate.
		if len(text) > 4096 {
			text = string([]rune(text)[:min(1024, len([]rune(text)))])
		}
		session := db.Session{ID: "codex:selected", Agent: "codex", Machine: "source", MessageCount: 2, UserMessageCount: 1, HasToolCalls: true, HasContextData: true}
		messages := []db.Message{
			{Ordinal: 0, Role: "user", Content: "user:" + text},
			{Ordinal: 1, Role: "assistant", Content: "assistant:" + text, ToolCalls: []db.ToolCall{{ToolName: "Read", ToolUseID: "call-1", InputJSON: `{"path":"fixture.txt"}`, ResultContent: "summary:" + text, ResultEvents: []db.ToolResultEvent{
				{EventIndex: 0, ToolUseID: "call-1", Source: "tool", Status: "started", Content: "first:" + text},
				{EventIndex: 1, ToolUseID: "call-1", Source: "tool", Status: "completed", Content: "second:" + text},
			}}}},
		}
		packet, err := encodeSessionPushData(t.Context(), "source", session, db.ArtifactExportData{Messages: messages})
		require.NoError(ht, err)
		receipt, write, err := DecodeSessionPush(packet)
		require.NoError(ht, err)
		assert.Equal(ht, session.ID, receipt.SessionID)
		assert.True(ht, write.Session.HasToolCalls)
		assert.True(ht, write.Session.HasContextData)
		require.Len(ht, write.Messages, 2)
		for i := range messages {
			assert.Equal(ht, i, write.Messages[i].Ordinal)
			assert.Equal(ht, messages[i].Content, write.Messages[i].Content)
		}
		require.Len(ht, write.Messages[1].ToolCalls, 1)
		call := write.Messages[1].ToolCalls[0]
		assert.Equal(ht, "call-1", call.ToolUseID)
		assert.JSONEq(ht, `{"path":"fixture.txt"}`, call.InputJSON)
		assert.Equal(ht, "summary:"+text, call.ResultContent)
		require.Len(ht, call.ResultEvents, 2)
		for i, event := range call.ResultEvents {
			assert.Equal(ht, i, event.EventIndex)
			assert.Equal(ht, messages[1].ToolCalls[0].ResultEvents[i].Content, event.Content)
			assert.True(ht, strings.HasSuffix(event.Content, text))
		}
	}, hegel.WithTestCases(2000))
}
