package parser

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAntigravityNativeProtoBodies(t *testing.T) {
	for _, agent := range []AgentType{AgentAntigravity, AgentAntigravityCLI} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "conversations", "22222222-3333-4444-5555-666666666666.db")
			mustMkdir(t, filepath.Dir(path))
			conn, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			createAntigravityStepTables(t, conn)
			// Step fields 19/20/14 and their payload fields come from the publisher's
			// 1.1.24 compiled trajectory/cortex descriptors.
			payloads := [][]byte{
				encodePB([]pbField{
					{num: 1, wire: pbWireVarint, varint: 14},
					{num: 19, wire: pbWireBytes, bytes: encodePB([]pbField{{num: 2, wire: pbWireBytes, bytes: []byte("[Thinking] is literal")}})},
				}),
				encodePB([]pbField{
					{num: 1, wire: pbWireVarint, varint: 15},
					{num: 20, wire: pbWireBytes, bytes: encodePB([]pbField{
						{num: 1, wire: pbWireBytes, bytes: []byte("assistant dialogue needle")},
						{num: 3, wire: pbWireBytes, bytes: []byte("reasoning needle more words")},
						{num: 6, wire: pbWireBytes, bytes: []byte("native-message")},
					})},
				}),
				encodePB([]pbField{{num: 14, wire: pbWireBytes, bytes: encodePB([]pbField{{num: 4, wire: pbWireBytes, bytes: []byte("tool output needle more words")}})}}),
				encodePB([]pbField{
					{num: 1, wire: pbWireVarint, varint: 15},
					{num: 20, wire: pbWireBytes, bytes: encodePB([]pbField{
						{num: 3, wire: pbWireBytes, bytes: []byte("opaque-only")},
						{num: 5, wire: pbWireVarint, varint: 1},
					})},
				}),
			}
			for i, payload := range payloads {
				mustExec(t, conn, "INSERT INTO steps (idx,step_type,step_payload) VALUES (?,?,?)", i, 0, payload)
			}
			require.NoError(t, conn.Close())
			var msgs []ParsedMessage
			if agent == AgentAntigravity {
				_, msgs, _, err = parseAntigravityTestSession(t, path, "project", "local")
			} else {
				_, msgs, err = parseAntigravityCLITestSession(t, path, "project", "local")
			}
			require.NoError(t, err)
			require.Len(t, msgs, 4)
			assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
			require.NotNil(t, msgs[0].ContentLayout)
			assert.Equal(t, "assistant dialogue needle", msgs[1].Content)
			assert.Equal(t, "reasoning needle more words", msgs[1].ThinkingText)
			assert.Equal(t, "native-message", msgs[1].SourceUUID)
			assert.Equal(t, 27, msgs[1].ContentLength)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking", End: 27}, {Kind: "text", End: 25}}}, msgs[1].ContentLayout)
			assert.Empty(t, msgs[2].Content)
			assert.Equal(t, "tool output needle more words", msgs[2].ToolResultText)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 29}}}, msgs[2].ContentLayout)
			assert.True(t, msgs[3].HasThinking)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[3].ContentLayout)
		})
	}
}

func TestAntigravityNativeHistoryAndArtifactBodies(t *testing.T) {
	root := t.TempDir()
	id := "22222222-3333-4444-5555-666666666666"
	path := filepath.Join(root, "conversations", id+".pb")
	writeSourceFile(t, path, "pb-stub")
	writeSourceFile(t, filepath.Join(root, "history.jsonl"), `{"conversationId":"`+id+`","display":"[Thinking] is literal","timestamp":1790812801000}`)
	writeSourceFile(t, filepath.Join(root, "brain", id, "plan.md"), "artifact-needle")
	_, msgs, err := parseAntigravityCLITestSession(t, path, "project", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	assert.Empty(t, msgs[0].Content)
	assert.Equal(t, "[plan.md]\nartifact-needle", msgs[0].ToolResultText)
	assert.Equal(t, 25, msgs[0].ContentLength)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 25}}}, msgs[0].ContentLayout)
	assert.Equal(t, "[Thinking] is literal", msgs[1].Content)
	require.NotNil(t, msgs[1].ContentLayout)
}

func TestAntigravityNativeRedactedTrajectoryBody(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "conversations", "22222222-3333-4444-5555-666666666666.pb")
	writeSourceFile(t, path, "pb-stub")
	writeSourceFile(t, filepath.Join(filepath.Dir(path), "22222222-3333-4444-5555-666666666666.trajectory.json"), `{"steps":[{"type":"CORTEX_STEP_TYPE_PLANNER_RESPONSE","plannerResponse":{"thinking":"opaque-only","thinkingRedacted":true,"thinkingSignature":"opaque-only"}}]}`)
	_, msgs, err := parseAntigravityCLITestSession(t, path, "project", "local")
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Empty(t, msgs[0].Content)
	assert.Empty(t, msgs[0].ThinkingText)
	assert.True(t, msgs[0].HasThinking)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[0].ContentLayout)
}

func TestAntigravityNativeProtoCallAndOutputFields(t *testing.T) {
	cases := []struct {
		name            string
		payload         []byte
		content, output string
		system          bool
		blocks          []ContentBlock
	}{
		{name: "planner call", payload: encodePB([]pbField{
			{num: 1, wire: pbWireVarint, varint: 15},
			{num: 20, wire: pbWireBytes, bytes: encodePB([]pbField{
				{num: 1, wire: pbWireBytes, bytes: []byte("literal")},
				{num: 3, wire: pbWireBytes},
				{num: 7, wire: pbWireBytes, bytes: encodePB([]pbField{
					{num: 1, wire: pbWireBytes, bytes: []byte("call-x")},
					{num: 2, wire: pbWireBytes, bytes: []byte("view_file")},
					{num: 3, wire: pbWireBytes, bytes: []byte(`{"path":"input-needle"}`)},
				})},
			})},
		}), content: "literal", blocks: []ContentBlock{{Kind: "thinking"}, {Kind: "text", End: 7}, {Kind: "tool_call"}}},
		{name: "command output", payload: encodePB([]pbField{{num: 28, wire: pbWireBytes, bytes: encodePB([]pbField{{num: 21, wire: pbWireBytes, bytes: encodePB([]pbField{{num: 1, wire: pbWireBytes, bytes: []byte("output-needle")}})}})}}), output: "output-needle", blocks: []ContentBlock{{Kind: "tool_result", End: 13}}},
		{name: "empty file output", payload: encodePB([]pbField{{num: 14, wire: pbWireBytes, bytes: encodePB([]pbField{{num: 4, wire: pbWireBytes}})}}), blocks: []ContentBlock{{Kind: "tool_result"}}},
		{name: "system", payload: encodePB([]pbField{{num: 114, wire: pbWireBytes, bytes: encodePB([]pbField{{num: 1, wire: pbWireBytes, bytes: []byte("notice")}})}}), content: "notice", system: true, blocks: []ContentBlock{{Kind: "text", End: 6}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "conversations", "22222222-3333-4444-5555-666666666666.db")
			mustMkdir(t, filepath.Dir(path))
			conn, err := sql.Open("sqlite3", path)
			require.NoError(t, err)
			createAntigravityStepTables(t, conn)
			mustExec(t, conn, "INSERT INTO steps (idx,step_type,step_payload) VALUES (0,0,?)", tc.payload)
			require.NoError(t, conn.Close())
			_, msgs, err := parseAntigravityCLITestSession(t, path, "project", "local")
			require.NoError(t, err)
			require.Len(t, msgs, 1)
			assert.Equal(t, tc.content, msgs[0].Content)
			assert.Equal(t, tc.output, msgs[0].ToolResultText)
			assert.Equal(t, tc.system, msgs[0].IsSystem)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: tc.blocks}, msgs[0].ContentLayout)
			if tc.name == "planner call" {
				require.Len(t, msgs[0].ToolCalls, 1)
				assert.Equal(t, "call-x", msgs[0].ToolCalls[0].ToolUseID)
				assert.Equal(t, `{"path":"input-needle"}`, msgs[0].ToolCalls[0].InputJSON)
				assert.Equal(t, "[Read: input-needle]", msgs[0].ToolCalls[0].Rendering)
			}
		})
	}
}
