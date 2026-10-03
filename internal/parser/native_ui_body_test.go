package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNativeUIMessageBodies(t *testing.T) {
	for _, format := range []string{"roo", "kilo-legacy"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			if format == "roo" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "history_item.json"),
					[]byte(`{"id":"native-ui","task":"request","status":"completed"}`), 0o644))
			} else {
				dir = writeKiloLegacyFixture(t)
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "ui_messages.json"), []byte(`[
{"ts":1000,"type":"say","say":"text","text":"[Thinking] is literal"},
{"ts":2000,"type":"say","say":"reasoning","text":"plan"},
{"ts":3000,"type":"say","say":"mcp_server_response","text":"unmatched-output"},
{"ts":4000,"type":"ask","ask":"command","text":"input-needle"},
{"ts":5000,"type":"say","say":"command_output","text":"output-needle"},
{"ts":6000,"type":"say","say":"condense_context","text":""},
{"ts":7000,"type":"say","say":"reasoning","text":""}
]`), 0o644))
			var sess *ParsedSession
			var msgs []ParsedMessage
			var err error
			if format == "roo" {
				sess, msgs, err = parseRooCodeSession(dir, "", "local")
			} else {
				sess, msgs, err = parseKiloLegacySession(dir, "", "local")
			}
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.Len(t, msgs, 6)
			assert.Equal(t, 1, sess.UserMessageCount)
			assert.Equal(t, "[Thinking] is literal", msgs[0].Content)
			assert.False(t, msgs[0].HasThinking)
			assert.Empty(t, msgs[1].Content)
			assert.Equal(t, "plan", msgs[1].ThinkingText)
			assert.Equal(t, 4, msgs[1].ContentLength)
			assert.Empty(t, msgs[2].Content)
			assert.Equal(t, "unmatched-output", msgs[2].ToolResultText)
			assert.Equal(t, SourceSubtypeToolResult, msgs[2].SourceSubtype)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_result", End: 16}}}, msgs[2].ContentLayout)
			require.Len(t, msgs[3].ToolCalls, 1)
			assert.Contains(t, msgs[3].ToolCalls[0].InputJSON, "input-needle")
			require.Len(t, msgs[3].ToolCalls[0].ResultEvents, 1)
			assert.Equal(t, "output-needle", msgs[3].ToolCalls[0].ResultEvents[0].Content)
			assert.Equal(t, "completed", msgs[3].ToolCalls[0].ResultEvents[0].Status)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "tool_call"}}}, msgs[3].ContentLayout)
			assert.True(t, msgs[4].IsCompactBoundary)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{}}, msgs[4].ContentLayout)
			assert.True(t, msgs[5].HasThinking)
			assert.Equal(t, &ContentLayout{Version: 1, Blocks: []ContentBlock{{Kind: "thinking"}}}, msgs[5].ContentLayout)
			assert.Equal(t, TerminationToolCallPending, sess.TerminationStatus)
			for _, msg := range msgs {
				require.NotNil(t, msg.ContentLayout)
			}
		})
	}
}
