package db

import (
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkMessageTokenUsageUpdate guards against re-tokenizing unchanged text
// for late usage receipts. Body size grows while each operation updates eight
// messages. Reset writes run outside the timer and the rows/op count.
func BenchmarkMessageTokenUsageUpdate(b *testing.B) {
	for _, size := range []int{1 << 10, 32 << 10, 256 << 10} {
		b.Run(fmt.Sprintf("body=%dKiB", size>>10), func(b *testing.B) {
			const count = 8
			d := testDB(b)
			seedBenchSession(b, d, "bench-usage", 0)
			var body strings.Builder
			for i := 0; body.Len() < size; i++ {
				fmt.Fprintf(&body, "word%08d ", i)
			}
			messages := benchSessionMessages("bench-usage", count)
			update := IncrementalSessionUpdate{MsgCount: count, NextOrdinal: count}
			for i := range messages {
				messages[i].Role = "assistant"
				messages[i].Content = body.String()[:size]
				messages[i].ContentLength = size
				messages[i].TokenUsage = nil
				update.MessageTokenUsageUpdates = append(update.MessageTokenUsageUpdates, MessageTokenUsageUpdate{
					Ordinal: i, TokenUsage: jsontext.Value(`{"output_tokens":7}`),
					OutputTokens: 7, HasOutputTokens: true,
				})
			}
			require.NoError(b, d.InsertMessages(b.Context(), messages))
			var writes int64
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				_, err := d.getWriter().Exec(b.Context(), `UPDATE messages
SET token_usage = '', output_tokens = 0, has_output_tokens = 0 WHERE session_id = 'bench-usage'`)
				require.NoError(b, err)
				var before, after int64
				require.NoError(b, d.getWriter().QueryRow(b.Context(), "SELECT total_changes()").Scan(&before))
				b.StartTimer()
				_, err = d.WriteSessionIncremental(b.Context(), "bench-usage", nil, update)
				b.StopTimer()
				require.NoError(b, err)
				require.NoError(b, d.getWriter().QueryRow(b.Context(), "SELECT total_changes()").Scan(&after))
				writes += after - before
			}
			b.ReportMetric(float64(writes)/float64(b.N), "rows/op")
		})
	}
}

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
			m.TokenUsage = jsontext.Value(
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
	if err := d.UpsertSession(b.Context(), Session{
		ID:      sessionID,
		Project: "bench",
		Machine: "local",
		Agent:   "claude",
	}); err != nil {
		require.NoErrorf(b, err, "seed session %s", sessionID)
	}
	msgs := benchSessionMessages(sessionID, n)
	if err := d.InsertMessages(b.Context(), msgs); err != nil {
		require.NoErrorf(b, err, "seed messages for %s", sessionID)
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
		if err := d.ReplaceSessionMessages(b.Context(), "bench-replace", msgs); err != nil {
			require.NoError(b, err, "replace")
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
		if err := d.UpsertSession(b.Context(), Session{
			ID:      sid,
			Project: "bench",
			Machine: "local",
			Agent:   "claude",
		}); err != nil {
			require.NoError(b, err, "upsert session")
		}
		for j := range msgs {
			msgs[j].SessionID = sid
		}
		if err := d.InsertMessages(b.Context(), msgs); err != nil {
			require.NoError(b, err, "insert messages")
		}
	}
}
