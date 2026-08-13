package sync

import (
	"context"
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

// BenchmarkCodexIncrementalSyncReads measures the source-reading pipeline that
// remains around a warm cursor append: Fingerprint hashes the full source and
// ComputeFileHashPrefix hashes through the proposed committed offset.
func BenchmarkCodexIncrementalSyncReads(b *testing.B) {
	b.StopTimer()
	ctx := context.Background()
	root, path, prefix, tail, startOrdinal := writeCodexSyncBenchmarkTranscript(b)
	cfg := parser.ProviderConfig{
		Roots:   []string{root},
		Machine: "benchmark-host",
	}
	provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
	require.True(b, ok)
	source, found, err := provider.FindSource(ctx, parser.FindSourceRequest{
		FullSessionID: "codex:" + codexSyncBenchmarkUUID,
	})
	require.NoError(b, err)
	require.True(b, found)

	prefixFingerprint, err := provider.Fingerprint(ctx, source)
	require.NoError(b, err)
	assert.Equal(b, int64(len(prefix)), prefixFingerprint.Size)
	full, err := provider.Parse(ctx, parser.ParseRequest{
		Source:      source,
		Fingerprint: prefixFingerprint,
	})
	require.NoError(b, err)
	require.Len(b, full.Results, 1)
	assert.Len(b, full.Results[0].Result.Messages, startOrdinal)

	// Keep the timed provider untouched after its prefix-only full parse. A
	// separately seeded provider handles output validation after the append.
	validationProvider, ok := parser.NewProvider(parser.AgentCodex, cfg)
	require.True(b, ok)
	_, err = validationProvider.Parse(ctx, parser.ParseRequest{
		Source:      source,
		Fingerprint: prefixFingerprint,
	})
	require.NoError(b, err)

	appendCodexSyncBenchmarkTail(b, path, tail)
	req := parser.IncrementalRequest{
		Source:       source,
		SessionID:    "codex:" + codexSyncBenchmarkUUID,
		Offset:       int64(len(prefix)),
		StartOrdinal: startOrdinal,
	}

	currentFingerprint, outcome, status, committedHash, err :=
		runCodexIncrementalSyncReads(ctx, validationProvider, source, path, req)
	require.NoError(b, err)
	requireCodexSyncBenchmarkOutcome(
		b, outcome, status, startOrdinal, int64(len(tail)),
	)
	require.Equal(b, int64(len(prefix)+len(tail)), currentFingerprint.Size)
	require.Equal(b, currentFingerprint.Size, req.Offset+outcome.ConsumedBytes)
	require.NotEmpty(b, currentFingerprint.Hash)
	require.Len(b, committedHash, 64)
	require.Equal(b, currentFingerprint.Hash, committedHash)

	// Two full-length linear reads dominate this pipeline: the provider source
	// fingerprint and the engine's committed-prefix hash. The warm tail parse is
	// intentionally left in the same measurement between them.
	b.SetBytes(2 * int64(len(prefix)+len(tail)))
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for b.Loop() {
		fingerprint, outcome, status, hash, err := runCodexIncrementalSyncReads(
			ctx, provider, source, path, req,
		)
		if err != nil || !codexSyncBenchmarkOutcomeValid(
			outcome, status, startOrdinal, int64(len(tail)),
		) || fingerprint.Size != int64(len(prefix)+len(tail)) ||
			fingerprint.Hash == "" || len(hash) != 64 || fingerprint.Hash != hash {
			b.StopTimer()
			require.NoError(b, err)
			requireCodexSyncBenchmarkOutcome(
				b, outcome, status, startOrdinal, int64(len(tail)),
			)
			assert.Equal(b, int64(len(prefix)+len(tail)), fingerprint.Size)
			require.NotEmpty(b, fingerprint.Hash)
			assert.Len(b, hash, 64)
			assert.Equal(b, fingerprint.Hash, hash)
			b.StartTimer()
		}
		codexSyncBenchmarkOutcomeSink = outcome
		codexSyncBenchmarkHashSink = hash
	}
}

func runCodexIncrementalSyncReads(
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
	path string,
	req parser.IncrementalRequest,
) (
	parser.SourceFingerprint,
	parser.IncrementalOutcome,
	parser.IncrementalStatus,
	string,
	error,
) {
	// Fingerprint retains the existing full-source content hash.
	fingerprint, err := provider.Fingerprint(ctx, source)
	if err != nil {
		return parser.SourceFingerprint{}, parser.IncrementalOutcome{},
			parser.IncrementalUnsupported, "", err
	}
	req.Fingerprint = fingerprint
	outcome, status, err := provider.ParseIncremental(ctx, req)
	if err != nil {
		return fingerprint, outcome, status, "", err
	}
	// This is the engine's second remaining linear read, through the offset
	// that would be committed after the incremental database write succeeds.
	committedHash, err := ComputeFileHashPrefix(
		path, req.Offset+outcome.ConsumedBytes,
	)
	return fingerprint, outcome, status, committedHash, err
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

func requireCodexSyncBenchmarkOutcome(
	b *testing.B,
	outcome parser.IncrementalOutcome,
	status parser.IncrementalStatus,
	startOrdinal int,
	tailBytes int64,
) {
	b.Helper()
	require.Equal(b, parser.IncrementalApplied, status)
	require.Len(b, outcome.Messages, 2)
	assert.Equal(b, "codex:"+codexSyncBenchmarkUUID, outcome.SessionID)
	assert.Equal(b, 2, outcome.MessageCount)
	assert.Equal(b, 1, outcome.UserMessageCount)
	assert.Equal(b, tailBytes, outcome.ConsumedBytes)
	assert.Equal(b, parser.RoleUser, outcome.Messages[0].Role)
	assert.Equal(b, codexSyncBenchmarkTailUser, outcome.Messages[0].Content)
	assert.Equal(b, startOrdinal, outcome.Messages[0].Ordinal)
	assert.Equal(b, parser.RoleAssistant, outcome.Messages[1].Role)
	assert.Equal(b, codexSyncBenchmarkTailAgent, outcome.Messages[1].Content)
	assert.Equal(b, startOrdinal+1, outcome.Messages[1].Ordinal)
}

func codexSyncBenchmarkOutcomeValid(
	outcome parser.IncrementalOutcome,
	status parser.IncrementalStatus,
	startOrdinal int,
	tailBytes int64,
) bool {
	return status == parser.IncrementalApplied &&
		outcome.SessionID == "codex:"+codexSyncBenchmarkUUID &&
		outcome.MessageCount == 2 &&
		outcome.UserMessageCount == 1 &&
		outcome.ConsumedBytes == tailBytes &&
		len(outcome.Messages) == 2 &&
		outcome.Messages[0].Role == parser.RoleUser &&
		outcome.Messages[0].Content == codexSyncBenchmarkTailUser &&
		outcome.Messages[0].Ordinal == startOrdinal &&
		outcome.Messages[1].Role == parser.RoleAssistant &&
		outcome.Messages[1].Content == codexSyncBenchmarkTailAgent &&
		outcome.Messages[1].Ordinal == startOrdinal+1
}

const (
	codexLateToolBenchmarkUUID  = "019eb791-cf7d-75c1-8439-9ed74c122b02"
	codexLateToolBenchmarkTurns = 250
)

// BenchmarkCodexIncrementalLateToolOutput measures absorbing a stream in
// which each batch appends a new function_call plus the function_call_output
// for the previous batch's call. Output records therefore always refer to a
// call committed in an earlier sync batch.
//
// Before the incremental tool-result path, every such output forced an
// authoritative full reparse of the whole transcript, so per-op cost scaled
// with stored history; the incremental path attaches each event to the
// already-stored call and keeps per-op cost bounded by the
// fingerprint/prefix-hash reads plus the tiny tail. The session grows by two
// records per iteration, so per-op cost is only comparable between runs with
// the same iteration count (the bench gate always runs with a fixed
// -benchtime=Nx).
func BenchmarkCodexIncrementalLateToolOutput(b *testing.B) {
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
