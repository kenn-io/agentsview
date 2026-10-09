package sync

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestStagedMultiAgentToolObservationParity(t *testing.T) {
	for _, prior := range []struct {
		content         string
		imagePeer, nul  bool
		outcome, ending string
	}{
		{"file contents", true, false, "content", "recovered"},
		{"file contents", false, true, "empty", "abandoned"},
		{"command not found", false, true, "empty", "abandoned"},
		{"[image]", false, true, "unknown", "unknown"},
		{"[image]", true, true, "unknown", "unknown"},
	} {
		for _, textAgent := range []string{"agent-a", ""} {
			t.Run(prior.content+strconv.FormatBool(prior.imagePeer)+textAgent, func(t *testing.T) {
				staged, err := newCodexStagingSink(t.Context(), t.TempDir(), nil)
				require.NoError(t, err)
				defer func() { require.NoError(t, staged.Close()) }()
				full := parser.NewCodexCollectingSink(0)
				for _, sink := range []parser.CodexSessionSink{full, staged} {
					sink.AppendMessage(parser.ParsedMessage{Role: parser.RoleAssistant, ToolCalls: []parser.ParsedToolCall{{ToolUseID: "grep", ToolName: "Grep", Category: "Grep"}}})
					sink.AppendToolResultEvent(t.Context(), "grep", nil, parser.ParsedToolResultEvent{Content: "No matches found", Status: "completed"})
					sink.AppendMessage(parser.ParsedMessage{Role: parser.RoleAssistant, ToolCalls: []parser.ParsedToolCall{{ToolUseID: "read", ToolName: "Read", Category: "Read"}}})
					sink.AppendToolResultEvent(t.Context(), "read", nil, parser.ParsedToolResultEvent{AgentID: textAgent, Content: prior.content, Status: "completed"})
					if prior.imagePeer {
						sink.AppendToolResultEvent(t.Context(), "read", nil, parser.ParsedToolResultEvent{AgentID: "image-agent", Content: "[image]", Status: "completed"})
					}
					if prior.nul {
						sink.AppendToolResultEvent(t.Context(), "read", nil, parser.ParsedToolResultEvent{AgentID: textAgent, Content: "\x00", Status: "completed"})
					}

				}
				for range 16 {
					for _, call := range []string{"grep", "read"} {
						_, _, err := staged.ResolveSummary(t.Context(), db.StagedToolCallKey(call, 0))
						require.NoError(t, err)
					}
					session := db.Session{ID: "fixture", TerminationStatus: new("clean")}
					fullMessages := toDBMessages(pendingWrite{sess: parser.ParsedSession{Agent: parser.AgentCodex}, msgs: full.Messages()}, nil)
					for i := range fullMessages {
						db.SanitizeMessage(&fullMessages[i])
					}
					want, _ := computeSignalsAndSecrets(session, fullMessages)
					got, _ := computeSignalsAndSecretsWithContentFailures(session, toDBMessages(pendingWrite{sess: parser.ParsedSession{Agent: parser.AgentCodex}, msgs: staged.Messages()}, nil), staged.ContentFailures())
					require.Equal(t, want.ToolObservations, got.ToolObservations)
					require.Equal(t, prior.outcome, got.ToolObservations[1].Outcome)
					require.Equal(t, new(prior.ending), got.ToolObservations[0].SequenceEnding)
				}
			})
		}
	}
}

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
			sink, err := newCodexStagingSink(t.Context(), t.TempDir(), nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, sink.Close()) }()
			sink.AppendMessage(parser.ParsedMessage{ToolCalls: []parser.ParsedToolCall{{
				ToolUseID: "call", ToolName: "exec_command", Category: "Bash",
			}}})
			sink.AppendToolResultEvent(t.Context(), "call", nil, parser.ParsedToolResultEvent{
				Source: "function_call_output", Content: tc.content,
			})
			require.NoError(t, sink.Err())
			key := db.StagedToolCallKey("call", 0)
			summary, length, err := sink.ResolveSummary(t.Context(), key)
			require.NoError(t, err)
			require.Empty(t, summary)
			require.Equal(t, tc.length, length)
			require.Equal(t, tc.failure, sink.ContentFailures()[key].Failure)
			sink.AppendToolResultEvent(t.Context(), "call", nil, parser.ParsedToolResultEvent{
				Source: "function_call_output", Content: "done",
			})
			require.NoError(t, sink.Err())
			summary, length, err = sink.ResolveSummary(t.Context(), key)
			require.NoError(t, err)
			require.Equal(t, "done", summary)
			require.Equal(t, 4, length)
			require.False(t, sink.ContentFailures()[key].Failure)
		})
	}
}

func BenchmarkStagedSingleEventSummary(b *testing.B) {
	for _, size := range []int{1024, 1 << 20} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			sink, err := newCodexStagingSink(b.Context(), b.TempDir(), nil)
			require.NoError(b, err)
			defer func() { require.NoError(b, sink.Close()) }()
			sink.AppendMessage(parser.ParsedMessage{ToolCalls: []parser.ParsedToolCall{{
				ToolUseID: "call", ToolName: "exec_command", Category: "Bash",
			}}})
			sink.AppendToolResultEvent(b.Context(), "call", nil, parser.ParsedToolResultEvent{
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
