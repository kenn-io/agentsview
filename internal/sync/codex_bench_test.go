package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	codexSyncBenchmarkUUID       = "019eb791-cf7d-75c1-8439-9ed74c122b01"
	codexSyncBenchmarkPriorTurns = 1500
	codexSyncBenchmarkTailUser   = "Tail request: measure the remaining source reads."
	codexSyncBenchmarkTailAgent  = "Tail response: the appended records were parsed."
)

var (
	codexSyncBenchmarkOutcomeSink parser.IncrementalOutcome
	codexSyncBenchmarkHashSink    string
)

// BenchmarkCodexCheckpointAppendResume measures the source-reading pipeline a
// checkpoint-resumed Codex append performs: the checkpoint gate (stat +
// anchor digest + fingerprint resume), the seeded tail parse, the committed
// prefix resume hash, the next anchor digest, and the next checkpoint
// assembly. It replaces the pre-checkpoint benchmark that timed the old
// full-source fingerprint plus prefix re-hash pipeline.
func BenchmarkCodexCheckpointAppendResume(b *testing.B) {
	silenceBenchLogs(b)
	ctx := context.Background()
	root, path, prefix, tail, startOrdinal := writeCodexSyncBenchmarkTranscript(b)

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
	sessionID := "codex:" + codexSyncBenchmarkUUID
	cp, ok, err := database.GetParserCheckpoint(sessionID)
	require.NoError(b, err)
	require.True(b, ok, "the full sync must persist a checkpoint")
	blobs, ok, err := database.GetParserCheckpointBlobs(sessionID)
	require.NoError(b, err)
	require.True(b, ok)

	cfg := parser.ProviderConfig{
		Roots:   []string{root},
		Machine: "benchmark-host",
	}
	provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
	require.True(b, ok)
	source, found, err := provider.FindSource(ctx, parser.FindSourceRequest{
		FullSessionID: sessionID,
	})
	require.NoError(b, err)
	require.True(b, found)

	appendCodexSyncBenchmarkTail(b, path, tail)

	// Warm the timed pipeline once so the per-op loop measures the
	// checkpoint resume work itself.
	_, err = runCodexCheckpointAppendReads(
		ctx, engine, provider, source, path, cp, blobs,
		startOrdinal, len(prefix), len(tail),
	)
	require.NoError(b, err)

	// Per append the source reads are bounded: the anchor window (twice)
	// plus the tail (fingerprint resume, parse, committed-prefix resume).
	b.SetBytes(int64(len(tail)) + 2*codexCheckpointAnchorSize)
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for b.Loop() {
		outcome, err := runCodexCheckpointAppendReads(
			ctx, engine, provider, source, path, cp, blobs,
			startOrdinal, len(prefix), len(tail),
		)
		if err != nil {
			b.StopTimer()
			require.NoError(b, err)
			b.StartTimer()
		}
		codexSyncBenchmarkOutcomeSink = outcome
	}
}

func runCodexCheckpointAppendReads(
	ctx context.Context,
	engine *Engine,
	provider parser.Provider,
	source parser.SourceRef,
	path string,
	cp *db.ParserCheckpoint,
	blobs db.ParserCheckpointBlobs,
	startOrdinal int,
	prefixLen, tailLen int,
) (parser.IncrementalOutcome, error) {
	file := parser.DiscoveredFile{
		Path:  path,
		Agent: parser.AgentCodex,
	}
	res, err := engine.codexCheckpointFingerprint(ctx, source, file)
	if err != nil {
		return parser.IncrementalOutcome{}, err
	}
	if res.decision != codexCheckpointAppend {
		return parser.IncrementalOutcome{}, fmt.Errorf(
			"checkpoint gate decided %d, want append", res.decision,
		)
	}
	outcome, status, err := provider.ParseIncremental(ctx, parser.IncrementalRequest{
		Source:       source,
		Fingerprint:  res.fingerprint,
		SessionID:    "codex:" + codexSyncBenchmarkUUID,
		Offset:       cp.Offset,
		StartOrdinal: startOrdinal,
		Seed:         blobs.Cursor,
	})
	if err != nil {
		return outcome, err
	}
	if status != parser.IncrementalApplied {
		return outcome, fmt.Errorf("incremental status %v, want applied", status)
	}
	if outcome.ConsumedBytes != int64(tailLen) {
		return outcome, fmt.Errorf(
			"consumed %d, want %d", outcome.ConsumedBytes, tailLen,
		)
	}
	// The engine's remaining checkpoint work for the append.
	state, hash, err := codexResumeHash(path, cp.Offset, cp.Offset+outcome.ConsumedBytes, blobs.HashState)
	if err != nil {
		return outcome, err
	}
	anchorDigest, err := codexCheckpointAnchorDigest(
		path, cp.Offset+outcome.ConsumedBytes,
	)
	if err != nil {
		return outcome, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return outcome, err
	}
	built, _ := buildCodexCheckpoint(
		"codex:"+codexSyncBenchmarkUUID,
		"codex",
		path,
		info,
		cp.Offset+outcome.ConsumedBytes,
		info.ModTime().UnixNano(),
		outcome.NextCursor,
		state,
		hash,
		startOrdinal+2,
		anchorDigest,
	)
	codexSyncBenchmarkHashSink = built.Hash
	return outcome, nil
}

func writeCodexSyncBenchmarkTranscript(
	b *testing.B,
) (root, path, prefix, tail string, startOrdinal int) {
	b.Helper()
	root = filepath.Join(b.TempDir(), "sessions")
	path = filepath.Join(
		root,
		"2026",
		"07",
		"10",
		"rollout-2026-07-10T07-12-15-"+codexSyncBenchmarkUUID+".jsonl",
	)
	require.NoError(b, os.MkdirAll(filepath.Dir(path), 0o755))

	fixture := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			"2026-07-10T07:00:00Z",
			codexSyncBenchmarkUUID,
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
	for i := range codexSyncBenchmarkPriorTurns {
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
	prefix = fixture.String()
	startOrdinal = 2 + 2*codexSyncBenchmarkPriorTurns
	tail = testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON(
			"user", codexSyncBenchmarkTailUser, "2026-07-10T07:12:15Z",
		),
		testjsonl.CodexMsgJSON(
			"assistant", codexSyncBenchmarkTailAgent, "2026-07-10T07:12:16Z",
		),
	)
	require.NoError(b, os.WriteFile(path, []byte(prefix), 0o644))
	return root, path, prefix, tail, startOrdinal
}

func appendCodexSyncBenchmarkTail(b *testing.B, path, tail string) {
	b.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(b, err)
	_, err = f.WriteString(tail)
	require.NoError(b, err)
	require.NoError(b, f.Close())
}

const (
	codexLateToolBenchmarkUUID  = "019eb791-cf7d-75c1-8439-9ed74c122b02"
	codexLateToolBenchmarkTurns = 250
)

// BenchmarkCodexLateToolOutputDebouncedBurst measures absorbing a stream in
// which each batch appends a new function_call plus the function_call_output
// for the previous batch's call. Output records therefore always refer to a
// call committed in an earlier sync batch.
//
// This is a debounced-burst benchmark, not a single-quiet-append gate: the
// engine's signal scheduler is stretched to an hour so the O(history)
// full recompute runs only on the first iteration and the remaining
// iterations are amortized. It guards the late-result update path's
// per-append cost, but it deliberately does not measure the quiet-session
// first append (see BenchmarkCodexQuietAppendSignals* for that gate). The
// session grows by two records per iteration, so per-op cost is only
// comparable between runs with the same iteration count (the bench gate
// always runs with a fixed -benchtime=Nx).
func BenchmarkCodexLateToolOutputDebouncedBurst(b *testing.B) {
	silenceBenchLogs(b)
	ctx := context.Background()
	root, path, _ := writeCodexLateToolBenchmarkTranscript(b)

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

	// Stretch the debounce window so the flush timer cannot fire inside
	// the timed loop (same rationale as BenchmarkSyncPathsIncrementalAppend).
	engine.signalSched.mu.Lock()
	engine.signalSched.interval = time.Hour
	engine.signalSched.quiet = time.Hour
	engine.signalSched.mu.Unlock()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(b, err)
	defer f.Close()

	// Pre-build the appended lines: constructing JSONL inside the timed loop
	// would allocate and be gated as if it were sync work. Iteration i appends
	// call_{i+1} and the output for call_i, so the output is always "late".
	lines := make([]string, b.N)
	for i := range lines {
		lines[i] = testjsonl.JoinJSONL(
			testjsonl.CodexFunctionCallWithCallIDJSON(
				"exec_command",
				codexLateToolBenchmarkCall(i+1),
				nil,
				codexLateToolBenchmarkTS(2*i+1),
			),
			testjsonl.CodexFunctionCallOutputJSON(
				codexLateToolBenchmarkCall(i),
				"result "+strconv.Itoa(i),
				codexLateToolBenchmarkTS(2*i+2),
			),
		)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, err := f.WriteString(lines[i]); err != nil {
			b.Fatalf("append: %v", err)
		}
		stats := engine.SyncAll(ctx, nil)
		if stats.Failed != 0 {
			b.Fatalf("sync failed for appended output: %+v", stats)
		}
	}
	b.StopTimer()

	msgs, err := database.GetAllMessages(ctx, "codex:"+codexLateToolBenchmarkUUID)
	require.NoError(b, err)
	wantMsgs := 3 + 2*codexLateToolBenchmarkTurns + b.N
	require.Len(b, msgs, wantMsgs,
		"each appended call adds exactly one message row")
	results := make(map[string]db.ToolCall, len(msgs))
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			results[msgs[i].ToolCalls[j].ToolUseID] = msgs[i].ToolCalls[j]
		}
	}
	for i := range b.N {
		call, ok := results[codexLateToolBenchmarkCall(i)]
		require.True(b, ok, "call %d must be stored", i)
		assert.Equal(b, "exec_command", call.ToolName)
		assert.Equal(b, "result "+strconv.Itoa(i), call.ResultContent)
		require.Len(b, call.ResultEvents, 1,
			"each call must carry exactly its own output event")
		assert.Equal(b, "result "+strconv.Itoa(i), call.ResultEvents[0].Content)
	}
	newest, ok := results[codexLateToolBenchmarkCall(b.N)]
	require.True(b, ok, "the newest call must be stored")
	assert.Empty(b, newest.ResultContent)
	assert.Empty(b, newest.ResultEvents)
}

func codexLateToolBenchmarkCall(i int) string {
	return "call_" + strconv.Itoa(i)
}

func codexLateToolBenchmarkTS(i int) string {
	return time.Date(
		2026, 7, 10, 7, 12, i, 0, time.UTC,
	).Format("2006-01-02T15:04:05Z")
}

func writeCodexLateToolBenchmarkTranscript(
	b testing.TB,
) (root, path, prefix string) {
	b.Helper()
	root = filepath.Join(b.TempDir(), "sessions")
	path = filepath.Join(
		root,
		"2026",
		"07",
		"10",
		"rollout-2026-07-10T07-12-15-"+codexLateToolBenchmarkUUID+".jsonl",
	)
	require.NoError(b, os.MkdirAll(filepath.Dir(path), 0o755))

	fixture := testjsonl.NewSessionBuilder().
		AddCodexMeta(
			"2026-07-10T07:00:00Z",
			codexLateToolBenchmarkUUID,
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
	for i := range codexLateToolBenchmarkTurns {
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
	// call_0 is committed in the prefix with no output; iteration 0 appends
	// call_1 plus the late output for call_0.
	fixture.AddRaw(testjsonl.CodexFunctionCallWithCallIDJSON(
		"exec_command",
		"call_0",
		nil,
		codexLateToolBenchmarkTS(0),
	))
	prefix = fixture.String()
	require.NoError(b, os.WriteFile(path, []byte(prefix), 0o644))
	return root, path, prefix
}
