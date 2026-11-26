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
	path := createTestFile(t, "test.jsonl", content)
	for _, agent := range []AgentType{AgentCodex, AgentTraeX, AgentAugureCode, AgentInterpreter} {
		t.Run(string(agent), func(t *testing.T) {
			provider, ok := NewProvider(agent, ProviderConfig{Machine: "local"})
			require.True(t, ok)
			_, msgs, err := provider.(*codexProvider).parseSession(path, "local", false)
			require.NoError(t, err)
			var dirs []string
			for _, msg := range msgs {
				for _, call := range msg.ToolCalls {
					dirs = append(dirs, call.WorkingDirectory)
				}
			}
			want := []string{"", ""}
			if agent == AgentCodex {
				want = []string{"/original", "/later"}
			}
			require.Equal(t, want, dirs)
		})
	}
}
