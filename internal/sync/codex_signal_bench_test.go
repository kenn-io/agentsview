package sync

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const codexSignalBenchmarkUUID = "019eb791-cf7d-75c1-8439-9ed74c122c01"

// BenchmarkCodexQuietAppendSignals500/5000/15000 are the non-amortized
// quiet-session append gate: every iteration performs one engine-level
// append (a new function_call plus the previous call's late output) with
// signals and secret findings fully maintained inline — the debounce
// scheduler is disabled, so no iteration amortizes the recompute against
// the others. The three sizes prove per-append latency does not scale with
// stored history. Each benchmark self-asserts zero GetAllMessages calls
// during the timed loop.
func BenchmarkCodexQuietAppendSignals500(b *testing.B) {
	benchCodexQuietAppendSignals(b, 500)
}

func BenchmarkCodexQuietAppendSignals5000(b *testing.B) {
	benchCodexQuietAppendSignals(b, 5000)
}

func BenchmarkCodexQuietAppendSignals15000(b *testing.B) {
	benchCodexQuietAppendSignals(b, 15000)
}

func benchCodexQuietAppendSignals(b *testing.B, turns int) {
	silenceBenchLogs(b)
	ctx := context.Background()
	root, path, uuid := writeCodexSignalBenchmarkTranscript(b, turns)

	database, err := db.Open(filepath.Join(b.TempDir(), "bench.db"))
	require.NoError(b, err)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "benchmark-host",
	})
	b.Cleanup(func() {
		engine.Close()
		if err := database.Close(); err != nil {
			b.Errorf("close bench db: %v", err)
		}
	})
	first := engine.SyncAll(ctx, nil)
	require.Equal(b, 1, first.Synced)
	require.Zero(b, first.Failed)

	// Disable the debounce so every append pays the full inline
	// signals+secrets maintenance; nothing is amortized across iterations.
	engine.signalSched.mu.Lock()
	engine.signalSched.interval = 0
	engine.signalSched.quiet = 0
	engine.signalSched.mu.Unlock()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(b, err)
	defer f.Close()

	// Pre-build the appended lines: constructing JSONL inside the timed
	// loop would allocate and be gated as if it were sync work. Iteration i
	// appends call_{i+1} and the output for call_i, so the output is
	// always "late".
	lines := make([]string, b.N)
	for i := range lines {
		lines[i] = testjsonl.JoinJSONL(
			testjsonl.CodexFunctionCallWithCallIDJSON(
				"exec_command",
				codexSignalBenchmarkCall(i+1),
				nil,
				codexSignalBenchmarkTS(2*i+1),
			),
			testjsonl.CodexFunctionCallOutputJSON(
				codexSignalBenchmarkCall(i),
				"result "+strconv.Itoa(i),
				codexSignalBenchmarkTS(2*i+2),
			),
		)
	}

	loadsBefore := database.MessagesLoadCount()
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, err := f.WriteString(lines[i]); err != nil {
			b.Fatalf("append: %v", err)
		}
		stats := engine.SyncAll(ctx, nil)
		if stats.Failed != 0 || stats.Synced != 1 {
			b.Fatalf("sync failed for appended output: %+v", stats)
		}
	}
	b.StopTimer()
	require.Equal(b, loadsBefore, database.MessagesLoadCount(),
		"the maintained quiet append must never call GetAllMessages")

	sess, err := database.GetSessionFull(
		ctx, "codex:"+uuid,
	)
	require.NoError(b, err)
	require.NotNil(b, sess)
	require.Equal(b, db.CurrentQualitySignalVersion,
		sess.QualitySignalVersion,
		"every append must leave the signal version current")
}

// BenchmarkCodexColdFullSync gates the cold full-parse pipeline: a fresh
// database and engine ingest the transcript from scratch, including the
// single-pass checkpoint capture. Per-op cost deliberately exceeds the
// usual micro-benchmark band; it exists to catch a regression that adds a
// source read pass (as the pre-fix checkpoint persistence did).
func BenchmarkCodexColdFullSync(b *testing.B) {
	silenceBenchLogs(b)
	ctx := context.Background()
	root, path, uuid := writeCodexSignalBenchmarkTranscript(b, 7500)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		database, err := db.Open(filepath.Join(b.TempDir(), "bench.db"))
		require.NoError(b, err)
		engine := NewEngine(database, EngineConfig{
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentCodex: {root},
			},
			Machine: "benchmark-host",
		})
		stats := engine.SyncAll(ctx, nil)
		if stats.Failed != 0 || stats.Synced != 1 {
			b.Fatalf("cold full sync failed: %+v", stats)
		}
		engine.Close()
		if err := database.Close(); err != nil {
			b.Fatalf("close bench db: %v", err)
		}
		_ = path
		_ = uuid
	}
}

func codexSignalBenchmarkCall(i int) string {
	return "call_" + strconv.Itoa(i)
}

func codexSignalBenchmarkTS(i int) string {
	return time.Date(
		2026, 7, 10, 7, 12, i, 0, time.UTC,
	).Format("2006-01-02T15:04:05Z")
}

// writeCodexSignalBenchmarkTranscript builds a Codex transcript with
// `turns` prior user/assistant turns plus an unanswered call_0, so each
// appended iteration can emit a call plus the previous call's late output.
func writeCodexSignalBenchmarkTranscript(
	b testing.TB, turns int,
) (root, path, uuid string) {
	b.Helper()
	uuid = codexSignalBenchmarkUUID
	root = filepath.Join(b.TempDir(), "sessions")
	path = filepath.Join(
		root,
		"2026",
		"07",
		"10",
		"rollout-2026-07-10T07-12-15-"+uuid+".jsonl",
	)
	require.NoError(b, os.MkdirAll(filepath.Dir(path), 0o755))

	fixture := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			"2026-07-10T07:00:00Z",
			uuid,
			"/workspace/project-a",
			"codex_cli_rs",
		).
		AddRaw(testjsonl.CodexTurnContextJSON(
			"gpt-5.4", "2026-07-10T07:00:01Z",
		)).
		AddCodexMessage(
			"2026-07-10T07:00:02Z",
			"user",
			"Initial request: inspect the project and make a careful change.",
		).
		AddCodexMessage(
			"2026-07-10T07:00:03Z",
			"assistant",
			"Initial response: I will inspect the relevant code and tests.",
		)
	contextPayload := strings.Repeat(
		"Retain concrete code, test, and validation context. ", 8,
	)
	for i := range turns {
		turn := strconv.Itoa(i)
		fixture.AddCodexMessage(
			"2026-07-10T07:01:00Z",
			"user",
			"Prior turn "+turn+" request: continue the implementation. "+
				contextPayload,
		)
		fixture.AddCodexMessage(
			"2026-07-10T07:01:01Z",
			"assistant",
			"Prior turn "+turn+" response: applied the next bounded change. "+
				contextPayload,
		)
	}
	// call_0 is committed in the prefix with no output; iteration 0
	// appends call_1 plus the late output for call_0.
	fixture.AddRaw(testjsonl.CodexFunctionCallWithCallIDJSON(
		"exec_command",
		"call_0",
		nil,
		codexSignalBenchmarkTS(0),
	))
	prefix := fixture.String()
	require.NoError(b, os.WriteFile(path, []byte(prefix), 0o644))
	return root, path, uuid
}
