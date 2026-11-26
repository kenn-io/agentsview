package parser

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexLaunchCallCwd(t *testing.T) {
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("abc-123", "/original", "user", "2024-01-01T10:00:00Z"),
		`{"type":"turn_context","payload":{"cwd":"/original"}}`,
		`{"type":"response_item","timestamp":"2024-01-01T10:00:02Z","payload":{"type":"function_call","name":"shell_command","call_id":"one","arguments":"{\"command\":\"codex exec hi\"}"}}`,
		`{"type":"turn_context","payload":{"cwd":"/later"}}`,
		`{"type":"response_item","timestamp":"2024-01-01T10:00:03Z","payload":{"type":"function_call","name":"shell_command","call_id":"two","arguments":"{\"command\":\"codex exec hi\"}"}}`,
	)
	_, msgs := runCodexParserTest(t, "test.jsonl", content, false)
	var dirs []string
	for _, msg := range msgs {
		for _, call := range msg.ToolCalls {
			dirs = append(dirs, call.WorkingDirectory)
		}
	}
	require.Equal(t, []string{"/original", "/later"}, dirs)
}
