package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkInsertMessagesToolPayloads covers bulk tool-result writes that the
// short-message benchmark does not exercise. Use a fixed -benchtime (1x or 3x)
// when comparing revisions because each iteration grows the archive.
func BenchmarkInsertMessagesToolPayloads(b *testing.B) {
	database := testDB(b)
	const messageCount = 12
	const line = "tool output 'quoted' café λ\n"
	const input = `{"command":"printf 'café λ'"}`
	payloads := make([]string, 3)
	for index, size := range []int{1 << 10, 64 << 10, 1 << 20} {
		payloads[index] = strings.Repeat(line, size/len(line)) +
			strings.Repeat("x", size%len(line))
	}
	messages := make([]Message, messageCount)
	var payloadBytes int64
	for index := range messages {
		payload := payloads[index%len(payloads)]
		callID := fmt.Sprintf("call-%d", index)
		messages[index] = Message{
			Ordinal: index, Role: "assistant", Content: "running a tool",
			ContentLength: len("running a tool"), HasToolUse: true,
			Timestamp: "2026-06-01T10:00:00Z",
			ToolCalls: []ToolCall{{
				MessageOrdinal: index, ToolName: "exec_command", Category: "Bash",
				ToolUseID: callID, InputJSON: input,
				ResultContent: payload, ResultContentLength: len(payload),
				ResultEvents: []ToolResultEvent{{
					ToolUseID: callID, Source: "function_call_output",
					Status: "completed", Content: payload, ContentLength: len(payload),
				}},
			}},
		}
		payloadBytes += int64(len(payload) + len(input) + len(messages[index].Content))
	}

	b.ReportAllocs()
	b.SetBytes(payloadBytes)
	b.ResetTimer()
	var sessionID string
	for iteration := range b.N {
		sessionID = fmt.Sprintf("bench-tool-payloads-%06d", iteration)
		require.NoError(b, database.UpsertSession(Session{
			ID: sessionID, Project: "bench", Machine: "local", Agent: "codex",
		}))
		for index := range messages {
			messages[index].SessionID = sessionID
			messages[index].ToolCalls[0].SessionID = sessionID
		}
		require.NoError(b, database.InsertMessages(messages))
	}
	b.StopTimer()

	stored, err := database.GetAllMessages(b.Context(), sessionID)
	require.NoError(b, err)
	require.Len(b, stored, messageCount)
	for index, message := range stored {
		require.Len(b, message.ToolCalls, 1)
		call := message.ToolCalls[0]
		require.Equal(b, input, call.InputJSON)
		require.Len(b, call.ResultEvents, 1)
		require.Equal(b, payloads[index%len(payloads)], call.ResultEvents[0].Content)
	}
}
