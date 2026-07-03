package db

import (
	"encoding/json"
	"fmt"
	"testing"
)

// Hot-path benchmarks for the message write and usage aggregation
// paths that have regressed in the past. CI's bench-gate workflow
// runs them on every PR and compares allocs/op, B/op, and ns/op
// against the merge base, so a reintroduced O(session-history)
// rewrite or per-row JSON parse fails the PR instead of shipping:
//
//   - BenchmarkReplaceSessionMessagesStreamingMerge: a one-row tail
//     change must take the in-place diff path (one UPDATE) rather
//     than delete+reinserting every row and rewriting the FTS index
//     (regressed pre-#954: streaming chunk merges rewrote whole
//     sessions on every appended chunk).
//   - BenchmarkInsertMessagesBatch: bulk ingest must keep multi-row
//     batched inserts (#411).
//
// BenchmarkGetDailyUsage in usage_test.go covers the usage
// aggregation scan (#309) and is part of the same CI gate.

// benchSessionMessages builds n alternating user/assistant messages.
// Assistant messages carry a model and token_usage payload so writes
// exercise the same columns real ingest does.
func benchSessionMessages(sessionID string, n int) []Message {
	msgs := make([]Message, 0, n)
	for i := range n {
		role := "user"
		content := fmt.Sprintf(
			"user message %d with enough text to look real", i,
		)
		if i%2 == 1 {
			role = "assistant"
			content = fmt.Sprintf(
				"assistant reply %d with enough text to look real", i,
			)
		}
		m := Message{
			SessionID:     sessionID,
			Ordinal:       i,
			Role:          role,
			Content:       content,
			Timestamp:     fmt.Sprintf("2026-06-%02dT10:00:00Z", 1+i%28),
			ContentLength: len(content),
		}
		if role == "assistant" {
			m.Model = "claude-bench-model"
			m.TokenUsage = json.RawMessage(
				`{"input_tokens":120,"output_tokens":45,` +
					`"cache_creation_input_tokens":10,` +
					`"cache_read_input_tokens":200}`,
			)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

func seedBenchSession(
	b *testing.B, d *DB, sessionID string, n int,
) []Message {
	b.Helper()
	if err := d.UpsertSession(Session{
		ID:      sessionID,
		Project: "bench",
		Machine: "local",
		Agent:   "claude",
	}); err != nil {
		b.Fatalf("seed session %s: %v", sessionID, err)
	}
	msgs := benchSessionMessages(sessionID, n)
	if err := d.InsertMessages(msgs); err != nil {
		b.Fatalf("seed messages for %s: %v", sessionID, err)
	}
	return msgs
}

// BenchmarkReplaceSessionMessagesStreamingMerge measures the
// streaming chunk-merge shape: replacing a stored session where only
// the tail message's content changed. The diff planner must apply a
// single in-place UPDATE; cost must not scale with the number of
// unchanged stored rows being rewritten.
func BenchmarkReplaceSessionMessagesStreamingMerge(b *testing.B) {
	const stored = 1000
	d := testDB(b)
	msgs := seedBenchSession(b, d, "bench-replace", stored)
	last := len(msgs) - 1

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		content := fmt.Sprintf(
			"assistant reply %d merged streaming tail variant %d",
			last, i,
		)
		msgs[last].Content = content
		msgs[last].ContentLength = len(content)
		if err := d.ReplaceSessionMessages("bench-replace", msgs); err != nil {
			b.Fatalf("replace: %v", err)
		}
	}
}

// BenchmarkInsertMessagesBatch measures bulk session ingest: one
// session row plus a batch insert of its messages, the unit of work
// the full-sync write pipeline performs per session.
//
// Each iteration adds a new session, so the database grows with the
// iteration count and per-op cost is only comparable between runs
// with the same count: the bench gate always runs with a fixed
// -benchtime=Nx (see bench.yml and the Makefile) so baseline and
// candidate insert into identically sized databases.
func BenchmarkInsertMessagesBatch(b *testing.B) {
	const batch = 200
	d := testDB(b)

	// Build the message fixture once: constructing 200 Message
	// structs (~400 fmt.Sprintf calls) inside the timed loop would
	// be gated as if it were ingest cost. Only the SessionID is
	// rewritten per iteration, which allocates nothing.
	msgs := benchSessionMessages("", batch)

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		sid := fmt.Sprintf("bench-insert-%06d", i)
		if err := d.UpsertSession(Session{
			ID:      sid,
			Project: "bench",
			Machine: "local",
			Agent:   "claude",
		}); err != nil {
			b.Fatalf("upsert session: %v", err)
		}
		for j := range msgs {
			msgs[j].SessionID = sid
		}
		if err := d.InsertMessages(msgs); err != nil {
			b.Fatalf("insert messages: %v", err)
		}
	}
}
