//go:build macrobench

package sync

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
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
