package artifact

import (
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
	_, write, err := DecodeSessionPush(packet)
	require.NoError(t, err)
	assert.Equal(t, usage, write.UsageEvents, "keep native IDs, message ordinals and every accounting field")
	assert.Equal(t, plan, write.PlanUsage, "the root reference remains text, without origin prefix or fetching its session")
}

func TestSessionPushRichRoundTripProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text())
		// Model storage-valid text; distinct prefixes keep events separate.
		if len(text) > 4096 {
			text = string([]rune(text)[:min(1024, len([]rune(text)))])
		}
		toolText := db.SanitizeUTF8(text)
		session := db.Session{ID: "codex:selected", Agent: "codex", Machine: "source", MessageCount: 2, UserMessageCount: 1, HasToolCalls: true, HasContextData: true}
		messages := []db.Message{
			{Ordinal: 0, Role: "user", Content: text},
			{Ordinal: 1, Role: "assistant", Content: "assistant:" + text, ToolCalls: []db.ToolCall{{ToolName: "Read", ToolUseID: "call-1", InputJSON: `{"path":"fixture.txt"}`, ResultContent: "summary:" + toolText, ResultEvents: []db.ToolResultEvent{
				{EventIndex: 0, ToolUseID: "call-1", Source: "tool", Status: "started", Content: "first:" + toolText},
				{EventIndex: 1, ToolUseID: "call-1", Source: "tool", Status: "completed", Content: "second:" + toolText},
			}}}},
		}
		packet, err := encodeSessionPushData(t.Context(), "source", session, db.ArtifactExportData{Messages: messages})
		require.NoError(ht, err)
		receipt, write, err := DecodeSessionPush(packet)
		require.NoError(ht, err)
		assert.Equal(ht, session.ID, receipt.SessionID)
		require.Len(ht, write.Messages, 2)
		for i := range messages {
			assert.Equal(ht, i, write.Messages[i].Ordinal)
			assert.Equal(ht, messages[i].Content, write.Messages[i].Content)
		}
		require.Len(ht, write.Messages[1].ToolCalls, 1)
		call := write.Messages[1].ToolCalls[0]
		assert.Equal(ht, "call-1", call.ToolUseID)
		assert.JSONEq(ht, `{"path":"fixture.txt"}`, call.InputJSON)
		assert.Equal(ht, "summary:"+toolText, call.ResultContent)
		require.Len(ht, call.ResultEvents, 2)
		for i, event := range call.ResultEvents {
			assert.Equal(ht, i, event.EventIndex)
			assert.Equal(ht, messages[1].ToolCalls[0].ResultEvents[i].Content, event.Content)
		}
	}, hegel.WithTestCases(2000))
}
