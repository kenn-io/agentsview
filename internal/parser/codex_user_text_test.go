package parser

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexUserTextMixedInjectedBlocks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks string
		want   string
	}{
		{
			name: "prompt before context",
			blocks: `{"type":"input_text","text":"Review the changes"},` +
				`{"type":"input_text","text":"<environment_context>runtime context</environment_context>"}`,
			want: "Review the changes",
		},
		{
			name: "instructions before prompt",
			blocks: `{"type":"input_text","text":"# AGENTS.md\n<INSTRUCTIONS>Repository rules</INSTRUCTIONS>"},` +
				`{"type":"input_text","text":"Review the changes"}`,
			want: "Review the changes",
		},
		{
			name:   "prompt after envelope in same block",
			blocks: `{"type":"input_text","text":"<INSTRUCTIONS>Repository rules</INSTRUCTIONS>\nReview the changes"}`,
			want:   "Review the changes",
		},
		{
			name: "skill between prose blocks",
			blocks: `{"type":"input_text","text":"Review the changes"},` +
				`{"type":"input_text","text":"<skill>Skill instructions</skill>"},` +
				`{"type":"input_text","text":"Keep the public API"}`,
			want: "Review the changes\nKeep the public API",
		},
		{
			name:   "ordinary prose quoting envelope",
			blocks: `{"type":"input_text","text":"Explain <INSTRUCTIONS>example</INSTRUCTIONS> and this log: analysis: done"}`,
			want:   "Explain <INSTRUCTIONS>example</INSTRUCTIONS> and this log: analysis: done",
		},
	} {
		for _, later := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/later=%t", tc.name, later), func(t *testing.T) {
				lines := []string{testjsonl.CodexSessionMetaJSON(
					"mixed-context", "/tmp/project", "codex_cli_rs", tsEarly,
				)}
				if later {
					lines = append(lines, testjsonl.CodexMsgJSON("user", "Start here", tsEarly))
				}
				lines = append(lines, `{"type":"response_item","payload":{"type":"message",`+
					`"role":"user","id":"mixed-user","content":[`+tc.blocks+`]}}`)
				session, messages := runCodexParserTest(t, "mixed.jsonl", testjsonl.JoinJSONL(lines...), false)
				wantCount := len(lines) - 1
				require.Len(t, messages, wantCount)
				message := messages[len(messages)-1]
				assert.Equal(t, tc.want, message.Content)
				assert.Equal(t, wantCount, session.UserMessageCount)
			})
		}
	}
}
