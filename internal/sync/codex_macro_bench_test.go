//go:build macrobench

package sync

import "testing"

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
