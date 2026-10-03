package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKimiNativeWireBodies(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"legacy", []string{
			`{"timestamp":1000,"message":{"type":"TurnBegin","payload":{"user_input":[{"type":"text","text":"[Thinking] is literal"}]}}}`,
			`{"timestamp":1001,"message":{"type":"ContentPart","payload":{"type":"text","text":"first"}}}`,
			`{"timestamp":1002,"message":{"type":"ContentPart","payload":{"type":"think","think":"plan"}}}`,
			`{"timestamp":1003,"message":{"type":"ToolCall","payload":{"id":"call-1","function":{"name":"Glob","arguments":"{\"pattern\":\"input-needle\"}"}}}}`,
			`{"timestamp":1004,"message":{"type":"ContentPart","payload":{"type":"text","text":"after"}}}`,
			`{"timestamp":1005,"message":{"type":"ToolResult","payload":{"tool_call_id":"call-1","return_value":{"output":"output-needle"}}}}`,
			`{"timestamp":1006,"message":{"type":"ContentPart","payload":{"type":"think","think":"","encrypted":"encrypted-needle"}}}`,
			`{"timestamp":1007,"message":{"type":"TurnEnd","payload":{}}}`,
		}},
		{"native", []string{
			`{"timestamp":1000,"type":"turn.prompt","input":[{"type":"text","text":"[Thinking] is literal"}]}`,
			`{"timestamp":1001,"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"first"}}}`,
			`{"timestamp":1002,"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"think","think":"plan"}}}`,
			`{"timestamp":1003,"type":"context.append_loop_event","event":{"type":"tool.call","toolCallId":"call-1","name":"Glob","args":{"pattern":"input-needle"}}}`,
			`{"timestamp":1004,"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"after"}}}`,
			`{"timestamp":1005,"type":"context.append_loop_event","event":{"type":"tool.result","toolCallId":"call-1","result":{"output":"output-needle"}}}`,
			`{"timestamp":1006,"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"think","think":"","encrypted":"encrypted-needle"}}}`,
			`{"timestamp":1007,"type":"context.append_loop_event","event":{"type":"step.end"}}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeKimiWireJSONL(t, "project", "native-body", tc.lines)
			sess, msgs, err := parseKimiSession(path, "project", "local")
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.Len(t, msgs, 4)
			assert.Equal(t, 1, sess.UserMessageCount)
			assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
			assert.False(t, msgs[0].HasThinking)
			assert.Equal(t, "first\nafter", msgs[1].Content)
			assert.Equal(t, "plan", msgs[1].ThinkingText)
			assert.Equal(t, 60, msgs[1].ContentLength)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{
				{Kind: "text", End: 5}, {Kind: "thinking", End: 4}, {Kind: "tool_call"}, {Kind: "text", Start: 6, End: 11},
			}}, msgs[1].ContentLayout)
			require.Len(t, msgs[1].ToolCalls, 1)
			assert.Equal(t, "[Glob: input-needle]", msgs[1].ToolCalls[0].Rendering)
			assert.Empty(t, msgs[2].Content)
			assert.Equal(t, "output-needle", msgs[2].ToolResultText)
			assert.True(t, msgs[3].HasThinking)
			assert.Empty(t, msgs[3].ThinkingText)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[3].ContentLayout)
			for _, msg := range msgs {
				require.NotNil(t, msg.ContentLayout)
			}
		})
	}
}

func TestKimiNativeUsageOnlyBody(t *testing.T) {
	path := writeKimiWireJSONL(t, "project", "usage-body", []string{
		`{"time":1000,"type":"turn.prompt","input":[{"type":"text","text":"request"}]}`,
		`{"time":2000,"type":"context.append_loop_event","event":{"type":"step.end","model":"kimi-code/kimi-for-coding","usage":{"inputOther":10,"output":5},"finishReason":"end_turn"}}`,
		`{"time":2001,"type":"usage.record","model":"kimi-code/kimi-for-coding","usage":{"inputOther":10,"output":5}}`,
	})
	sess, msgs, err := parseKimiSession(path, "project", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 2)
	assert.Empty(t, msgs[1].Content)
	assert.False(t, msgs[1].HasThinking)
	assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{}}, msgs[1].ContentLayout)
	assert.Equal(t, int64(2000), msgs[1].Timestamp.UnixMilli())
	assert.Equal(t, 10, msgs[1].ContextTokens)
	assert.Equal(t, 5, msgs[1].OutputTokens)
	assert.Equal(t, "end_turn", msgs[1].StopReason)
	assert.Equal(t, 5, sess.TotalOutputTokens)
	assert.Equal(t, 10, sess.PeakContextTokens)
}
