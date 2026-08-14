//go:build macrobench

package sync

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Macro benchmarks for the 10MB-vs-1GB same-append ratio gate. They are
// excluded from the PR bench gate (build tag macrobench, and the gated
// packages run without it) because the 1GB fixture takes minutes per run.
// Procedure (documented in docs/internal/performance-gates.md):
//
//	cd internal/sync
//	go test -tags 'fts5,macrobench' -run '^$' -bench 'BenchmarkMacroCodexQuietAppend' \
//	  -benchmem -count=6 -benchtime=5x
//
// The gate: the p95 sec/op of the 1GB run must be within 2x of the 10MB
// run's p95 for the same append shape.
func BenchmarkMacroCodexQuietAppend10MB(b *testing.B) {
	benchCodexQuietAppendSignals(b, 8000)
}

func BenchmarkMacroCodexQuietAppend1GB(b *testing.B) {
	benchCodexQuietAppendSignals(b, 790000)
}

// TestMacroCodexRealSession drives the real-session macro measurement on an
// isolated copy of a production-scale Codex transcript: cold full sync,
// one 794B late tool-output append, and a no-op resync, each logged with
// wall time and the process rchar delta. Paths come from environment
// variables so no private absolute path lives in the repository:
//
//	MACRO_CODEX_SESSION=/path/to/rollout-*.jsonl \
//	MACRO_CODEX_LATE_OUTPUT=/path/to/late-output.jsonl \
//	MACRO_CODEX_TRUNCATE=<byte offset of the late output line> \
//	/usr/bin/time -v go test -tags 'fts5,macrobench' \
//	  -run TestMacroCodexRealSession -count=1 -v ./internal/sync/
//
// The source is stream-copied into a temp directory (never fully read into
// memory, so the peak RSS reflects the engine, not the harness) and
// truncated at MACRO_CODEX_TRUNCATE — the byte offset of the late output's
// own line — so the live archive is never touched by the engine and the
// append is a genuine late-result update.
func TestMacroCodexRealSession(t *testing.T) {
	src := os.Getenv("MACRO_CODEX_SESSION")
	late := os.Getenv("MACRO_CODEX_LATE_OUTPUT")
	if src == "" || late == "" || os.Getenv("MACRO_CODEX_TRUNCATE") == "" {
		t.Skip("set MACRO_CODEX_SESSION, MACRO_CODEX_LATE_OUTPUT, and MACRO_CODEX_TRUNCATE")
	}
	lateBytes, err := os.ReadFile(late)
	require.NoError(t, err)
	trunc, err := strconv.ParseInt(os.Getenv("MACRO_CODEX_TRUNCATE"), 10, 64)
	require.NoError(t, err)

	root := t.TempDir()
	day := filepath.Join(root, "2026", "08", "09")
	require.NoError(t, os.MkdirAll(day, 0o755))
	dst := filepath.Join(day, filepath.Base(src))
	if err := copyFilePrefix(src, dst, trunc); err != nil {
		t.Fatalf("copying snapshot prefix: %v", err)
	}

	database, err := db.Open(filepath.Join(t.TempDir(), "macro.db"))
	require.NoError(t, err)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "macro-host",
	})
	t.Cleanup(func() {
		engine.Close()
		_ = database.Close()
	})

	rchar := func() int64 {
		t.Helper()
		data, err := os.ReadFile("/proc/self/io")
		require.NoError(t, err)
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "rchar: ") {
				v, err := strconv.ParseInt(
					strings.TrimSpace(strings.TrimPrefix(line, "rchar: ")),
					10, 64,
				)
				require.NoError(t, err)
				return v
			}
		}
		t.Fatal("no rchar in /proc/self/io")
		return 0
	}

	// 1. Cold full sync.
	r0 := rchar()
	start := time.Now()
	stats := engine.SyncAll(context.Background(), nil)
	fullDur := time.Since(start)
	t.Logf(
		"MACRO_FULL synced=%d skipped=%d dur=%s rchar=%d",
		stats.Synced, stats.Skipped, fullDur, rchar()-r0,
	)
	require.Equal(t, 1, stats.Synced)

	sessionID := func() string {
		inc, ok := database.GetSessionForIncremental(
			dst, string(parser.AgentCodex),
		)
		require.True(t, ok, "the snapshot session must be incremental-tracked")
		return inc.ID
	}()

	// 2. Append the late tool output.
	f, err := os.OpenFile(dst, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.Write(lateBytes)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	r0 = rchar()
	start = time.Now()
	stats = engine.SyncAll(context.Background(), nil)
	incDur := time.Since(start)
	t.Logf(
		"MACRO_INC synced=%d skipped=%d dur=%s rchar=%d",
		stats.Synced, stats.Skipped, incDur, rchar()-r0,
	)
	require.Equal(t, 1, stats.Synced)
	sess, err := database.GetSessionFull(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	t.Logf(
		"MACRO_INC_SESSION last_write_incremental=%v msgs=%d file_size=%d",
		sess.LastWriteIncremental, sess.MessageCount, *sess.FileSize,
	)
	require.True(t, sess.LastWriteIncremental,
		"the late-output append must take the incremental path")
	require.Equal(t, db.CurrentQualitySignalVersion,
		sess.QualitySignalVersion,
		"the maintained append must keep signals current")

	// 3. No-op resync.
	r0 = rchar()
	start = time.Now()
	stats = engine.SyncAll(context.Background(), nil)
	noopDur := time.Since(start)
	t.Logf(
		"MACRO_NOOP synced=%d skipped=%d dur=%s rchar=%d",
		stats.Synced, stats.Skipped, noopDur, rchar()-r0,
	)
	require.Equal(t, 1, stats.Skipped)
}

// copyFilePrefix stream-copies the first limit bytes of src to dst, so a
// near-gigabyte snapshot never inflates the macro process's RSS.
func copyFilePrefix(src, dst string, limit int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if limit > info.Size() {
		limit = info.Size()
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, io.LimitReader(in, limit))
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

// TestMacroCodexStreamingMemoryGates is the P3 streaming-parse memory gate:
// it cold-syncs synthetic Codex transcripts whose content is dominated by
// tool outputs at 10MB / 100MB / 1GB, recording wall time, rchar delta,
// peak live heap (polled during the sync), forced-GC live heap, and peak
// RSS. The gate fails on the pre-P3 implementation (the parser keeps the
// whole session in the Go heap) and must pass once the single-pass
// staging sink lands:
//
//   - 1GB peak live heap < 512MiB (stretch: < 350MiB);
//   - 10MB -> 1GB peak growth <= 2x.
//
// Message count is held constant across sizes (500 turns) so the
// measurement isolates content-proportional memory.
func TestMacroCodexStreamingMemoryGates(t *testing.T) {
	sizes := []struct {
		name     string
		turns    int
		outBytes int
	}{
		{name: "10MB", turns: 500, outBytes: 20 << 10},
		{name: "100MB", turns: 500, outBytes: 200 << 10},
		{name: "1GB", turns: 500, outBytes: 2 << 20},
	}
	var basePeak uint64
	for i, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			root, _, _, _ := writeCodexStreamingBenchmarkTranscript(
				t, size.turns, size.outBytes,
			)
			database, err := db.Open(filepath.Join(t.TempDir(), "macro.db"))
			require.NoError(t, err)
			engine := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{
					parser.AgentCodex: {root},
				},
				Machine: "macro-host",
			})
			t.Cleanup(func() {
				engine.Close()
				_ = database.Close()
			})

			peak := pollPeakLiveHeap()
			r0 := macroRchar(t)
			start := time.Now()
			stats := engine.SyncAll(context.Background(), nil)
			dur := time.Since(start)
			rcharDelta := macroRchar(t) - r0
			peakLive := peak()
			require.Equal(t, 1, stats.Synced)

			t.Logf(
				"STREAMING_GATE %s file=%dMB dur=%s rchar=%d "+
					"peak_live=%dMiB forced_gc=%dMiB peak_rss=%dMiB",
				size.name,
				(size.turns*size.outBytes)/(1<<20),
				dur,
				rcharDelta,
				peakLive/(1<<20),
				forcedGCLiveHeap()/(1<<20),
				peakProcessRSSBytes()/(1<<20),
			)
			if i == 0 {
				basePeak = peakLive
			}
			if size.name == "1GB" {
				require.Less(t, peakLive, uint64(512<<20),
					"1GB cold sync peak live heap must stay under 512MiB")
				require.LessOrEqual(t, peakLive, 2*basePeak,
					"10MB -> 1GB peak live heap must grow at most 2x")
			}
		})
	}
}

// writeCodexStreamingBenchmarkTranscript writes a synthetic Codex
// transcript with `turns` turns, each ending in a function_call plus a
// function_call_output carrying outBytes of content. Lines are written
// directly to the file so the fixture itself never allocates a
// file-sized string.
func writeCodexStreamingBenchmarkTranscript(
	t testing.TB, turns, outBytes int,
) (root, dst, uuid string, sizeBytes int64) {
	t.Helper()
	uuid = codexSignalBenchmarkUUID
	root = filepath.Join(t.TempDir(), "sessions")
	day := filepath.Join(root, "2026", "07", "10")
	require.NoError(t, os.MkdirAll(day, 0o755))
	dst = filepath.Join(day, "rollout-2026-07-10T07-00-00-"+uuid+".jsonl")
	f, err := os.Create(dst)
	require.NoError(t, err)
	write := func(line string) {
		t.Helper()
		_, err := f.WriteString(line + "\n")
		require.NoError(t, err)
	}
	write(testjsonl.CodexSessionMetaJSON(
		uuid, "/workspace/project-a", "codex_cli_rs",
		"2026-07-10T07:00:00Z",
	))
	write(testjsonl.CodexTurnContextJSON(
		"gpt-5.4", "2026-07-10T07:00:01Z",
	))
	seed := "tool output: build log line with realistic command content. "
	pad := strings.Repeat(
		seed, (outBytes+len(seed)-1)/len(seed),
	)[:outBytes]
	for i := range turns {
		ts := time.Date(
			2026, 7, 10, 7, 1, 0, 0, time.UTC,
		).Add(time.Duration(i) * time.Second)
		tsStr := ts.Format("2006-01-02T15:04:05Z")
		callID := "call_" + strconv.Itoa(i)
		write(testjsonl.CodexMsgJSON(
			"user", "run task "+strconv.Itoa(i), tsStr,
		))
		write(testjsonl.CodexMsgJSON(
			"assistant", "running task "+strconv.Itoa(i), tsStr,
		))
		write(testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", callID, nil, tsStr,
		))
		write(testjsonl.CodexFunctionCallOutputJSON(
			callID, pad, tsStr,
		))
	}
	require.NoError(t, f.Close())
	info, err := os.Stat(dst)
	require.NoError(t, err)
	return root, dst, uuid, info.Size()
}

// pollPeakLiveHeap samples the runtime's live-heap metric (the heap
// occupied by live objects at the last GC, excluding uncollected garbage)
// and returns the peak observed until the returned function is called.
func pollPeakLiveHeap() func() uint64 {
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	all := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				metrics.Read(all)
				if v := all[0].Value.Uint64(); v > peak.Load() {
					peak.Store(v)
				}
			}
		}
	}()
	return func() uint64 {
		close(stop)
		<-done
		return peak.Load()
	}
}

func forcedGCLiveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

func peakProcessRSSBytes() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			fields := strings.Fields(line)
			if len(fields) == 3 {
				kb, err := strconv.ParseUint(fields[1], 10, 64)
				if err == nil {
					return kb << 10
				}
			}
		}
	}
	return 0
}

func macroRchar(t testing.TB) int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/io")
	require.NoError(t, err)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "rchar: ") {
			v, err := strconv.ParseInt(
				strings.TrimSpace(strings.TrimPrefix(line, "rchar: ")),
				10, 64,
			)
			require.NoError(t, err)
			return v
		}
	}
	t.Fatal("no rchar in /proc/self/io")
	return 0
}

// TestMacroCodexStagedParseMemoryGates runs the same three sizes through
// the streaming staged path end to end (scratch-backed parse plus the
// staged publish) and applies the same bounds as the legacy gate. It is
// the direct memory gate for the P3 staging sink; the engine wiring
// behind the >128MB cutoff reuses exactly this path.
func TestMacroCodexStagedParseMemoryGates(t *testing.T) {
	sizes := []struct {
		name     string
		turns    int
		outBytes int
	}{
		{name: "10MB", turns: 500, outBytes: 20 << 10},
		{name: "100MB", turns: 500, outBytes: 200 << 10},
		{name: "1GB", turns: 500, outBytes: 2 << 20},
	}
	var basePeak uint64
	for i, size := range sizes {
		t.Run(size.name, func(t *testing.T) {
			root, _, uuid, _ := writeCodexStreamingBenchmarkTranscript(
				t, size.turns, size.outBytes,
			)
			cfg := parser.ProviderConfig{
				Roots:   []string{root},
				Machine: "macro-host",
			}
			provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
			require.True(t, ok)
			source, found, err := provider.FindSource(
				context.Background(), parser.FindSourceRequest{
					FullSessionID: "codex:" + uuid,
				},
			)
			require.NoError(t, err)
			require.True(t, found)

			peak := pollPeakLiveHeap()
			start := time.Now()
			staged, err := newCodexStagingSink(nil)
			require.NoError(t, err)
			sess, msgs, _, _, _, err := parser.ParseCodexSessionStreaming(
				cfg, source, staged,
			)
			require.NoError(t, err)
			require.NotNil(t, sess)

			database, err := db.Open(filepath.Join(t.TempDir(), "macro.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = database.Close() })
			row := db.Session{
				ID:               sess.ID,
				Project:          sess.Project,
				Machine:          sess.Machine,
				Agent:            string(sess.Agent),
				MessageCount:     sess.MessageCount,
				UserMessageCount: sess.UserMessageCount,
			}
			require.NoError(t, database.UpsertSession(row))
			dbMsgs := toDBMessages(pendingWrite{
				sess: *sess, msgs: msgs,
			}, nil)
			update, findingsFromMsgs := computeSignalsAndSecrets(
				row, dbMsgs,
			)
			positions := make(map[string]db.StagedToolCallPosition)
			for _, m := range dbMsgs {
				for callIdx, tc := range m.ToolCalls {
					if tc.ToolUseID == "" {
						continue
					}
					positions[tc.ToolUseID] = db.StagedToolCallPosition{
						ToolUseID: tc.ToolUseID,
						Ordinal:   m.Ordinal,
						CallIndex: callIdx,
					}
				}
			}
			combined := append(
				append([]db.SecretFinding(nil), findingsFromMsgs...),
				staged.Findings(row.ID, positions)...,
			)
			update.SecretLeakCount = definiteFindingCount(combined)
			require.NoError(t, database.ReplaceSessionContentStaged(
				row.ID, dbMsgs, update, combined, staged,
				map[string]bool{},
			))
			require.NoError(t, staged.Close())
			peakLive := peak()
			dur := time.Since(start)

			t.Logf(
				"STAGED_GATE %s dur=%s peak_live=%dMiB forced_gc=%dMiB peak_rss=%dMiB",
				size.name, dur,
				peakLive/(1<<20),
				forcedGCLiveHeap()/(1<<20),
				peakProcessRSSBytes()/(1<<20),
			)
			if i == 0 {
				basePeak = peakLive
			}
			if size.name == "1GB" {
				require.Less(t, peakLive, uint64(512<<20),
					"1GB staged parse peak live heap must stay under 512MiB")
				// The streaming path's baseline (SQLite session, test
				// scaffolding, scratch connections) is a fixed overhead
				// that dwarfs the 10MB tier's parse work, so the growth
				// ratio is measured against a 16MiB floor: below that,
				// run-to-run noise dominates and a 2x check on a 7MiB
				// base is meaningless. The bound still catches any
				// O(file) retention, which lands hundreds of MiB above
				// the floor.
				growthBase := max(basePeak, 16<<20)
				require.LessOrEqual(t, peakLive, 2*growthBase,
					"10MB -> 1GB staged peak live heap must grow at most 2x")
			}
		})
	}
}

// TestMacroCodexEngineStagedFullParse syncs a >128MB Codex transcript
// through the real engine and asserts the staged streaming path (the
// >stagedCodexParseMinBytes cutoff) publishes the message, tool-call,
// event, and summary rows the archive expects: events carry real content,
// summaries carry the per-call aggregated output, and the session counts
// match the fixture shape.
func TestMacroCodexEngineStagedFullParse(t *testing.T) {
	const turns = 750
	const outBytes = 200 << 10 // 150MB total, above the 128MB cutoff
	root, _, uuid, _ := writeCodexStreamingBenchmarkTranscript(
		t, turns, outBytes,
	)

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "macro-host",
	})
	t.Cleanup(engine.Close)

	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Synced)

	sessionID := "codex:" + uuid
	got, err := database.GetSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, turns*3, got.MessageCount)
	require.Equal(t, turns, got.UserMessageCount)

	msgs, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, msgs, turns*3)
	var eventCount int
	var summaryBytes int
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			summaryBytes += len(tc.ResultContent)
			for _, ev := range tc.ResultEvents {
				eventCount++
				require.NotEmpty(t, ev.Content,
					"staged publish must store real event content")
				require.GreaterOrEqual(t, ev.ContentLength, outBytes,
					"event content length must match the staged row")
			}
		}
	}
	require.Equal(t, turns, eventCount)
	require.Greater(t, summaryBytes, turns*(outBytes-1<<16),
		"per-call summaries must carry the staged output content")
}

// TestMacroCodexRealArchiveColdSync is the acceptance run for the staged
// streaming path against a real large Codex transcript. Set
// MACRO_CODEX_945MB to the archive path; the test copies it into the test
// root (never touching the original), syncs it through the real engine,
// and gates peak live heap and RSS under 512MiB. A second no-op sync then
// asserts the unchanged transcript is skipped without re-reading it.
func TestMacroCodexRealArchiveColdSync(t *testing.T) {
	src := os.Getenv("MACRO_CODEX_945MB")
	if src == "" {
		t.Skip("set MACRO_CODEX_945MB to a large real Codex transcript")
	}
	info, err := os.Stat(src)
	require.NoError(t, err)
	require.Greater(t, info.Size(), int64(500<<20),
		"MACRO_CODEX_945MB must point at a >500MiB transcript")

	// Rebuild the dated codex layout under the test root so discovery
	// sees a real transcript copy, and keep the original untouched.
	srcDir := filepath.Dir(src)
	rel := filepath.Join(
		filepath.Base(filepath.Dir(filepath.Dir(srcDir))),
		filepath.Base(filepath.Dir(srcDir)),
		filepath.Base(srcDir),
		filepath.Base(src),
	)
	root := filepath.Join(t.TempDir(), "sessions")
	dst := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	in, err := os.Open(src)
	require.NoError(t, err)
	out, err := os.Create(dst)
	require.NoError(t, err)
	_, err = io.Copy(out, in)
	require.NoError(t, err)
	require.NoError(t, in.Close())
	require.NoError(t, out.Close())

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCodex: {root},
		},
		Machine: "macro-host",
	})
	t.Cleanup(engine.Close)

	start := time.Now()
	peak := pollPeakLiveHeap()
	rcharBefore := macroRchar(t)
	stats := engine.SyncAll(t.Context(), nil)
	peakLive := peak()
	rcharDelta := macroRchar(t) - rcharBefore
	require.Zero(t, stats.Failed)
	require.Equal(t, 1, stats.Synced)
	t.Logf(
		"REAL945 cold dur=%s peak_live=%dMiB rss=%dMiB read=%dMiB size=%dMiB",
		time.Since(start), peakLive/(1<<20),
		peakProcessRSSBytes()/(1<<20), rcharDelta/(1<<20),
		info.Size()/(1<<20),
	)
	require.Less(t, peakLive, uint64(512<<20),
		"945MB cold sync peak live heap must stay under 512MiB")
	require.Less(t, peakProcessRSSBytes(), uint64(512<<20),
		"945MB cold sync RSS must stay under 512MiB")
	// The single-pass tee bounds transcript reads to one file pass. The
	// remaining rchar is scratch publish I/O: the staged rows and
	// summaries are read back once each while the replace transaction
	// copies them into the archive. The ceiling excludes any second
	// transcript pass (the pre-P3 cold path re-read the source multiple
	// times and exceeded this several times over).
	require.LessOrEqual(t, rcharDelta, 6*info.Size(),
		"cold sync must not re-read the source multiple times")

	// No-op sync: the unchanged transcript must be skipped without
	// touching the source bytes again.
	noopBefore := macroRchar(t)
	stats = engine.SyncAll(t.Context(), nil)
	noopDelta := macroRchar(t) - noopBefore
	require.Zero(t, stats.Failed)
	require.Zero(t, stats.Synced, "unchanged archive must be skipped")
	t.Logf("REAL945 noop dur=%s read=%dKiB",
		time.Since(start), noopDelta/(1<<10))
	require.LessOrEqual(t, noopDelta, int64(16<<20),
		"no-op sync must not re-read the transcript")
}
