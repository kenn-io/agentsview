package sync

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnchangedSkipPersistenceIsCardinalityIndependent(t *testing.T) {
	for _, size := range []int{8, 8000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{})
			t.Cleanup(engine.Close)
			entries := make(map[string]int64, size)
			for i := range size {
				entries[fmt.Sprintf("/archive/session-%d.jsonl", i)] = 42
			}
			engine.InjectSkipCache(entries)
			require.Equal(t, size, engine.persistSkipCache(t.Context()))
			allocations := testing.AllocsPerRun(5, func() { engine.persistSkipCache(t.Context()) })
			assert.Less(t, allocations, float64(10), "unchanged background persistence must not copy or rewrite the archive-sized cache")
			got, err := database.LoadSkippedFiles(t.Context())
			require.NoError(t, err)
			assert.Equal(t, entries, got)
			// A removed source must not return on the next flush or engine restart.
			engine.clearSkip(t.Context(), "/archive/session-0.jsonl")
			delete(entries, "/archive/session-0.jsonl")
			engine.cacheSkip("/archive/new.jsonl", 84)
			entries["/archive/new.jsonl"] = 84
			engine.persistSkipCache(t.Context())
			restarted := NewEngine(t.Context(), database, EngineConfig{})
			t.Cleanup(restarted.Close)
			assert.Equal(t, entries, restarted.SnapshotSkipCache())
		})
	}
}

func BenchmarkPersistUnchangedSkipCache(b *testing.B) {
	routeBenchLogs(b)
	engine, database := openBenchEngine(b, b.TempDir())
	entries := make(map[string]int64, 10000)
	for i := range 10000 {
		entries[fmt.Sprintf("/archive/session-%d.jsonl", i)] = 42
	}
	engine.InjectSkipCache(entries)
	require.Equal(b, 10000, engine.persistSkipCache(b.Context()))
	got, err := database.LoadSkippedFiles(b.Context())
	require.NoError(b, err)
	require.Len(b, got, 10000)
	b.ReportAllocs()
	for b.Loop() {
		require.Equal(b, 10000, engine.persistSkipCache(b.Context()))
	}
}
