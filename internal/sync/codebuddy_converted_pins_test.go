package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A reply pinned while its conversation was read from the 4.x directory
// keeps its pin and note once IDE 5.x converts the conversation, although
// the converted reply also shows its reasoning or tool calls. A reply whose
// time it shares with another reply cannot be told apart and loses its pin,
// as before.
func TestSyncCodeBuddyConversionKeepsPinnedReply(t *testing.T) {
	const (
		prompt   = `{"role":"user","createdAt":"2026-01-01T00:00:01Z","message":{"content":[{"type":"text","text":"old prompt"}]}}`
		cbcMeta  = `{"type":"session-meta","id":"m","sessionId":"conv1","meta":{"codebuddy.ai/converterVersion":"ide-main-craft-jsonl/29"}}`
		cbcUser  = `{"id":"u1","type":"message","role":"user","timestamp":1767225601000,"content":[{"type":"input_text","text":"old prompt"}]}`
		cbcLater = `{"id":"u2","parentId":"a1","type":"message","role":"user","timestamp":1767225700000,"content":[{"type":"input_text","text":"continued in 5.x"}]}`
	)
	for _, tc := range []struct {
		name     string
		legacy   map[string]string
		order    []string
		pinned   string
		cbc      []string
		wantUUID string
		wantPin  bool
	}{
		{
			name: "reasoning with usage",
			legacy: map[string]string{
				"u1": prompt,
				"a1": `{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","extra":{"requestId":"req1","modelId":"glm-x","statsSnapshot":{"inputTokens":300,"outputTokens":40}},"message":{"content":[{"type":"reasoning","text":"check the files"},{"type":"text","text":"old reply"}]}}`,
			},
			order:  []string{"u1", "a1"},
			pinned: "old reply",
			cbc: []string{cbcMeta, cbcUser,
				`{"id":"a1:reasoning","parentId":"u1","type":"reasoning","timestamp":1767225602000,"rawContent":[{"type":"reasoning_text","text":"check the files"}]}`,
				`{"id":"a1","parentId":"a1:reasoning","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"old reply"}]}`,
				cbcLater,
			},
			wantUUID: "cb4:req1:a1",
			wantPin:  true,
		},
		{
			name: "tool call without usage",
			legacy: map[string]string{
				"u1": prompt,
				"a1": `{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","message":{"content":[{"type":"text","text":"let me look"},{"type":"tool-call","toolCallId":"c1","toolName":"Bash","args":{"command":"ls"}}]}}`,
				"t1": `{"role":"tool","createdAt":"2026-01-01T00:00:03Z","message":{"content":[{"type":"tool-result","toolCallId":"c1","result":{"result":{"stdout":"a.go"}}}]}}`,
			},
			order:  []string{"u1", "a1", "t1"},
			pinned: "let me look",
			cbc: []string{cbcMeta, cbcUser,
				`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"let me look"}]}`,
				`{"id":"a1:call:0","parentId":"a1","type":"function_call","timestamp":1767225602000,"name":"Bash","callId":"c1","arguments":{"command":"ls"}}`,
				`{"id":"t1","parentId":"a1:call:0","type":"function_call_result","timestamp":1767225603000,"name":"Bash","callId":"c1","status":"completed","output":{"type":"text","text":"a.go"}}`,
				`{"id":"u2","parentId":"t1","type":"message","role":"user","timestamp":1767225700000,"content":[{"type":"input_text","text":"continued in 5.x"}]}`,
			},
			wantPin: true,
		},
		{
			name: "two replies at the same time",
			legacy: map[string]string{
				"u1": prompt,
				"a1": `{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","message":{"content":[{"type":"reasoning","text":"check the files"},{"type":"text","text":"old reply"}]}}`,
				"a2": `{"role":"assistant","createdAt":"2026-01-01T00:00:02Z","message":{"content":[{"type":"text","text":"second reply"}]}}`,
			},
			order:  []string{"u1", "a1", "a2"},
			pinned: "old reply",
			cbc: []string{cbcMeta, cbcUser,
				`{"id":"a1:reasoning","parentId":"u1","type":"reasoning","timestamp":1767225602000,"rawContent":[{"type":"reasoning_text","text":"check the files"}]}`,
				`{"id":"a1","parentId":"a1:reasoning","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"old reply"}]}`,
				`{"id":"a2","parentId":"a1","type":"message","role":"assistant","timestamp":1767225602000,"content":[{"type":"output_text","text":"second reply"}]}`,
				`{"id":"u2","parentId":"a2","type":"message","role":"user","timestamp":1767225700000,"content":[{"type":"input_text","text":"continued in 5.x"}]}`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcSyncHome(t)
			legacyRoot := t.TempDir()
			convDir := filepath.Join(legacyRoot, "history", "ws", "conv1")
			require.NoError(t, os.MkdirAll(filepath.Join(convDir, "messages"), 0o755))
			manifest := `{"messages":[`
			for i, id := range tc.order {
				if i > 0 {
					manifest += ","
				}
				manifest += `{"id":"` + id + `"}`
				require.NoError(t, os.WriteFile(filepath.Join(convDir, "messages", id+".json"), []byte(tc.legacy[id]), 0o600))
			}
			require.NoError(t, os.WriteFile(filepath.Join(convDir, "index.json"), []byte(manifest+"]}"), 0o600))

			database := openTestDB(t)
			engine := newCBCEngine(t, database, home, legacyRoot)
			engine.SyncAll(t.Context(), nil)
			const sessionID = "codebuddy:conv1"
			msgs, err := database.GetAllMessages(t.Context(), sessionID)
			require.NoError(t, err)
			var replyID int64
			for _, m := range msgs {
				if m.Role == "assistant" && m.Content == tc.pinned {
					replyID = m.ID
				}
			}
			require.NotZero(t, replyID, "the 4.x reply is archived")
			note := "keep this answer"
			_, err = database.PinMessage(t.Context(), sessionID, replyID, &note)
			require.NoError(t, err)

			transcript := filepath.Join(project, "conv1.jsonl")
			writeCBCSync(t, transcript, tc.cbc...)
			engine.SyncAll(t.Context(), nil)

			sess, err := database.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.NotNil(t, sess.FilePath)
			require.Equal(t, transcript, *sess.FilePath, "the converted transcript owns the conversation")
			pins, err := database.ListPinnedMessages(t.Context(), sessionID, "")
			require.NoError(t, err)
			if !tc.wantPin {
				assert.Empty(t, pins, "a reply that shares its time cannot be identified")
				return
			}
			require.Len(t, pins, 1, "the pin moves to the converted reply")
			require.NotNil(t, pins[0].Note)
			assert.Equal(t, note, *pins[0].Note)
			msgs, err = database.GetAllMessages(t.Context(), sessionID)
			require.NoError(t, err)
			var pinnedContent, pinnedUUID string
			found := false
			for _, m := range msgs {
				if m.ID == pins[0].MessageID {
					pinnedContent, pinnedUUID, found = m.Content, m.SourceUUID, true
					assert.Equal(t, "assistant", m.Role)
				}
			}
			require.True(t, found, "the pin points at a stored message")
			assert.Contains(t, pinnedContent, tc.pinned)
			assert.NotEqual(t, tc.pinned, pinnedContent, "the converted reply renders more than its text")
			assert.Equal(t, tc.wantUUID, pinnedUUID)
		})
	}
}
