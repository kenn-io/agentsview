package parser

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHermesNativeRoleBodies(t *testing.T) {
	for _, format := range []string{"jsonl", "json", "state"} {
		t.Run(format, func(t *testing.T) {
			var msgs []ParsedMessage
			switch format {
			case "jsonl":
				_, msgs = runHermesJSONLTest(t, "", `{"role":"user","content":"[Thinking] is literal","timestamp":"2026-04-03T15:00:00"}
{"role":"assistant","content":"answer","reasoning":"plan","tool_calls":[{"id":"call-1","function":{"name":"read_file","arguments":"{\"path\":\"input-needle\"}"}}],"timestamp":"2026-04-03T15:00:01"}
{"role":"tool","content":"output-needle","tool_call_id":"call-1","timestamp":"2026-04-03T15:00:02"}
{"role":"tool","content":"unmatched-output","timestamp":"2026-04-03T15:00:03"}`)
			case "json":
				_, msgs = runHermesJSONTest(t, "", `{"messages":[
{"role":"user","content":"[Thinking] is literal","timestamp":"2026-04-03T15:00:00"},
{"role":"assistant","content":"answer","reasoning":"plan","tool_calls":[{"id":"call-1","function":{"name":"read_file","arguments":"{\"path\":\"input-needle\"}"}}],"timestamp":"2026-04-03T15:00:01"},
{"role":"tool","content":"output-needle","tool_call_id":"call-1","timestamp":"2026-04-03T15:00:02"},
{"role":"tool","content":"unmatched-output","timestamp":"2026-04-03T15:00:03"}]}`)
			case "state":
				root := t.TempDir()
				createHermesTestStateDB(t, root,
					[]hermesTestSessionRow{{id: "native", startedAt: 1775228400}}, nil)
				conn, err := sql.Open("sqlite3", filepath.Join(root, "state.db"))
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), `INSERT INTO messages
(session_id, role, content, reasoning, tool_calls, tool_call_id, timestamp) VALUES
('native', 'user', '[Thinking] is literal', NULL, NULL, NULL, 1775228400),
('native', 'assistant', 'answer', 'plan', '[{"id":"call-1","function":{"name":"read_file","arguments":"{\"path\":\"input-needle\"}"}}]', NULL, 1775228401),
('native', 'tool', 'output-needle', NULL, NULL, 'call-1', 1775228402),
('native', 'tool', 'unmatched-output', NULL, NULL, NULL, 1775228403)`)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
				results, err := parseHermesTestArchive(t, root, "", "local")
				require.NoError(t, err)
				require.Len(t, results, 1)
				msgs = results[0].Messages
			}
			require.Len(t, msgs, 4)
			if format == "state" {
				assert.Equal(t, "native:2", msgs[1].SourceUUID)
			}
			assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
			assert.False(t, msgs[0].HasThinking)
			assert.Equal(t, "answer", msgs[1].Content)
			assert.Equal(t, "plan", msgs[1].ThinkingText)
			assert.Equal(t, 10, msgs[1].ContentLength)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
				{Kind: "thinking", End: 4}, {Kind: "text", End: 6}, {Kind: "tool_call"},
			}}, msgs[1].ContentLayout)
			require.Len(t, msgs[1].ToolCalls, 1)
			assert.Equal(t, `{"path":"input-needle"}`, msgs[1].ToolCalls[0].InputJSON)
			assert.Empty(t, msgs[2].Content)
			assert.Equal(t, "output-needle", msgs[2].ToolResultText)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 13}}}, msgs[2].ContentLayout)
			assert.Empty(t, msgs[3].Content)
			assert.Equal(t, "unmatched-output", msgs[3].ToolResultText)
			require.Len(t, msgs[3].ToolResults, 1)
			assert.Empty(t, msgs[3].ToolResults[0].ToolUseID)
			for _, msg := range msgs {
				require.NotNil(t, msg.ContentLayout)
			}
		})
	}
}

func TestHermesNativeReasoningFields(t *testing.T) {
	_, msgs := runHermesJSONTest(t, "", `{"messages":[
{"role":"assistant","content":"answer","reasoning_content":"alias-plan"},
{"role":"assistant","content":"","reasoning_details":[{"type":"reasoning.encrypted","data":"opaque-needle"}]},
{"role":"assistant","content":"done","codex_reasoning_items":[{"type":"reasoning","summary":[{"type":"summary_text","text":"summary-plan"}],"encrypted_content":"encrypted-needle"}]}]}`)
	require.Len(t, msgs, 3)
	assert.Equal(t, "answer", msgs[0].Content)
	assert.Equal(t, "alias-plan", msgs[0].ThinkingText)
	assert.Empty(t, msgs[1].Content)
	assert.True(t, msgs[1].HasThinking)
	assert.Empty(t, msgs[1].ThinkingText)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[1].ContentLayout)
	assert.Equal(t, "done", msgs[2].Content)
	assert.Equal(t, "summary-plan", msgs[2].ThinkingText)
	assert.NotContains(t, msgs[2].ThinkingText, "encrypted-needle")
}

func TestReasonixNativeRoleBodies(t *testing.T) {
	path := writeReasonixJSONL(t,
		`{"role":"user","content":"[Thinking] is literal"}`,
		`{"role":"assistant","content":"answer","reasoning_content":"plan","tool_calls":[{"id":"call-1","name":"read_file","arguments":"{\"path\":\"input-needle\"}"}]}`,
		`{"role":"tool","content":"output-needle","tool_call_id":"call-1"}`,
		`{"role":"tool","content":"unmatched-output"}`,
	)
	sess, msgs, _, err := parseReasonixSession(path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 4)
	assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
	assert.False(t, msgs[0].HasThinking)
	assert.Equal(t, "answer", msgs[1].Content)
	assert.Equal(t, "plan", msgs[1].ThinkingText)
	assert.Equal(t, 35, msgs[1].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
		{Kind: "thinking", End: 4}, {Kind: "text", End: 6}, {Kind: "tool_call"},
	}}, msgs[1].ContentLayout)
	assert.Empty(t, msgs[2].Content)
	assert.Equal(t, "output-needle", msgs[2].ToolResultText)
	assert.Empty(t, msgs[3].Content)
	assert.Equal(t, "unmatched-output", msgs[3].ToolResultText)
	for _, msg := range msgs {
		require.NotNil(t, msg.ContentLayout)
	}
}
