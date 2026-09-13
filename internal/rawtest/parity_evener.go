package rawtest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func EvenerParity(t *testing.T, root string) ParityFixture {
	dir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "primary.transcript.jsonl"), []byte(`{"kind":"header","format_version":2,"session_id":"primary","created_at":"2026-07-06T12:00:00Z","model":"gpt-5.4","profile_id":"synthetic","working_dir":"/workspace/project"}
{"kind":"entry","seq":0,"turn":{"kind":"USER_INPUT","timestamp":"2026-07-06T12:00:01Z","message":{"content":[{"kind":"text","text":"Inspect Evener."}]}}}
{"kind":"entry","seq":1,"turn":{"kind":"ASSISTANT","timestamp":"2026-07-06T12:00:02Z","message":{"content":[{"kind":"tool_call","tool_call":{"id":"shell-1","name":"shell","arguments":{"command":"pwd"}}}]},"usage":{"input_tokens":100,"output_tokens":7}}}
{"kind":"entry","seq":2,"turn":{"kind":"TOOL_RESULTS","timestamp":"2026-07-06T12:00:03Z","message":{"content":[{"kind":"tool_result","tool_result":{"tool_call_id":"shell-1","name":"shell","content":{"exit_code":1,"output":"synthetic failure"},"is_error":true}}]}}}
{"kind":"entry","seq":3,"turn":{"kind":"ASSISTANT","timestamp":"2026-07-06T12:00:04Z","message":{"content":[{"kind":"text","text":"Recorded."}]},"usage":{"input_tokens":0,"output_tokens":0}}}
{"kind":"entry","seq":4,"turn":{"kind":"ASSISTANT","timestamp":"2026-07-06T12:00:05Z","message":{"content":[{"kind":"text","text":"No further usage."}]}}}
`), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "primary.meta.json"), []byte(`{"id":"primary","name":"Evener review"}`), 0600))
	return ParityFixture{root, []ParitySession{{ID: "evener:primary", MessageUsage: map[int]ParityMessageUsage{0: {}, 1: {Context: 100, Output: 7, Present: true}, 3: {Present: true}, 4: {}}, Title: "Evener review", First: "Inspect Evener.", Roles: []string{"user", "assistant", "tool", "assistant", "assistant"}, Output: 7, HasOutput: true, ToolName: "shell", ToolResult: "synthetic failure"}}}
}
