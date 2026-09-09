package sync

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestStagedSingleEventSummaryThenAdditionalEvent(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		length        int
		failure       bool
	}{
		{"anonymous", "command not found", 17, true},
		{"whitespace", " \n", 0, false},
		{"sanitized", "ok\x00", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink, err := newCodexStagingSink(t.TempDir(), nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, sink.Close()) }()
			sink.AppendMessage(parser.ParsedMessage{ToolCalls: []parser.ParsedToolCall{{
				ToolUseID: "call", ToolName: "exec_command", Category: "Bash",
			}}})
			sink.AppendToolResultEvent("call", nil, parser.ParsedToolResultEvent{
				Source: "function_call_output", Content: tc.content,
			})
			require.NoError(t, sink.Err())
			key := db.StagedToolCallKey("call", 0)
			summary, length, err := sink.ResolveSummary(t.Context(), key)
			require.NoError(t, err)
			require.Empty(t, summary)
			require.Equal(t, tc.length, length)
			require.Equal(t, tc.failure, sink.ContentFailures()[key])
			sink.AppendToolResultEvent("call", nil, parser.ParsedToolResultEvent{
				Source: "function_call_output", Content: "done",
			})
			require.NoError(t, sink.Err())
			summary, length, err = sink.ResolveSummary(t.Context(), key)
			require.NoError(t, err)
			require.Equal(t, "done", summary)
			require.Equal(t, 4, length)
			require.False(t, sink.ContentFailures()[key])
		})
	}
}

func BenchmarkStagedSingleEventSummary(b *testing.B) {
	for _, size := range []int{1024, 1 << 20} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			sink, err := newCodexStagingSink(b.TempDir(), nil)
			require.NoError(b, err)
			defer func() { require.NoError(b, sink.Close()) }()
			sink.AppendMessage(parser.ParsedMessage{ToolCalls: []parser.ParsedToolCall{{
				ToolUseID: "call", ToolName: "exec_command", Category: "Bash",
			}}})
			sink.AppendToolResultEvent("call", nil, parser.ParsedToolResultEvent{
				Source: "function_call_output", Content: strings.Repeat("x", size),
			})
			require.NoError(b, sink.Err())
			key := db.StagedToolCallKey("call", 0)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				summary, length, err := sink.ResolveSummary(b.Context(), key)
				require.NoError(b, err)
				require.Empty(b, summary)
				require.Equal(b, size, length)
			}
		})
	}
}
