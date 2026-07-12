package sync

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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
//     a fresh archive through the default per-session write path.
//   - BenchmarkResyncBulkIngest: the same archive through the
//     resync bulk-write pipeline (writeBatchBulk /
//     DB.WriteSessionBatch, the #411 regression class).
//
// Fixture sizes scale via AGENTSVIEW_BENCH_SYNC_SESSIONS and
// AGENTSVIEW_BENCH_SYNC_MESSAGES for larger local runs.

const (
	defaultBenchSyncSessions = 40
	defaultBenchSyncMessages = 30
	benchLargeSessionLines   = 1000
)

// silenceBenchLogs discards the engine's global log output for the
// duration of the benchmark. Log lines interleave with `go test
// -bench` result lines on stdout, which corrupts them so benchfmt
// cannot parse the benchmark — benchgate fails on such corruption,
// and before it did, the corrupted benchmarks silently vanished
// from the gate on both sides.
func silenceBenchLogs(b *testing.B) {
	b.Helper()
	prev := log.Writer()
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(prev) })
}

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
	silenceBenchLogs(b)
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
				"warm no-op sync re-synced %d sessions", stats.Synced,
			)
		}
		if writes := engine.PhaseStats().BatchedWrites.Load(); writes != 0 {
			b.Fatalf(
				"warm no-op sync bulk-wrote %d sessions", writes,
			)
		}
	}
}

// BenchmarkSyncPathsIncrementalAppend measures absorbing a single
// appended JSONL line into a session that already stores
// benchLargeSessionLines messages, the streaming write the serve
// daemon performs thousands of times per day.
//
// The session grows by one message per iteration, so per-op cost is
// only comparable between runs with the same iteration count: the
// bench gate always runs with a fixed -benchtime=Nx (see bench.yml
// and the Makefile) so baseline and candidate absorb appends into
// identically sized sessions. Growth is deliberate — per-append cost
// staying flat as the session grows is exactly the invariant this
// benchmark protects.
func BenchmarkSyncPathsIncrementalAppend(b *testing.B) {
	silenceBenchLogs(b)
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

	// The first append after a full sync triggers the leading-edge
	// inline signal recompute — an O(stored history) message reload
	// plus secret scan whose one-time cost would otherwise dominate
	// the fixed 20-iteration measurement and mask steady-state
	// per-append regressions. Absorb it before the timer starts so
	// the timed loop measures the debounced steady state.
	warmup := testjsonl.NewSessionBuilder().AddClaudeUser(
		"2026-06-20T11:00:00Z", "warm-up append",
	).String()
	if _, err := f.WriteString(warmup); err != nil {
		b.Fatalf("warm-up append: %v", err)
	}
	engine.SyncPaths([]string{path})

	// Stretch the debounce window so the flush timer cannot fire
	// inside the timed loop on a slow run: allocs/op is gated on the
	// candidate's worst -count run, and one timer-driven O(history)
	// recompute landing mid-loop would flake the gate. Engine.Close
	// drains the deferred recompute during cleanup.
	engine.signalSched.mu.Lock()
	engine.signalSched.interval = time.Hour
	engine.signalSched.quiet = time.Hour
	engine.signalSched.mu.Unlock()

	// Pre-build the appended lines: constructing Claude JSONL via
	// testjsonl allocates (map + json.Marshal), and inside the timed
	// loop that helper cost would be gated as if it were sync work.
	lines := make([]string, b.N)
	for i := range lines {
		lines[i] = testjsonl.NewSessionBuilder().AddClaudeUser(
			"2026-06-20T11:00:00Z",
			fmt.Sprintf("streamed line %d", i),
		).String()
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, err := f.WriteString(lines[i]); err != nil {
			b.Fatalf("append: %v", err)
		}
		engine.SyncPaths([]string{path})
	}
	b.StopTimer()

	msgs, err := database.GetAllMessages(ctx, "bench-0000")
	if err != nil {
		b.Fatalf("GetAllMessages: %v", err)
	}
	want := benchLargeSessionLines + 1 + b.N // +1 for the warm-up append
	if len(msgs) < want {
		b.Fatalf(
			"appends were not absorbed: stored %d messages, want >= %d",
			len(msgs), want,
		)
	}
}

// benchColdArchive is the shared cold-ingest loop: each iteration
// syncs the same archive into a fresh database via syncOnce, then
// verify checks the iteration's outcome with the timer stopped.
func benchColdArchive(
	b *testing.B,
	syncOnce func(*Engine) SyncStats,
	verify func(*Engine, SyncStats, int),
) {
	b.Helper()
	silenceBenchLogs(b)
	sessions := benchIntFromEnv(
		"AGENTSVIEW_BENCH_SYNC_SESSIONS", defaultBenchSyncSessions,
	)
	perSession := benchIntFromEnv(
		"AGENTSVIEW_BENCH_SYNC_MESSAGES", defaultBenchSyncMessages,
	)
	dir := b.TempDir()
	writeBenchClaudeArchive(b, dir, sessions, perSession)
	dbDir := b.TempDir()

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		b.StopTimer()
		dbPath := filepath.Join(dbDir, fmt.Sprintf("cold-%d.db", i))
		database, err := db.Open(dbPath)
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

		stats := syncOnce(engine)

		b.StopTimer()
		verify(engine, stats, sessions)
		engine.Close()
		if err := database.Close(); err != nil {
			b.Fatalf("close bench db: %v", err)
		}
		// Drop this iteration's DB (and WAL/SHM sidecars) so disk
		// usage stays O(1) instead of retaining b.N populated
		// databases until the function returns.
		stale, err := filepath.Glob(dbPath + "*")
		if err != nil {
			b.Fatalf("glob %s: %v", dbPath, err)
		}
		for _, p := range stale {
			if err := os.Remove(p); err != nil {
				b.Fatalf("remove %s: %v", p, err)
			}
		}
		b.StartTimer()
	}
}

// BenchmarkSyncAllColdArchive measures first-sync ingest throughput
// through the public SyncAll path: parse plus the default
// per-session writes a user's first sync performs.
func BenchmarkSyncAllColdArchive(b *testing.B) {
	ctx := context.Background()
	benchColdArchive(b,
		func(engine *Engine) SyncStats {
			return engine.SyncAll(ctx, nil)
		},
		func(_ *Engine, stats SyncStats, sessions int) {
			if stats.Synced != sessions {
				b.Fatalf(
					"cold sync stored %d of %d sessions (failed=%d)",
					stats.Synced, sessions, stats.Failed,
				)
			}
		},
	)
}

// BenchmarkResyncBulkIngest measures the bulk-write ingest path a
// full resync uses: syncWriteBulk routes every parsed session
// through writeBatchBulk and DB.WriteSessionBatch — the #411
// regression class. This is a different write path from the default
// per-session writes BenchmarkSyncAllColdArchive covers, and it
// self-asserts that every session really went through the batch
// pipeline so the benchmark cannot silently measure the wrong path.
func BenchmarkResyncBulkIngest(b *testing.B) {
	ctx := context.Background()
	benchColdArchive(b,
		func(engine *Engine) SyncStats {
			engine.syncMu.Lock()
			defer engine.syncMu.Unlock()
			return engine.syncAllLocked(
				ctx, nil, time.Time{}, nil, syncWriteBulk, true, false,
			)
		},
		func(engine *Engine, stats SyncStats, sessions int) {
			if stats.Synced != sessions {
				b.Fatalf(
					"bulk ingest stored %d of %d sessions (failed=%d)",
					stats.Synced, sessions, stats.Failed,
				)
			}
			writes := engine.PhaseStats().BatchedWrites.Load()
			if writes != int64(sessions) {
				b.Fatalf(
					"bulk ingest wrote %d of %d sessions via the batch pipeline",
					writes, sessions,
				)
			}
		},
	)
}

// BenchmarkResyncBulkContributorIngest measures the same cold archive shape
// when it enters the atomic rebuild through a contributor engine.
func BenchmarkResyncBulkContributorIngest(b *testing.B) {
	ctx := context.Background()
	benchColdArchive(b,
		func(engine *Engine) SyncStats {
			dir := engine.agentDirs[parser.AgentClaude][0]
			engine.agentDirs = nil
			stats, err := engine.ResyncAllWithOptions(ctx, nil, RebuildOptions{
				Contributors: []RebuildContributor{{
					Name: "benchmark-contributor",
					Config: EngineConfig{
						AgentDirs: map[parser.AgentType][]string{
							parser.AgentClaude: {dir},
						},
						Machine:   "benchmark-contributor",
						IDPrefix:  "benchmark~",
						Ephemeral: true,
					},
				}},
			})
			if err != nil {
				b.Fatalf("contributor rebuild: %v", err)
			}
			return stats
		},
		func(_ *Engine, stats SyncStats, sessions int) {
			if stats.Synced != sessions {
				b.Fatalf(
					"contributor ingest stored %d of %d sessions (failed=%d)",
					stats.Synced, sessions, stats.Failed,
				)
			}
			if len(stats.RebuildPhases) != 2 {
				b.Fatalf("contributor ingest recorded %d rebuild phases", len(stats.RebuildPhases))
			}
			writes := stats.RebuildPhases[1].BatchedWrites
			if writes != int64(sessions) {
				b.Fatalf(
					"contributor ingest wrote %d of %d sessions via the batch pipeline",
					writes, sessions,
				)
			}
		},
	)
}
