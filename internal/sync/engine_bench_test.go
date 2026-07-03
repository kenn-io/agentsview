package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Hot-path benchmarks for the sync engine, covering the regression
// classes that have shipped before. CI's bench-gate workflow runs
// them on every PR and compares allocs/op, B/op, and ns/op against
// the merge base:
//
//   - BenchmarkSyncAllWarmNoop: a full sync over an already-synced,
//     unchanged archive must do stat+skip work only. Regressed when
//     the provider migration dropped pre-parse DB-freshness skips
//     and every full sync reparsed and rewrote unchanged sessions
//     (fixed by providerSourceUnchangedInDB), and when discovery
//     recomputed root-derived project info per source (#912).
//   - BenchmarkSyncPathsIncrementalAppend: absorbing one appended
//     line into a large session must scale with the appended data,
//     not the stored history (#954).
//   - BenchmarkSyncAllColdArchive: first-sync ingest throughput for
//     a fresh archive (the #411 bulk-write path).
//
// Fixture sizes scale via AGENTSVIEW_BENCH_SYNC_SESSIONS and
// AGENTSVIEW_BENCH_SYNC_MESSAGES for larger local runs.

const (
	defaultBenchSyncSessions = 40
	defaultBenchSyncMessages = 30
	benchLargeSessionLines   = 1000
)

func benchIntFromEnv(name string, def int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// writeBenchClaudeArchive lays out `sessions` Claude JSONL session
// files of `perSession` alternating user/assistant messages under
// dir/bench-project, mirroring the on-disk shape SyncAll discovers.
func writeBenchClaudeArchive(
	b *testing.B, dir string, sessions, perSession int,
) {
	b.Helper()
	proj := filepath.Join(dir, "bench-project")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		b.Fatalf("MkdirAll: %v", err)
	}
	for s := range sessions {
		builder := testjsonl.NewSessionBuilder()
		for m := 0; m < perSession; m += 2 {
			ts := fmt.Sprintf(
				"2026-06-20T10:%02d:%02dZ", (m/2/60)%60, (m/2)%60,
			)
			builder.AddClaudeUser(ts, fmt.Sprintf(
				"user message %d in session %d", m, s,
			))
			builder.AddClaudeAssistant(ts, fmt.Sprintf(
				"assistant reply %d in session %d", m, s,
			))
		}
		path := filepath.Join(
			proj, fmt.Sprintf("bench-%04d.jsonl", s),
		)
		if err := os.WriteFile(
			path, []byte(builder.String()), 0o644,
		); err != nil {
			b.Fatalf("WriteFile %s: %v", path, err)
		}
	}
}

// openBenchEngine opens a fresh SQLite DB and an engine watching dir
// as a Claude root. Cleanup closes the engine before the DB so any
// pending debounced signal recompute drains first.
func openBenchEngine(b *testing.B, dir string) (*Engine, *db.DB) {
	b.Helper()
	database, err := db.Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatalf("open bench db: %v", err)
	}
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {dir},
		},
		Machine: "local",
	})
	b.Cleanup(func() {
		engine.Close()
		if err := database.Close(); err != nil {
			b.Errorf("close bench db: %v", err)
		}
	})
	return engine, database
}

// BenchmarkSyncAllWarmNoop measures a full sync pass over an archive
// that is already fully synced and unchanged on disk: discovery plus
// per-source freshness skips. It also asserts the invariant the
// benchmark exists to protect — a warm no-op pass must not reparse
// or rewrite anything.
func BenchmarkSyncAllWarmNoop(b *testing.B) {
	sessions := benchIntFromEnv(
		"AGENTSVIEW_BENCH_SYNC_SESSIONS", defaultBenchSyncSessions,
	)
	perSession := benchIntFromEnv(
		"AGENTSVIEW_BENCH_SYNC_MESSAGES", defaultBenchSyncMessages,
	)
	dir := b.TempDir()
	writeBenchClaudeArchive(b, dir, sessions, perSession)
	engine, _ := openBenchEngine(b, dir)
	ctx := context.Background()

	first := engine.SyncAll(ctx, nil)
	if first.Synced != sessions {
		b.Fatalf(
			"initial sync stored %d of %d sessions (failed=%d)",
			first.Synced, sessions, first.Failed,
		)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		stats := engine.SyncAll(ctx, nil)
		if stats.Synced != 0 {
			b.Fatalf(
				"warm no-op sync reparsed %d sessions", stats.Synced,
			)
		}
	}
}

// BenchmarkSyncPathsIncrementalAppend measures absorbing a single
// appended JSONL line into a session that already stores
// benchLargeSessionLines messages, the streaming write the serve
// daemon performs thousands of times per day.
func BenchmarkSyncPathsIncrementalAppend(b *testing.B) {
	dir := b.TempDir()
	writeBenchClaudeArchive(b, dir, 1, benchLargeSessionLines)
	engine, database := openBenchEngine(b, dir)
	ctx := context.Background()

	first := engine.SyncAll(ctx, nil)
	if first.Synced != 1 {
		b.Fatalf(
			"initial sync stored %d sessions (failed=%d)",
			first.Synced, first.Failed,
		)
	}

	path := filepath.Join(dir, "bench-project", "bench-0000.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		b.Fatalf("OpenFile: %v", err)
	}
	defer f.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		line := testjsonl.NewSessionBuilder().AddClaudeUser(
			"2026-06-20T11:00:00Z",
			fmt.Sprintf("streamed line %d", i),
		).String()
		if _, err := f.WriteString(line); err != nil {
			b.Fatalf("append: %v", err)
		}
		engine.SyncPaths([]string{path})
	}
	b.StopTimer()

	msgs, err := database.GetAllMessages(ctx, "bench-0000")
	if err != nil {
		b.Fatalf("GetAllMessages: %v", err)
	}
	if len(msgs) < benchLargeSessionLines+b.N {
		b.Fatalf(
			"appends were not absorbed: stored %d messages, want >= %d",
			len(msgs), benchLargeSessionLines+b.N,
		)
	}
}

// BenchmarkSyncAllColdArchive measures first-sync ingest throughput:
// parse plus bulk write of a whole archive into a fresh database.
func BenchmarkSyncAllColdArchive(b *testing.B) {
	sessions := benchIntFromEnv(
		"AGENTSVIEW_BENCH_SYNC_SESSIONS", defaultBenchSyncSessions,
	)
	perSession := benchIntFromEnv(
		"AGENTSVIEW_BENCH_SYNC_MESSAGES", defaultBenchSyncMessages,
	)
	dir := b.TempDir()
	writeBenchClaudeArchive(b, dir, sessions, perSession)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		database, err := db.Open(
			filepath.Join(b.TempDir(), fmt.Sprintf("cold-%d.db", i)),
		)
		if err != nil {
			b.Fatalf("open bench db: %v", err)
		}
		engine := NewEngine(database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentClaude: {dir},
			},
			Machine: "local",
		})
		b.StartTimer()

		stats := engine.SyncAll(ctx, nil)

		b.StopTimer()
		if stats.Synced != sessions {
			b.Fatalf(
				"cold sync stored %d of %d sessions (failed=%d)",
				stats.Synced, sessions, stats.Failed,
			)
		}
		engine.Close()
		if err := database.Close(); err != nil {
			b.Fatalf("close bench db: %v", err)
		}
		b.StartTimer()
	}
}
