package parser

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAntigravityUnknownInnerSchemaKeepsLegacyBody(t *testing.T) {
	for _, agent := range []AgentType{AgentAntigravity, AgentAntigravityCLI} {
		t.Run(string(agent), func(t *testing.T) {
			for _, field := range []int{19, 20, 14, 28, 114} {
				t.Run(fmt.Sprintf("field_%d", field), func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "conversations", "22222222-3333-4444-5555-666666666666.db")
					mustMkdir(t, filepath.Dir(path))
					conn, err := sql.Open("sqlite3", path)
					require.NoError(t, err)
					createAntigravityStepTables(t, conn)
					kind := uint64(15)
					if field == 19 {
						kind = 14
					}
					payload := encodePB([]pbField{
						{num: 1, wire: pbWireVarint, varint: kind},
						{num: field, wire: pbWireBytes, bytes: encodePB([]pbField{{num: 77, wire: pbWireBytes, bytes: []byte("future schema visible dialogue")}})},
					})
					mustExec(t, conn, "INSERT INTO steps (idx,step_type,step_payload) VALUES (0,0,?)", payload)
					require.NoError(t, conn.Close())
					var msgs []ParsedMessage
					if agent == AgentAntigravity {
						_, msgs, _, err = parseAntigravityTestSession(t, path, "project", "local")
					} else {
						_, msgs, err = parseAntigravityCLITestSession(t, path, "project", "local")
					}
					require.NoError(t, err)
					require.Len(t, msgs, 1)
					assert.Equal(t, "future schema visible dialogue", msgs[0].Content)
					assert.Nil(t, msgs[0].ContentLayout)
				})
			}
		})
	}
}

func TestHermesNativeStateStructuredContent(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sessions"), 0o755))
	createHermesTestStateDB(t, root, []hermesTestSessionRow{{id: "structured", startedAt: 1788878363}}, []hermesTestMessageRow{
		{sessionID: "structured", role: "user", content: "\x00json:" + `[{"type":"text","text":"[Thinking] is literal"},{"type":"image_url","image_url":{"url":"https://example.invalid/image"}}]`, timestamp: 1788878364},
		{sessionID: "structured", role: "assistant", content: "\x00json:" + `[{"type":"text","text":"answer"}]`, timestamp: 1788878365},
		{sessionID: "structured", role: "tool", content: "\x00json:" + `[{"type":"text","text":"output-needle"}]`, timestamp: 1788878366},
		{sessionID: "structured", role: "user", content: "\x00json:" + `{"future_text":"future body"}`, timestamp: 1788878367},
		{sessionID: "structured", role: "assistant", content: "\x00json:" + `[{"type":"future","text":"unproven work"}]`, timestamp: 1788878368},
	})
	conn, err := sql.Open("sqlite3", filepath.Join(root, "state.db"))
	require.NoError(t, err)
	mustExec(t, conn, "UPDATE messages SET reasoning='plan' WHERE id=2")
	require.NoError(t, conn.Close())
	results, err := parseHermesTestArchive(t, root, "project", "local")
	require.NoError(t, err)
	require.Len(t, results, 1)
	msgs := results[0].Messages
	require.Len(t, msgs, 5)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.Equal(t, "structured:1", msgs[0].SourceUUID)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "text", End: 21}}}, msgs[0].ContentLayout)
	assert.Equal(t, "answer", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking", End: 4}, {Kind: "text", End: 6}}}, msgs[1].ContentLayout)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[2].ContentLayout)
	assert.Equal(t, `json:{"future_text":"future body"}`, msgs[3].Content)
	assert.Nil(t, msgs[3].ContentLayout)
	assert.Equal(t, `json:[{"type":"future","text":"unproven work"}]`, msgs[4].Content)
	assert.Nil(t, msgs[4].ContentLayout)
}

func TestHermesUnknownContentKeepsCompleteLegacyReasoning(t *testing.T) {
	const opaque = `json:[{"type":"future_part","text":"opaque-body"}]`
	const expected = "[Thinking]\nretained-plan\n[/Thinking]\n\n" + opaque
	file := hermesAssistantBody(gjson.Parse(`{"role":"assistant","content":"\u0000json:[{\"type\":\"future_part\",\"text\":\"opaque-body\"}]","reasoning":"retained-plan"}`))
	state := convertHermesStateMessages([]hermesStateMessage{{role: "assistant", content: "\x00" + opaque, reasoning: "retained-plan"}}, "legacy-reasoning")
	require.Len(t, state, 1)
	for _, message := range []ParsedMessage{file, state[0]} {
		assert.Nil(t, message.ContentLayout)
		assert.Equal(t, expected, message.Content)
		assert.Equal(t, "retained-plan", message.ThinkingText)
		assert.True(t, message.HasThinking)
	}
}

func TestNativeUsageOnlyDoesNotChangeTerminationSpeaker(t *testing.T) {
	cases := []struct {
		name, records string
		stop          string
		want          TerminationStatus
	}{
		{name: "pending call", records: `{"type":"function_call","name":"read","callId":"call-x","arguments":{"path":"input-needle"}}
{"type":"message","role":"assistant","content":"","providerData":{"usage":{"inputTokens":10,"outputTokens":2}}}`, want: TerminationToolCallPending},
		{name: "resolved call", records: `{"type":"function_call","name":"read","callId":"call-x","arguments":{"path":"input-needle"}}
{"type":"function_call_result","callId":"call-x","output":"output-needle"}
{"type":"message","role":"assistant","content":"","providerData":{"usage":{"inputTokens":10,"outputTokens":2}}}`, want: TerminationClean},
		{name: "user replied", records: `{"type":"message","role":"assistant","content":"answer"}
{"type":"message","role":"user","content":"next question"}
{"type":"message","role":"assistant","content":"","providerData":{"usage":{"inputTokens":10,"outputTokens":2}}}`, stop: "end_turn", want: TerminationClean},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.jsonl")
			writeSourceFile(t, path, tc.records+"\n")
			_, msgs, err := parseWorkBuddyTestSession(t, path, "project", "local")
			require.NoError(t, err)
			require.NotEmpty(t, msgs)
			assert.Equal(t, tc.want, Classify(msgs, tc.stop, false))
		})
	}
}

func TestAntigravityNativeRawThinkingField(t *testing.T) {
	for _, agent := range []AgentType{AgentAntigravity, AgentAntigravityCLI} {
		t.Run(string(agent), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "conversations", "22222222-3333-4444-5555-666666666666.db")
			mustMkdir(t, filepath.Dir(path))
			conn, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			createAntigravityStepTables(t, conn)
			payload := encodePB([]pbField{
				{num: 1, wire: pbWireVarint, varint: 15},
				{num: 20, wire: pbWireBytes, bytes: encodePB([]pbField{{num: 16, wire: pbWireBytes, bytes: []byte("thinking-needle")}})},
			})
			mustExec(t, conn, "INSERT INTO steps (idx,step_type,step_payload) VALUES (0,0,?)", payload)
			require.NoError(t, conn.Close())
			var msgs []ParsedMessage
			if agent == AgentAntigravity {
				_, msgs, _, err = parseAntigravityTestSession(t, path, "project", "local")
			} else {
				_, msgs, err = parseAntigravityCLITestSession(t, path, "project", "local")
			}
			require.NoError(t, err)
			require.Len(t, msgs, 1)
			assert.Empty(t, msgs[0].Content)
			assert.True(t, msgs[0].HasThinking)
			assert.Equal(t, "thinking-needle", msgs[0].ThinkingText)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking", End: 15}}}, msgs[0].ContentLayout)
		})
	}
}
