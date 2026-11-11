package artifact

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/assets"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"hegel.dev/go/hegel"
)

func TestSessionPushTimeout(t *testing.T) {
	assert.Equal(t, 2*time.Minute, SessionPushTimeout(0))
	assert.Equal(t, 2*time.Minute+time.Second, SessionPushTimeout(64<<10))
	assert.Equal(t, 19*time.Minute+4*time.Second, SessionPushTimeout(64<<20))
	assert.Equal(t, 19*time.Minute+4*time.Second, SessionPushTimeout(-1))
}

func TestSessionPushExactSelection(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"codex:chosen", "codex:parent", "codex:chosen-child", "other"} {
		session := db.Session{ID: id, Agent: "codex", Machine: "source", Project: "example", MessageCount: 1, UserMessageCount: 1}
		message := db.Message{SessionID: id, Ordinal: 0, Role: "user", Content: id}
		if id == "codex:chosen" {
			session.FilePath = new("/fixture/source.jsonl")
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
	_, _, err = EncodeSessionPush(t.Context(), d, "missing", "codex")
	require.ErrorContains(t, err, "not in the local archive")
	_, _, err = EncodeSessionPush(t.Context(), d, "codex:chosen", "claude")
	require.ErrorContains(t, err, "does not match --agent")

	remoteID, parentID := "host~codex:chosen", "host~codex:parent"
	_, err = d.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session:  db.Session{ID: remoteID, Agent: "codex", Machine: "host", ParentSessionID: &parentID, RelationshipType: "subagent", MessageCount: 1, UserMessageCount: 1},
		Messages: []db.Message{{SessionID: remoteID, Ordinal: 0, Role: "user", Content: "remote transcript"}}, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	data, _, err = EncodeSessionPush(t.Context(), d, remoteID, "codex")
	require.NoError(t, err)
	receipt, write, err = DecodeSessionPush(data)
	require.NoError(t, err)
	assert.Equal(t, remoteID, receipt.SessionID)
	require.Equal(t, &parentID, write.Session.ParentSessionID)

	t.Run("launcher parent", func(t *testing.T) {
		source, receiver := testDB(t), testDB(t)
		for _, id := range []string{"parent", "other-parent", "child"} {
			require.NoError(t, source.UpsertSession(t.Context(), db.Session{ID: id, Agent: "claude", Machine: "source"}))
		}
		_, err := source.SetSessionExternalParent(t.Context(), "child", "parent")
		require.NoError(t, err)
		push := func(id string) bool {
			packet, _, err := EncodeSessionPush(t.Context(), source, id, "")
			require.NoError(t, err)
			receipt, write, err := DecodeSessionPush(packet)
			require.NoError(t, err)
			if id == "child" {
				assert.Equal(t, new("parent"), receipt.ExternalParentSessionID)
			}
			created, err := receiver.ApplySessionPush(t.Context(), receipt, write)
			require.NoError(t, err)
			return created
		}
		assert.True(t, push("parent"))
		assert.True(t, push("other-parent"))
		assert.True(t, push("child"))
		link, err := receiver.GetSessionExternalParent(t.Context(), "child")
		require.NoError(t, err)
		assert.Equal(t, "parent", link.ParentSessionID)
		assert.True(t, link.Applied)
		children, err := receiver.GetChildSessions(t.Context(), "parent")
		require.NoError(t, err)
		require.Len(t, children, 1)
		assert.Equal(t, "child", children[0].ID)
		_, err = receiver.SetSessionExternalParent(t.Context(), "child", "other-parent")
		require.NoError(t, err)
		assert.False(t, push("child"))
		children, err = receiver.GetChildSessions(t.Context(), "parent")
		require.NoError(t, err)
		assert.Empty(t, children)
		children, err = receiver.GetChildSessions(t.Context(), "other-parent")
		require.NoError(t, err)
		require.Len(t, children, 1)
		assert.Equal(t, "child", children[0].ID)
		_, err = receiver.ClearSessionExternalParent(t.Context(), "child")
		require.NoError(t, err)
		assert.False(t, push("child"))
		child, err := receiver.GetSession(t.Context(), "child")
		require.NoError(t, err)
		assert.Nil(t, child.ParentSessionID)
		children, err = receiver.GetChildSessions(t.Context(), "other-parent")
		require.NoError(t, err)
		assert.Empty(t, children)
	})
	t.Run("transcript parent takes precedence", func(t *testing.T) {
		source, receiver := testDB(t), testDB(t)
		for _, id := range []string{"parser-parent", "launcher-parent"} {
			require.NoError(t, source.UpsertSession(t.Context(), db.Session{ID: id, Agent: "claude", Machine: "source"}))
		}
		require.NoError(t, source.UpsertSession(t.Context(), db.Session{ID: "child", Agent: "claude", Machine: "source", ParentSessionID: new("parser-parent"), RelationshipType: "subagent"}))
		link, err := source.SetSessionExternalParent(t.Context(), "child", "launcher-parent")
		require.NoError(t, err)
		assert.False(t, link.Applied)
		packet, _, err := EncodeSessionPush(t.Context(), source, "child", "")
		require.NoError(t, err)
		receipt, write, err := DecodeSessionPush(packet)
		require.NoError(t, err)
		assert.Equal(t, new("launcher-parent"), receipt.ExternalParentSessionID)
		assert.Equal(t, new("parser-parent"), write.Session.ParentSessionID)
		created, err := receiver.ApplySessionPush(t.Context(), receipt, write)
		require.NoError(t, err)
		assert.True(t, created)
		link, err = receiver.GetSessionExternalParent(t.Context(), "child")
		require.NoError(t, err)
		assert.Equal(t, "launcher-parent", link.ParentSessionID)
		assert.False(t, link.Applied)
		child, err := receiver.GetSession(t.Context(), "child")
		require.NoError(t, err)
		assert.Equal(t, new("parser-parent"), child.ParentSessionID)
		_, err = receiver.ClearSessionExternalParent(t.Context(), "child")
		require.NoError(t, err)
		child, err = receiver.GetSession(t.Context(), "child")
		require.NoError(t, err)
		assert.Equal(t, new("parser-parent"), child.ParentSessionID)
	})
	t.Run("pending launcher parent", func(t *testing.T) {
		source, receiver := testDB(t), testDB(t)
		require.NoError(t, source.UpsertSession(t.Context(), db.Session{ID: "child", Agent: "claude", Machine: "source"}))
		link, err := source.SetSessionExternalParent(t.Context(), "child", "parent")
		require.NoError(t, err)
		assert.False(t, link.Applied)
		push := func(id string) bool {
			packet, _, err := EncodeSessionPush(t.Context(), source, id, "")
			require.NoError(t, err)
			receipt, write, err := DecodeSessionPush(packet)
			require.NoError(t, err)
			if id == "child" {
				assert.Equal(t, new("parent"), receipt.ExternalParentSessionID)
			}
			created, err := receiver.ApplySessionPush(t.Context(), receipt, write)
			require.NoError(t, err)
			return created
		}
		assert.True(t, push("child"))
		require.NoError(t, source.UpsertSession(t.Context(), db.Session{ID: "parent", Agent: "claude", Machine: "source"}))
		assert.True(t, push("parent"))
		_, err = receiver.LinkSubagentSessionsContext(t.Context())
		require.NoError(t, err)
		child, err := receiver.GetSession(t.Context(), "child")
		require.NoError(t, err)
		require.NotNil(t, child.ParentSessionID)
		assert.Equal(t, "parent", *child.ParentSessionID)
		children, err := receiver.GetChildSessions(t.Context(), "parent")
		require.NoError(t, err)
		require.Len(t, children, 1)
		assert.Equal(t, "child", children[0].ID)
		link, err = receiver.GetSessionExternalParent(t.Context(), "child")
		require.NoError(t, err)
		assert.True(t, link.Applied)
		assert.False(t, push("child"))
	})
}

func TestSessionPushReceiptTracksTranscriptContent(t *testing.T) {
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
			data.Messages[0].Model = "corrected-model"
			data.Messages[0].ReasoningEffort = "corrected-effort"
			data.Messages[0].ProviderID = "corrected-provider"
			data.Messages[0].ClaudeMessageID = "corrected-message"
			data.Messages[0].ClaudeRequestID = "corrected-request"
			data.Messages[0].SourceUUID = "new-source-id"
			data.Messages[0].SourceType = "updated-source-type"
			data.Messages[0].SourceParentUUID = "new-parent-id"
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
				version := fmt.Sprintf(`"v":%d`, messageSegmentFormatVersion)
				require.Contains(t, segments[i].NDJSON, version)
				segments[i].NDJSON = strings.Replace(segments[i].NDJSON, version, `"v":1`, 1)
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
	const expected = `{"messages":[{"content":"answer","has_thinking":true,"has_tool_use":true,"is_compact_boundary":true,"is_system":true,"ordinal":0,"prompt_source":"prompt","role":"assistant","source_subtype":"notice","thinking_text":"thought","timestamp":"2026-01-01T00:00:00Z","tool_calls":[{"events":[{"agent":"agent","content":"result","id":"call","source":"tool","status":"done","subagent":"child","timestamp":"2026-01-01T00:00:01Z"}],"id":"call","input":"input","name":"Read","result":"result","subagent":"child"}]}],"v":1}` + "\n"
	assert.Equal(t, expected, string(canonical))
	packet, err := encodeSessionPushData(t.Context(), "source", db.Session{ID: "sample", Agent: "claude", Machine: "source", MessageCount: 1}, db.ArtifactExportData{Messages: []db.Message{message.dbMessage()}})
	require.NoError(t, err)
	receipt, _, err := DecodeSessionPush(packet)
	require.NoError(t, err)
	assert.Equal(t, "d544bb593031823a5b2175f36a2135f9619c000846b3289f15bfa8b5ea203bf0", receipt.PayloadHash)
	message.IsSidechain = true
	packet, err = encodeSessionPushData(t.Context(), "source", db.Session{ID: "sample", Agent: "claude", Machine: "source", MessageCount: 1}, db.ArtifactExportData{Messages: []db.Message{message.dbMessage()}})
	require.NoError(t, err)
	sidechainReceipt, _, err := DecodeSessionPush(packet)
	require.NoError(t, err)
	assert.NotEqual(t, receipt.PayloadHash, sidechainReceipt.PayloadHash)
	message.IsSidechain = false
	message.ToolCalls[0].ResultContent = `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	packet, err = encodeSessionPushData(t.Context(), "source", db.Session{ID: "sample", Agent: "claude", Machine: "source", MessageCount: 1}, db.ArtifactExportData{Messages: []db.Message{message.dbMessage()}})
	require.NoError(t, err)
	receipt, _, err = DecodeSessionPush(packet)
	require.NoError(t, err)
	assert.Equal(t, "e4411fff4c01f253d797d8b92db40d2b24f8bc49c224b23ed802e30b27754aec", receipt.PayloadHash)

	source := testDB(t)
	image := `[{"type":"input_image","image_url":"data:image/png;base64,AAEC"}]`
	dbMessage := db.Message{SessionID: "images", Ordinal: 0, Role: "assistant", Content: "answer", ToolCalls: []db.ToolCall{{ToolName: "Read", ToolUseID: "call", ResultContent: image, ResultEvents: []db.ToolResultEvent{{ToolUseID: "call", Source: "tool", Content: image}}}}}
	session := db.Session{ID: "images", Agent: "claude", Machine: "source", MessageCount: 1}
	_, err = source.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: session, Messages: []db.Message{dbMessage}, ReplaceMessages: true}})
	require.NoError(t, err)
	inline, _, err := EncodeSessionPush(t.Context(), source, session.ID, "")
	require.NoError(t, err)
	inlineReceipt, _, err := DecodeSessionPush(inline)
	require.NoError(t, err)
	dir := t.TempDir()
	source.SetAssetsDir(dir)
	_, err = source.MigrateToolImages(t.Context(), db.StripImagesFilter{}, func(mediaType string, body []byte) (string, bool, error) {
		return assets.Put(dir, mediaType, body)
	})
	require.NoError(t, err)
	migrated, _, err := EncodeSessionPush(t.Context(), source, session.ID, "")
	require.NoError(t, err)
	migratedReceipt, _, err := DecodeSessionPush(migrated)
	require.NoError(t, err)
	assert.Equal(t, inlineReceipt.PayloadHash, migratedReceipt.PayloadHash)
	for _, field := range []string{"summary", "event"} {
		t.Run(field, func(t *testing.T) {
			changed := dbMessage
			changed.ToolCalls = append([]db.ToolCall(nil), dbMessage.ToolCalls...)
			changed.ToolCalls[0].ResultEvents = append([]db.ToolResultEvent(nil), dbMessage.ToolCalls[0].ResultEvents...)
			if field == "summary" {
				changed.ToolCalls[0].ResultContent = strings.ReplaceAll(image, "AAEC", "AAED")
			} else {
				changed.ToolCalls[0].ResultEvents[0].Content = strings.ReplaceAll(image, "AAEC", "AAED")
			}
			_, err := source.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: session, Messages: []db.Message{changed}, ReplaceMessages: true}})
			require.NoError(t, err)
			packet, _, err := EncodeSessionPush(t.Context(), source, session.ID, "")
			require.NoError(t, err)
			changedReceipt, _, err := DecodeSessionPush(packet)
			require.NoError(t, err)
			assert.NotEqual(t, inlineReceipt.PayloadHash, changedReceipt.PayloadHash)
		})
	}
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
	t.Run("future envelope with new field", func(t *testing.T) {
		var value map[string]any
		require.NoError(t, json.Unmarshal(data, &value))
		value["v"] = 2
		value["compression"] = "new-format"
		packet, err := json.Marshal(value)
		require.NoError(t, err)
		_, _, err = DecodeSessionPush(packet)
		require.ErrorIs(t, err, ErrSessionPushFutureDataVersion)
	})
	for _, tc := range []struct {
		name, want string
		mutate     func(map[string]any)
	}{
		{"session", "global session ID is invalid", func(v map[string]any) { v["session_id"] = "other" }},
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
	t.Run("unknown manifest field", func(t *testing.T) {
		var envelope SessionPushEnvelope
		require.NoError(t, json.Unmarshal(data, &envelope))
		var fields map[string]any
		require.NoError(t, json.Unmarshal(envelope.Manifest, &fields))
		fields["future_optional_field"] = "value"
		envelope.Manifest, err = json.Marshal(fields)
		require.NoError(t, err)
		packet, err := json.Marshal(envelope)
		require.NoError(t, err)
		_, _, err = DecodeSessionPush(packet)
		require.NoError(t, err, "ignore unknown manifest fields like artifact import")
	})
	t.Run("future parser version", func(t *testing.T) {
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
	})
	for _, kind := range []string{"manifest", "segment"} {
		t.Run("future "+kind+" version", func(t *testing.T) {
			packet := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", MessageCount: 1}, []db.Message{{Ordinal: 0, Role: "user", Content: "fixture"}})
			var envelope SessionPushEnvelope
			require.NoError(t, json.Unmarshal(packet, &envelope))
			var m manifest
			require.NoError(t, json.Unmarshal(envelope.Manifest, &m))
			if kind == "manifest" {
				m.Version = manifestFormatVersion + 1
			} else {
				var segments []sessionPushSegment
				require.NoError(t, json.Unmarshal(envelope.Segments, &segments))
				require.Len(t, segments, 1)
				old := fmt.Sprintf(`"v":%d`, messageSegmentFormatVersion)
				require.Contains(t, segments[0].NDJSON, old)
				segments[0].NDJSON = strings.Replace(segments[0].NDJSON, old, fmt.Sprintf(`"v":%d`, messageSegmentFormatVersion+1), 1)
				segments[0].SHA256 = hashHex([]byte(segments[0].NDJSON))
				m.Segments[0] = segments[0].SHA256
				var err error
				envelope.Segments, err = json.Marshal(segments)
				require.NoError(t, err)
			}
			var err error
			envelope.Manifest, err = json.Marshal(m)
			require.NoError(t, err)
			packet, err = json.Marshal(envelope)
			require.NoError(t, err)
			_, _, err = DecodeSessionPush(packet)
			require.ErrorIs(t, err, ErrSessionPushFutureDataVersion)
		})
	}
	t.Run("invalid tool call coordinate", func(t *testing.T) {
		data := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", MessageCount: 1}, []db.Message{{Ordinal: 0, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultEvents: []db.ToolResultEvent{{EventIndex: 0, Content: "first"}, {EventIndex: 1, Content: "second"}}}}}})
		_, _, err := DecodeSessionPush(data)
		require.NoError(t, err, "the unmodified graph must be valid")
		var envelope SessionPushEnvelope
		require.NoError(t, json.Unmarshal(data, &envelope))
		var segments []sessionPushSegment
		require.NoError(t, json.Unmarshal(envelope.Segments, &segments))
		var record segmentMessage
		require.NoError(t, json.Unmarshal([]byte(segments[0].NDJSON), &record))
		record.ToolCalls[0].CallIndex = -1
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
		require.ErrorContains(t, err, "invalid tool call coordinate")
	})
}

func TestSessionPushRejectsUnresolvedAssetReference(t *testing.T) {
	content := "\x00" + `[{"byte_size":3,"image_ref":"asset://abc.png","media_type":"image/png","sha256":"abc","text":"![Image: image/png, 3 bytes](asset://abc.png)","type":"agentsview_image","version":1}]`
	packet := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", MessageCount: 1}, []db.Message{{Ordinal: 0, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: content, ResultEvents: []db.ToolResultEvent{{EventIndex: 0, Content: content}}}}}})
	_, _, err := DecodeSessionPush(packet)
	require.ErrorContains(t, err, "unresolved image assets")
	markdown := adversarialSessionPushPacket(t, db.Session{ID: "selected", Agent: "claude", Machine: "source", MessageCount: 1}, []db.Message{{Ordinal: 0, Role: "assistant", Content: "![image](asset:\x00//missing.png)"}})
	_, _, err = DecodeSessionPush(markdown)
	require.ErrorContains(t, err, "unresolved image assets")
}

func TestSessionPushUsageRoundTrip(t *testing.T) {
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
		text := db.SanitizeUTF8(hegel.Draw(ht, hegel.Text()))
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
	_, _, err := exportLoadedSessionToStore(t.Context(), sink, "source", &session, messages, nil, nil, productionArtifactLimits(), false)
	require.NoError(t, err)
	segments, err := json.Marshal(sink.segments)
	require.NoError(t, err)
	data, err := json.Marshal(SessionPushEnvelope{Version: 1, SessionID: session.ID, Manifest: sink.manifest, Segments: segments})
	require.NoError(t, err)
	return data
}

func TestSessionPushRetainedImages(t *testing.T) {
	source := testDB(t)
	sourceDir := t.TempDir()
	source.SetAssetsDir(sourceDir)
	source.SetToolResultImages(config.ToolResultImagesOffload)
	image := `[ {"type":"input_image","image_url":"data:image/png;base64,AAEC"} ]`
	ref, _, err := assets.Put(sourceDir, "image/png", []byte{0, 1, 2})
	require.NoError(t, err)
	session := db.Session{ID: "images", Agent: "claude", Machine: "source", MessageCount: 1}
	message := db.Message{SessionID: session.ID, Role: "assistant", Content: "![chart](" + ref + ")\n\n![other][pic]\n\n[pic]: " + ref + "\n\n`![example](asset://fake.png)`", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: image, ResultEvents: []db.ToolResultEvent{{Source: "tool", Content: "worker:\n" + image}}}}}
	_, err = source.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: session, Messages: []db.Message{message}, ReplaceMessages: true}})
	require.NoError(t, err)
	before, err := source.GetAllMessages(t.Context(), session.ID)
	require.NoError(t, err)
	packet, _, err := EncodeSessionPush(t.Context(), source, session.ID, "")
	require.NoError(t, err)
	receipt, write, err := DecodeSessionPush(packet)
	require.NoError(t, err)
	for _, policy := range []config.ToolResultImages{config.ToolResultImagesKeep, config.ToolResultImagesOffload, config.ToolResultImagesDrop} {
		t.Run(string(policy), func(t *testing.T) {
			receiver := testDB(t)
			dir := t.TempDir()
			receiver.SetAssetsDir(dir)
			receiver.SetToolResultImages(policy)
			created, err := receiver.ApplySessionPush(t.Context(), receipt, write)
			require.NoError(t, err)
			require.True(t, created)
			got, err := receiver.GetAllMessages(t.Context(), session.ID)
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Contains(t, got[0].Content, "![chart](data:image/png;base64,AAEC)")
			assert.Contains(t, got[0].Content, "[pic]: data:image/png;base64,AAEC")
			assert.Contains(t, got[0].Content, "`![example](asset://fake.png)`")
			images := db.MarkdownAssetImages(got[0].Content)
			require.Len(t, images, 2)
			assert.Equal(t, "ae4b3280e56e2faf83f414a6e3dabe9d5fbe18976544c05fed121accb85b53fc", images[0].SHA256)
			for _, content := range []string{got[0].ToolCalls[0].ResultContent, got[0].ToolCalls[0].ResultEvents[0].Content} {
				body, err := db.ReadRetainedImage(t.Context(), content, 0, dir, 100)
				require.NoError(t, err)
				if policy == config.ToolResultImagesDrop {
					assert.Empty(t, body)
				} else {
					assert.Equal(t, []byte{0, 1, 2}, body)
				}
			}
			created, err = receiver.ApplySessionPush(t.Context(), receipt, write)
			require.NoError(t, err)
			assert.False(t, created)
		})
	}
	after, err := source.GetAllMessages(t.Context(), session.ID)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	require.NoError(t, os.Remove(filepath.Join(sourceDir, strings.TrimPrefix(ref, "asset://"))))
	payload, _, err := EncodeSessionPush(t.Context(), source, session.ID, "")
	require.Error(t, err)
	assert.Nil(t, payload)
}
