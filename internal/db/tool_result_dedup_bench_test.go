package db

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
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
	if err := d.UpsertSession(Session{
		ID: sessionID, Project: "bench", Machine: "local", Agent: "claude",
	}); err != nil {
		b.Fatalf("seed session: %v", err)
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
	if err := d.InsertMessages(out); err != nil {
		b.Fatalf("seed messages: %v", err)
	}
}

func benchGetMessagesWithEvents(b *testing.B, eventsPerCall int) {
	d := testDB(b)
	const msgs, callsPerMsg = 200, 3
	seedBenchToolResultSession(b, d, "bench-events", msgs, callsPerMsg, eventsPerCall)
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		got, err := d.GetMessages(ctx, "bench-events", 0, msgs, true)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != msgs || got[0].ToolCalls[0].ResultContent == "" {
			b.Fatal("unexpected load")
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
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		w, err := d.BuildRecallEvidenceWindow(ctx, "bench-recall", 0, msgs-1)
		if err != nil {
			b.Fatal(err)
		}
		if len(w.Messages) != msgs || w.Messages[0].ToolCalls[0].ResultContent == "" {
			b.Fatal("unexpected window")
		}
	}
}

func BenchmarkRecallEvidenceWindowSingleEventCallsColdPools(b *testing.B) {
	d := testDB(b)
	const msgs, callsPerMsg = 200, 3
	seedBenchToolResultSession(b, d, "bench-recall-1", msgs, callsPerMsg, 1)
	ctx := context.Background()
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
			b.Fatal(err)
		}
		if len(w.Messages) != msgs || w.Messages[0].ToolCalls[0].ResultContent == "" {
			b.Fatal("unexpected window")
		}
	}
}
