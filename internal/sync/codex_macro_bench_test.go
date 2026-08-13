//go:build macrobench

package sync

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
			root, dst, _, sizeBytes :=
				writeCodexStreamingBenchmarkTranscript(
					t, size.turns, size.outBytes,
				)
			_ = sizeBytes

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

			forcedGCHeap := forcedGCLiveHeap()
			peakRSS := peakProcessRSSBytes()

			t.Logf(
				"STREAMING_GATE %s file=%dMB dur=%s rchar=%d "+
					"peak_live=%dMiB forced_gc=%dMiB peak_rss=%dMiB",
				size.name,
				(size.turns*size.outBytes)/(1<<20),
				dur,
				rcharDelta,
				peakLive/(1<<20),
				forcedGCHeap/(1<<20),
				peakRSS/(1<<20),
			)
			_ = dst

			if i == 0 {
				basePeak = peakLive
			}
			if size.name == "1GB" {
				// The gate: bounded absolute peak and sub-linear growth.
				// It red-lights the pre-P3 full-session-in-heap parser.
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

// pollPeakLiveHeap starts a poller capturing the highest live-heap value
// until the returned function is called.
func pollPeakLiveHeap() func() uint64 {
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > peak.Load() {
					peak.Store(ms.HeapAlloc)
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
