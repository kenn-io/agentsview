package artifact

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"hegel.dev/go/hegel"
)

func TestSessionPushExactSelection(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"codex:chosen", "codex:parent", "codex:chosen-child", "other"} {
		session := db.Session{ID: id, Agent: "codex", Machine: "source", Project: "example", MessageCount: 1, UserMessageCount: 1}
		message := db.Message{SessionID: id, Ordinal: 0, Role: "user", Content: id}
		if id == "codex:chosen" {
			session.ParentSessionID = new("codex:parent")
			session.RelationshipType = "subagent"
			message.ToolCalls = []db.ToolCall{{ToolName: "Agent", SubagentSessionID: "codex:chosen-child"}}
		}
		_, err := d.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
			Session: session, Messages: []db.Message{message}, ReplaceMessages: true,
		}})
		require.NoError(t, err)
	}
	data, _, err := EncodeSessionPush(t.Context(), d, "codex:chosen", "codex")
	require.NoError(t, err)
	receipt, write, err := DecodeSessionPush(data)
	require.NoError(t, err)
	assert.Equal(t, "codex:chosen", receipt.SessionID)
	assert.Equal(t, receipt.SessionID, write.Session.ID)
	require.Len(t, write.Messages, 1)
	assert.Equal(t, "codex:chosen", write.Messages[0].Content)
	require.Len(t, write.Messages[0].ToolCalls, 1)
	assert.Equal(t, "codex:chosen-child", write.Messages[0].ToolCalls[0].SubagentSessionID)
	require.NotNil(t, write.Session.ParentSessionID)
	assert.Equal(t, "codex:parent", *write.Session.ParentSessionID)
	assert.Nil(t, write.Session.FilePath)
	_, _, err = EncodeSessionPush(t.Context(), d, "codex:cho", "codex")
	require.Error(t, err)
	_, _, err = EncodeSessionPush(t.Context(), d, "codex:chosen", "claude")
	require.Error(t, err)
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
	for name, check := range map[string]func(*testing.T){
		"rename": func(t *testing.T) {
			t.Helper()
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
		},
		"usage and source metadata": func(t *testing.T) {
			t.Helper()
			session := db.Session{ID: "sample", Agent: "claude", Machine: "source", MessageCount: 1, UserMessageCount: 1}
			content := "[" + strings.Repeat("0,", 4096) + "0]"
			data := db.ArtifactExportData{Messages: []db.Message{{Ordinal: 0, Role: "user", Content: "hello", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content, Category: "Read", FilePath: "file", SkillName: "one"}}}}}
			first, err := encodeSessionPushData(t.Context(), "source", session, data)
			require.NoError(t, err)
			data.Messages[0].TokenUsage = []byte(`{"input_tokens":100}`)
			data.Messages[0].ContextTokens = 100
			data.Messages[0].OutputTokens = 20
			data.Messages[0].HasContextTokens = true
			data.Messages[0].HasOutputTokens = true
			data.Messages[0].SourceUUID = "new-source-id"
			data.Messages[0].SourceType = "updated-source-type"
			data.Messages[0].SourceParentUUID = "new-parent-id"
			data.Messages[0].IsSidechain = true
			data.Messages[0].ContentLength = 123
			data.Messages[0].ToolCalls[0].Category = "Other"
			data.Messages[0].ToolCalls[0].FilePath = "resolved-file"
			data.Messages[0].ToolCalls[0].SkillName = "two"
			second, err := encodeSessionPushData(t.Context(), "source", session, data)
			require.NoError(t, err)
			receipt1, _, err := DecodeSessionPush(first)
			require.NoError(t, err)
			receipt2, write, err := DecodeSessionPush(second)
			require.NoError(t, err)
			require.Len(t, write.Messages[0].ToolCalls, 1)
			assert.Equal(t, content, write.Messages[0].ToolCalls[0].ResultContent)
			assert.Equal(t, receipt1.PayloadHash, receipt2.PayloadHash)
		},
		"segments and wire version": func(t *testing.T) {
			t.Helper()
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
			for i := range segments {
				require.Contains(t, segments[i].NDJSON, `"v":4`)
				segments[i].NDJSON = strings.Replace(segments[i].NDJSON, `"v":4`, `"v":1`, 1)
				segments[i].SHA256 = hashHex([]byte(segments[i].NDJSON))
				m.Segments[i] = segments[i].SHA256
			}
			envelope.Manifest, err = json.Marshal(m)
			require.NoError(t, err)
			envelope.Segments, err = json.Marshal(segments)
			require.NoError(t, err)
			packet, err = json.Marshal(envelope)
			require.NoError(t, err)
			older, _, err := DecodeSessionPush(packet)
			require.NoError(t, err)
			assert.Equal(t, original.PayloadHash, older.PayloadHash)
		},
		"repeat": func(t *testing.T) {
			t.Helper()
			session := db.Session{ID: "sample", Agent: "claude", Machine: "source"}
			first, err := encodeSessionPushData(t.Context(), "source", session, db.ArtifactExportData{})
			require.NoError(t, err)
			second, err := encodeSessionPushData(t.Context(), "source", session, db.ArtifactExportData{})
			require.NoError(t, err)
			a, _, err := DecodeSessionPush(first)
			require.NoError(t, err)
			b, _, err := DecodeSessionPush(second)
			require.NoError(t, err)
			assert.Equal(t, a.PayloadHash, b.PayloadHash)
		},
	} {
		t.Run(name, check)
	}
}

func TestSessionPushIdentityV1(t *testing.T) {
	message := segmentMessage{Ordinal: 0, Role: "assistant", Content: "answer", ThinkingText: "thought", Timestamp: "2026-01-01T00:00:00Z", HasThinking: true, HasToolUse: true, Model: "model", ReasoningEffort: "high", ProviderID: "provider", ClaudeMessageID: "message", ClaudeRequestID: "request", IsSystem: true, SourceSubtype: "notice", PromptSource: "prompt", IsCompactBoundary: true, ToolCalls: []segmentToolCall{{ToolName: "Read", ToolUseID: "call", InputJSON: "input", ResultContent: "result", SubagentSessionID: "child", Category: "read", FilePath: "file", SkillName: "skill", ResultEvents: []segmentResultEvent{{ToolUseID: "call", AgentID: "agent", SubagentSessionID: "child", Source: "tool", Status: "done", Content: "result", Timestamp: "2026-01-01T00:00:01Z"}}}}}
	canonical, err := canonicalJSON(struct {
		Version  int                          `json:"v"`
		Messages []sessionPushIdentityMessage `json:"messages"`
	}{1, []sessionPushIdentityMessage{sessionPushIdentity(message)}})
	require.NoError(t, err)
	const expected = `{"messages":[{"claude_message_id":"message","claude_request_id":"request","content":"answer","has_thinking":true,"has_tool_use":true,"is_compact_boundary":true,"is_system":true,"model":"model","ordinal":0,"prompt_source":"prompt","provider_id":"provider","reasoning_effort":"high","role":"assistant","source_subtype":"notice","thinking_text":"thought","timestamp":"2026-01-01T00:00:00Z","tool_calls":[{"events":[{"agent":"agent","content":"result","id":"call","source":"tool","status":"done","subagent":"child","timestamp":"2026-01-01T00:00:01Z"}],"id":"call","input":"input","name":"Read","result":"result","subagent":"child"}]}],"v":1}` + "\n"
	assert.Equal(t, expected, string(canonical))
	assert.Equal(t, "97cd677a5770704bc32bd0fd2a85e8289beb0f00287cb7cc2ef4e748fdf67480", hashHex(canonical))
	packet, err := encodeSessionPushData(t.Context(), "source", db.Session{ID: "sample", Agent: "claude", Machine: "source", MessageCount: 1}, db.ArtifactExportData{Messages: []db.Message{message.dbMessage()}})
	require.NoError(t, err)
	receipt, _, err := DecodeSessionPush(packet)
	require.NoError(t, err)
	assert.Equal(t, "97cd677a5770704bc32bd0fd2a85e8289beb0f00287cb7cc2ef4e748fdf67480", receipt.PayloadHash)
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
	for name, check := range map[string]func(*testing.T){
		"sparse carrier": func(t *testing.T) {
			t.Helper()
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
		},
		"goose counts": func(t *testing.T) {
			t.Helper()
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
		},
		"filtered carrier usage": func(t *testing.T) {
			t.Helper()
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
		},
	} {
		t.Run(name, check)
	}
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
	for _, tc := range []struct {
		name, want string
		mutate     func(map[string]any)
	}{
		{"version", "unsupported version", func(v map[string]any) { v["v"] = 99 }},
		{"session", "invalid selected session", func(v map[string]any) { v["session_id"] = "other" }},
		{"segments", "segment closure mismatch", func(v map[string]any) { v["segments"] = []any{} }},
		{"unknown", "unknown", func(v map[string]any) { v["extra"] = "unrequested" }},
		{"hash", "segment hash mismatch", func(v map[string]any) { v["segments"].([]any)[0].(map[string]any)["ndjson"] = "{}" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value map[string]any
			require.NoError(t, json.Unmarshal(data, &value))
			tc.mutate(value)
			bad, err := json.Marshal(value)
			require.NoError(t, err)
			_, _, err = DecodeSessionPush(bad)
			require.ErrorContains(t, err, tc.want)
		})
	}
	for _, payload := range []string{`{"v":1,"v":1}`, `{} {}`} {
		_, _, err = DecodeSessionPush([]byte(payload))
		require.ErrorContains(t, err, "json")
	}
	ordinal := 0
	packet, err := encodeSessionPushData(t.Context(), "source", db.Session{ID: "sample", Agent: "claude", Machine: "source"}, db.ArtifactExportData{UsageEvents: []db.UsageEvent{{SessionID: "sample", MessageOrdinal: &ordinal, Source: "session"}}})
	require.NoError(t, err)
	var envelope SessionPushEnvelope
	require.NoError(t, json.Unmarshal(packet, &envelope))
	var wire manifest
	require.NoError(t, json.Unmarshal(envelope.Manifest, &wire))
	invalid := -1
	wire.UsageEvents[0].MessageOrdinal = &invalid
	envelope.Manifest, err = json.Marshal(wire)
	require.NoError(t, err)
	packet, err = json.Marshal(envelope)
	require.NoError(t, err)
	_, _, err = DecodeSessionPush(packet)
	require.ErrorContains(t, err, "usage has negative message ordinal")
	_, _, err = DecodeSessionPush([]byte(strings.Repeat(" ", int(MaxSessionPushBytes)+1)))
	require.ErrorContains(t, err, "body exceeds")
}

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

// Build through the existing artifact encoder, independently of push validation,
// so malformed-coordinate tests remain able to reach the receiver boundary.
func adversarialSessionPushPacket(t *testing.T, session db.Session, messages []db.Message) []byte {
	t.Helper()
	sink := &sessionPushCapture{}
	_, _, err := exportLoadedSessionToStore(t.Context(), sink, "source", &session, messages, nil, nil, productionArtifactLimits())
	require.NoError(t, err)
	segments, err := json.Marshal(sink.segments)
	require.NoError(t, err)
	data, err := json.Marshal(SessionPushEnvelope{Version: 1, SessionID: session.ID, Manifest: sink.manifest, Segments: segments})
	require.NoError(t, err)
	return data
}

func TestSessionPushAcceptsManyPRLinks(t *testing.T) {
	links := make([]db.PRLink, 4097)
	for i := range links {
		links[i] = db.PRLink{URL: "https://example.test/project/pull/1", Host: "example.test", Repository: "project", Number: 1}
	}
	valid := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", PRLinks: links[:256]}, nil)
	_, _, err := DecodeSessionPush(valid)
	require.NoError(t, err)
	var envelope SessionPushEnvelope
	require.NoError(t, json.Unmarshal(valid, &envelope))
	var fields map[string]any
	require.NoError(t, json.Unmarshal(envelope.Manifest, &fields))
	fields["future_optional_field"] = "value"
	envelope.Manifest, err = json.Marshal(fields)
	require.NoError(t, err)
	valid, err = json.Marshal(envelope)
	require.NoError(t, err)
	_, _, err = DecodeSessionPush(valid)
	require.NoError(t, err, "ignore unknown manifest fields like artifact import")
	data := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", PRLinks: links}, nil)
	_, _, err = DecodeSessionPush(data)
	require.NoError(t, err)
}

func TestSessionPushRejectsNestedCoordinates(t *testing.T) {
	for _, coordinate := range []string{"call", "event"} {
		t.Run(coordinate, func(t *testing.T) {
			data := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", MessageCount: 1}, []db.Message{{Ordinal: 0, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultEvents: []db.ToolResultEvent{{EventIndex: 0, Content: "first"}, {EventIndex: 1, Content: "second"}}}}}})
			_, _, err := DecodeSessionPush(data)
			require.NoError(t, err, "the unmodified graph must be valid")
			var envelope SessionPushEnvelope
			require.NoError(t, json.Unmarshal(data, &envelope))
			var segments []sessionPushSegment
			require.NoError(t, json.Unmarshal(envelope.Segments, &segments))
			var record segmentMessage
			require.NoError(t, json.Unmarshal([]byte(segments[0].NDJSON), &record))
			if coordinate == "call" {
				record.ToolCalls[0].CallIndex = -1
			} else {
				record.ToolCalls[0].ResultEvents[1].EventIndex = 0
			}
			body, err := canonicalJSON(record)
			require.NoError(t, err)
			segments[0].NDJSON, segments[0].SHA256 = string(body), hashHex(body)
			var m manifest
			require.NoError(t, json.Unmarshal(envelope.Manifest, &m))
			m.Segments[0] = segments[0].SHA256
			envelope.Manifest, err = json.Marshal(m)
			require.NoError(t, err)
			envelope.Segments, err = json.Marshal(segments)
			require.NoError(t, err)
			data, err = json.Marshal(envelope)
			require.NoError(t, err)
			_, _, err = DecodeSessionPush(data)
			require.Error(t, err)
		})
	}
}
