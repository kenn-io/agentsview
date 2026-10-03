package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkSourceScan50K measures the production scanner against synthetic
// files only. It reports retained Go heap separately from per-pass allocations.
func BenchmarkSourceScan50K(b *testing.B) {
	root := b.TempDir()
	for i := range 50000 {
		directory := filepath.Join(root, fmt.Sprintf("project-%03d", i/256))
		if i%256 == 0 {
			require.NoError(b, os.Mkdir(directory, 0o700))
		}
		require.NoError(b, os.WriteFile(filepath.Join(directory, fmt.Sprintf("session-%05d.jsonl", i)), nil, 0o600))
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	scanner := newSourceScanner([]WatchRoot{{Path: root, Recursive: true, SourceFileGlobs: []string{"*.jsonl"}}}, nil)
	emit := func(context.Context, WatchBatch) error { return nil }
	require.NoError(b, scanner.scan(b.Context(), emit))
	runtime.GC()
	var retained runtime.MemStats
	runtime.ReadMemStats(&retained)
	b.ResetTimer()
	for b.Loop() {
		require.NoError(b, scanner.scan(b.Context(), emit))
	}
	b.ReportMetric(float64(retained.HeapAlloc-before.HeapAlloc), "retained-bytes")
	b.ReportMetric(float64(scanner.stats().CachedNameBytes), "name-bytes")
	runtime.KeepAlive(scanner)
}
