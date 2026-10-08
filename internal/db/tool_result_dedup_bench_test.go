package db

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// benchSummary renders the parser's summary for the seeded events without
// depending on a db-level summarizer, so the file compiles on builds that
// store the summary and on builds that derive it.
func benchSummary(events []ToolResultEvent) string {
	if len(events) == 1 {
		return events[0].Content
	}
	parts := make([]string, 0, len(events))
	for _, ev := range events {
		parts = append(parts, ev.AgentID+":\n"+ev.Content)
	}
	return strings.Join(parts, "\n\n")
}

// Read-path cost of deriving tool-result summaries from events. Every call
// with events pays the summarizer on load, so the worst case is a session
// where each call carries several events from several agents, which takes
// the multi-agent rendering path. The single-event case is what almost every
// real call looks like.

func seedBenchToolResultSession(
	b *testing.B, d *DB, sessionID string, msgs, callsPerMsg, eventsPerCall int,
) {
	b.Helper()
	if err := d.UpsertSession(b.Context(), Session{
		ID: sessionID, Project: "bench", Machine: "local", Agent: "claude",
	}); err != nil {
		require.NoError(b, err, "seed session")
	}
	out := make([]Message, 0, msgs)
	for i := range msgs {
		m := Message{
			SessionID:  sessionID,
			Ordinal:    i,
			Role:       "assistant",
			Content:    fmt.Sprintf("assistant turn %d", i),
			HasToolUse: true,
			Timestamp:  "2026-06-01T10:00:00Z",
		}
		for c := range callsPerMsg {
			tc := ToolCall{
				SessionID: sessionID,
				ToolName:  "Agent",
				Category:  "Task",
				ToolUseID: fmt.Sprintf("call_%d_%d", i, c),
			}
			for e := range eventsPerCall {
				content := fmt.Sprintf(
					"agent %d reporting on turn %d call %d with a few "+
						"hundred bytes of output text so the row is "+
						"realistic: %s",
					e, i, c, string(make([]byte, 300)),
				)
				ev := ToolResultEvent{
					Source:        "subagent_notification",
					Status:        "completed",
					Content:       content,
					ContentLength: len(content),
					EventIndex:    e,
				}
				if eventsPerCall > 1 {
					ev.AgentID = fmt.Sprintf("agent-%d", e)
				}
				tc.ResultEvents = append(tc.ResultEvents, ev)
			}
			tc.ResultContent = benchSummary(tc.ResultEvents)
			tc.ResultContentLength = len(tc.ResultContent)
			m.ToolCalls = append(m.ToolCalls, tc)
		}
		out = append(out, m)
	}
	if err := d.InsertMessages(b.Context(), out); err != nil {
		require.NoError(b, err, "seed messages")
	}
}

func benchGetMessagesWithEvents(b *testing.B, eventsPerCall int) {
	b.Helper()

	d := testDB(b)
	const msgs, callsPerMsg = 200, 3
	seedBenchToolResultSession(b, d, "bench-events", msgs, callsPerMsg, eventsPerCall)
	ctx := b.Context()
	b.ResetTimer()
	for b.Loop() {
		got, err := d.GetMessages(ctx, "bench-events", 0, msgs, true)
		if err != nil {
			require.NoError(b, err)
		}
		if len(got) != msgs || got[0].ToolCalls[0].ResultContent == "" {
			require.NotEmpty(b, got, "unexpected load")
			require.NotEmpty(b, got[0].ToolCalls, "unexpected load")
			require.NotEmpty(b, got[0].ToolCalls[0].ResultContent, "unexpected load")
		}
	}
}

func BenchmarkGetMessagesSingleEventCalls(b *testing.B) {
	benchGetMessagesWithEvents(b, 1)
}

func BenchmarkGetMessagesFiveAgentEventCalls(b *testing.B) {
	benchGetMessagesWithEvents(b, 5)
}

func BenchmarkRecallEvidenceWindowFiveAgentEventCalls(b *testing.B) {
	d := testDB(b)
	const msgs, callsPerMsg = 200, 3
	seedBenchToolResultSession(b, d, "bench-recall", msgs, callsPerMsg, 5)
	ctx := b.Context()
	b.ResetTimer()
	for b.Loop() {
		w, err := d.BuildRecallEvidenceWindow(ctx, "bench-recall", 0, msgs-1)
		if err != nil {
			require.NoError(b, err)
		}
		if len(w.Messages) != msgs || w.Messages[0].ToolCalls[0].ResultContent == "" {
			require.Len(b, w.Messages, msgs, "unexpected window")
			require.NotEmpty(b, w.Messages[0].ToolCalls, "unexpected window")
			require.NotEmpty(b, w.Messages[0].ToolCalls[0].ResultContent, "unexpected window")
		}
	}
}

func BenchmarkRecallEvidenceWindowSingleEventCallsColdPools(b *testing.B) {
	d := testDB(b)
	const msgs, callsPerMsg = 200, 3
	seedBenchToolResultSession(b, d, "bench-recall-1", msgs, callsPerMsg, 1)
	ctx := b.Context()
	b.ResetTimer()
	for b.Loop() {
		// Two collections evict both the primary and victim sync.Pool
		// caches. Keep them outside the measurement so every operation
		// measures the same cold-pool read instead of whichever pool state
		// the process happened to inherit from earlier benchmarks.
		b.StopTimer()
		runtime.GC()
		runtime.GC()
		b.StartTimer()
		w, err := d.BuildRecallEvidenceWindow(ctx, "bench-recall-1", 0, msgs-1)
		if err != nil {
			require.NoError(b, err)
		}
		if len(w.Messages) != msgs || w.Messages[0].ToolCalls[0].ResultContent == "" {
			require.Len(b, w.Messages, msgs, "unexpected window")
			require.NotEmpty(b, w.Messages[0].ToolCalls, "unexpected window")
			require.NotEmpty(b, w.Messages[0].ToolCalls[0].ResultContent, "unexpected window")
		}
	}
}

func BenchmarkToolCallResultStateFixedAgent(b *testing.B) {
	for _, mixed := range []bool{false, true} {
		b.Run(fmt.Sprintf("sole1MiB/mixed%t", mixed), func(b *testing.B) {
			d := testDB(b)
			require.NoError(b, d.UpsertSession(b.Context(), Session{ID: "sole", Project: "bench", Machine: "local", Agent: "codex"}))
			messages := []Message{{SessionID: "sole", Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: "summary", ResultEvents: []ToolResultEvent{{Content: strings.Repeat("x", 1<<20)}}}}}}
			positions := []ToolCallPosition{{}}
			if mixed {
				messages = append(messages, Message{SessionID: "sole", Ordinal: 1, Role: "assistant", ToolCalls: []ToolCall{{ToolName: "Read", ResultContent: "summary", ResultEvents: []ToolResultEvent{{Content: "[image]"}, {EventIndex: 1, Content: "text"}}}}})
				positions = append(positions, ToolCallPosition{MessageOrdinal: 1})
			}
			require.NoError(b, d.InsertMessages(b.Context(), messages))
			tx, err := d.getWriter().Begin(b.Context())
			require.NoError(b, err)
			for _, position := range positions {
				require.NoError(b, ensureToolCallAgentStateTx(b.Context(), tx, "sole", position))
			}
			require.NoError(b, tx.Commit())
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				tx, err := d.getWriter().Begin(b.Context())
				require.NoError(b, err)
				facts, err := (signalTxQuery{tx: tx, sessionID: "sole"}).ToolCallsByPosition(b.Context(), positions)
				require.NoError(b, err)
				require.Len(b, facts, len(positions))
				for _, fact := range facts {
					require.False(b, fact.ResultContentUnknown)
				}
				require.NoError(b, tx.Rollback())
			}
		})
	}
	for _, count := range []int{100, 10000} {
		for _, blank := range []bool{false, true} {
			firstSizes := []int{0}
			if !blank {
				firstSizes = append(firstSizes, 1<<20)
			}
			for _, firstSize := range firstSizes {
				b.Run(fmt.Sprintf("events%d/blank%t/first%d", count, blank, firstSize), func(b *testing.B) {
					d := testDB(b)
					require.NoError(b, d.UpsertSession(b.Context(), Session{ID: "state", Project: "bench", Machine: "local", Agent: "codex"}))
					events := make([]ToolResultEvent, count)
					for i := range events {
						events[i] = ToolResultEvent{AgentID: "agent-a", EventIndex: i}
						if !blank {
							events[i].Content = fmt.Sprintf("result %d", i)
						}
						if i == 0 && firstSize > 0 {
							events[i].Content = strings.Repeat("x", firstSize)
						}
						PrepareToolResultEvent(&events[i])
					}
					require.NoError(b, d.InsertMessages(b.Context(), []Message{{SessionID: "state", Ordinal: 0, Role: "assistant", ToolCalls: []ToolCall{{ToolUseID: "call", ToolName: "wait_agent", ResultEvents: events}}}}))
					tx, err := d.getWriter().Begin(b.Context())
					require.NoError(b, err)
					require.NoError(b, ensureToolCallAgentStateTx(b.Context(), tx, "state", ToolCallPosition{}))
					require.NoError(b, tx.Commit())
					content := "next result"
					if blank {
						content = "\t"
					}
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						tx, err := d.getWriter().Begin(b.Context())
						require.NoError(b, err)
						changed, _, err := applyToolCallResultUpdateTx(b.Context(), tx, "state", ToolCallResultUpdate{ToolUseID: "call", Events: []ToolResultEvent{{AgentID: "agent-a", Content: content}}}, nil, "", "")
						require.NoError(b, err)
						require.True(b, changed)
						facts, err := (signalTxQuery{tx: tx, sessionID: "state"}).ToolCallsByPosition(b.Context(), []ToolCallPosition{{}})
						require.NoError(b, err)
						require.Len(b, facts, 1)
						require.False(b, facts[0].ResultContentUnknown)
						require.NoError(b, tx.Rollback())
					}
				})
			}
		}
	}
}
