package db

import (
	"context"
	"fmt"
	"io"
	"log"
	"testing"
)

// Pre-change baseline for content-search page fetches. CI's bench-gate
// workflow will compare these against a candidate once per-match citation
// derivation lands on top of SearchContent (see the conversation-unit
// citations design), so a regression in the derivation cost shows up as an
// ns/op or allocs/op delta on a PR instead of shipping silently.
//
// The corpus is built once per benchmark (outside the timed loop) to stress
// exactly what citation derivation has to walk: long assistant monologues,
// system rows breaking up a run without ending it, and sidechain stretches
// that do end a run. See seedContentSearchBench for the exact shape.
const (
	benchContentSessions = 40
	benchContentMessages = 300
	// benchContentRunStart and benchContentRunEnd bound the one long
	// assistant run per session (inclusive ordinals).
	benchContentRunStart = 10
	benchContentRunEnd   = 260
	// benchContentSegment is both the system-row cadence inside the run and
	// the sidechain-stretch width: every 50th ordinal inside the run (not at
	// its start or end) is a system row, and the assistant messages between
	// consecutive system rows alternate is_sidechain, so the run contains
	// multiple contiguous sidechain stretches rather than one flat run.
	benchContentSegment = 50
)

// seedContentSearchBench builds a corpus that stresses the citation
// derivation: long assistant runs (the monologue case), sidechain
// stretches, system rows inside runs, and a term that matches broadly.
// 40 sessions x 300 messages; in each session ordinals 10..260 form one
// assistant run (with a system row every 50 ordinals inside it), the rest
// alternate user/assistant. Every assistant message contains "needle";
// IN-RUN assistant messages additionally contain "runneedle", so the
// rank-ordered FTS benchmark can pin its page to the long-run region
// instead of filling with outside-run hits.
func seedContentSearchBench(b *testing.B, d *DB) {
	b.Helper()
	origLog := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(origLog) })
	for i := range benchContentSessions {
		sessionID := fmt.Sprintf("bench-search-%03d", i)
		if err := d.UpsertSession(Session{
			ID: sessionID, Project: "bench", Machine: "local", Agent: "claude",
			// MessageCount > 0 so buildSessionFilter's base
			// "message_count > 0" predicate keeps the session; UserMessageCount
			// > 1 so the one-shot/automated exclusion in contentSessionFilter
			// does not drop it either.
			MessageCount:     benchContentMessages,
			UserMessageCount: 2,
		}); err != nil {
			b.Fatalf("seed session %s: %v", sessionID, err)
		}
		msgs := benchContentSearchMessages(sessionID)
		if err := d.InsertMessages(msgs); err != nil {
			b.Fatalf("seed messages for %s: %v", sessionID, err)
		}
	}
}

// benchContentSearchMessages builds the benchContentMessages-message
// timeline for one session per seedContentSearchBench's shape.
func benchContentSearchMessages(sessionID string) []Message {
	msgs := make([]Message, 0, benchContentMessages)
	for i := range benchContentMessages {
		msgs = append(msgs, benchContentSearchMessage(sessionID, i))
	}
	return msgs
}

// benchContentSearchMessage builds the single message at ordinal in
// sessionID: a system row or sidechain-tagged assistant turn inside the
// long run (benchContentRunStart..benchContentRunEnd), or an alternating
// user/assistant message outside it. Every assistant message contains
// "needle".
func benchContentSearchMessage(sessionID string, ordinal int) Message {
	ts := fmt.Sprintf("2026-06-%02dT10:00:00Z", 1+ordinal%28)
	if ordinal >= benchContentRunStart && ordinal <= benchContentRunEnd {
		return benchContentRunMessage(sessionID, ordinal, ts)
	}
	if ordinal%2 == 1 {
		content := fmt.Sprintf(
			"assistant reply %d contains needle outside the monologue run", ordinal,
		)
		return Message{
			SessionID: sessionID, Ordinal: ordinal, Role: "assistant",
			Content: content, Timestamp: ts, Model: "claude-bench-model",
			ContentLength: len(content),
		}
	}
	content := fmt.Sprintf("user message %d asking an unrelated question", ordinal)
	return Message{
		SessionID: sessionID, Ordinal: ordinal, Role: "user",
		Content: content, Timestamp: ts, ContentLength: len(content),
	}
}

// benchContentRunMessage builds one message inside the long assistant run:
// a system row on interior segment boundaries, otherwise an assistant
// message tagged is_sidechain for alternating benchContentSegment-wide
// stretches.
func benchContentRunMessage(sessionID string, ordinal int, ts string) Message {
	offset := ordinal - benchContentRunStart
	runLen := benchContentRunEnd - benchContentRunStart
	if offset > 0 && offset < runLen && offset%benchContentSegment == 0 {
		content := fmt.Sprintf(
			"system notice at ordinal %d inside the assistant run", ordinal,
		)
		return Message{
			SessionID: sessionID, Ordinal: ordinal, Role: "system",
			Content: content, Timestamp: ts, IsSystem: true,
			ContentLength: len(content),
		}
	}
	content := fmt.Sprintf(
		"assistant monologue turn %d contains needle and runneedle for the search benchmark",
		ordinal,
	)
	return Message{
		SessionID: sessionID, Ordinal: ordinal, Role: "assistant",
		Content: content, Timestamp: ts, Model: "claude-bench-model",
		IsSidechain:   (offset/benchContentSegment)%2 == 1,
		ContentLength: len(content),
	}
}

// BenchmarkSearchContentSubstringPage measures a full 50-hit substring
// content-search page over benchContentSessions sessions, each with a
// "needle" in every assistant message -- the same broad-match, worst-case
// shape citation derivation has to walk per match.
func BenchmarkSearchContentSubstringPage(b *testing.B) {
	d := testDB(b)
	seedContentSearchBench(b, d)
	f := ContentSearchFilter{Pattern: "needle", Limit: 50}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		page, err := d.SearchContent(context.Background(), f)
		if err != nil || len(page.Matches) != 50 {
			b.Fatalf("search: %v (%d matches, want a full 50-hit page)", err, len(page.Matches))
		}
	}
}

// BenchmarkSearchContentFTSPage is BenchmarkSearchContentSubstringPage's
// FTS-mode counterpart, over the identical corpus. It searches the run-only
// term "runneedle": FTS orders by rank, so a broad term would fill the
// 50-hit page with outside-run hits and never exercise the long-run
// derivation shape this benchmark exists to measure.
func BenchmarkSearchContentFTSPage(b *testing.B) {
	d := testDB(b)
	if !d.HasFTS() {
		b.Skip("fts5 not available")
	}
	seedContentSearchBench(b, d)
	f := ContentSearchFilter{Pattern: "runneedle", Mode: "fts", Limit: 50}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		page, err := d.SearchContent(context.Background(), f)
		if err != nil || len(page.Matches) != 50 {
			b.Fatalf("search: %v (%d matches, want a full 50-hit page)", err, len(page.Matches))
		}
	}
}
