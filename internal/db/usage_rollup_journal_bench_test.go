//go:build fts5

package db

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/money"
)

// This fixture uses public archive/cache calls so the same benchmark can
// measure the pre-journal implementation. About 3% of one-row sessions span
// midnight, giving roughly 30,900 daily rows at 30,000 sessions.
func BenchmarkUsageRollupJournaled(b *testing.B) {
	count := 300
	if raw := os.Getenv("AGENTSVIEW_BENCH_SESSIONS"); raw != "" {
		var err error
		count, err = strconv.Atoi(raw)
		require.NoError(b, err)
		require.Positive(b, count)
	}
	for _, dailyRows := range []int{1, 3} {
		b.Run(fmt.Sprintf("rows-%d", dailyRows), func(b *testing.B) {
			database, filter, want := seedUsageJournalBenchmark(b, count, dailyRows)
			for _, readers := range []int{1, 7} {
				b.Run(fmt.Sprintf("readers-%d", readers), func(b *testing.B) {
					b.ReportAllocs()
					for range b.N {
						if readers == 1 {
							got, err := database.GetDailyUsage(b.Context(), filter)
							require.NoError(b, err)
							require.Equal(b, want, got)
							continue
						}
						var wg sync.WaitGroup
						errors := make(chan error, readers)
						for range readers {
							wg.Go(func() {
								got, err := database.GetDailyUsage(b.Context(), filter)
								// Fatal assertions belong to the benchmark goroutine.
								if err != nil {
									errors <- err
									return
								}
								assert.Equal(b, want, got)
							})
						}
						wg.Wait()
						close(errors)
						for err := range errors {
							require.NoError(b, err)
						}
					}
				})
			}
		})
	}
}

func seedUsageJournalBenchmark(tb testing.TB, count, dailyRows int) (*DB, UsageFilter, DailyUsageResult) {
	tb.Helper()
	database := testDB(tb)
	require.NoError(tb, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 1_000_000}, OutputPerMTok: money.Money{Microdollars: 2_000_000}}}))
	for start := 0; start < count; start += 256 {
		var writes []SessionBatchWrite
		for i := start; i < min(start+256, count); i++ {
			id := fmt.Sprintf("journal-%05d", i)
			day := time.Date(2026, 8, 4+i%5, 12, 0, 0, 0, time.UTC)
			started := day.Format(time.RFC3339)
			write := SessionBatchWrite{Session: Session{ID: id, Project: "project", Machine: "fixture", Agent: "codex", StartedAt: &started, UserMessageCount: 1}}
			for j := range 3 {
				offset := 0
				if dailyRows == 3 {
					offset = j
				} else if i%33 == 0 && j == 2 {
					offset = 1
				}
				write.Messages = append(write.Messages, Message{SessionID: id, Ordinal: j, Role: "assistant", Timestamp: day.AddDate(0, 0, offset).Add(time.Duration(j) * time.Minute).Format(time.RFC3339), Model: "journal-model", TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20}`)})
			}
			writes = append(writes, write)
		}
		_, err := database.WriteSessionBatchAtomic(tb.Context(), writes)
		require.NoError(tb, err)
	}
	filter := UsageFilter{From: "2026-08-04", To: "2026-08-10", SkipSessionCounts: true}
	require.NoError(tb, database.StartUsageCacheBackfill(tb.Context()))
	require.NoError(tb, database.WaitUsageCacheBackfill(tb.Context()))
	want, err := database.GetDailyUsage(tb.Context(), filter)
	require.NoError(tb, err)
	require.Equal(tb, count*300, want.Totals.InputTokens)
	require.Equal(tb, count*60, want.Totals.OutputTokens)
	require.Equal(tb, int64(count*420), want.Totals.TotalCost.Microdollars)
	return database, filter, want
}

// Warm reads must reuse installed totals. The optional timing gate keeps its
// larger fixture for callers supplying a base measurement.
func TestUsageJournaledWarmSevenDaysUnderHalfBase(t *testing.T) {
	var base int64
	count := 300
	if raw := os.Getenv("AGENTSVIEW_USAGE_BASE_NS"); raw != "" {
		count = 30000
		var err error
		base, err = strconv.ParseInt(raw, 10, 64)
		require.NoError(t, err)
		require.Positive(t, base)
	}
	database, filter, want := seedUsageJournalBenchmark(t, count, 1)
	database.StopUsageCacheBackfill()
	databaseID, err := database.GetDatabaseID(t.Context())
	require.NoError(t, err)
	cache, err := database.usageCache.Generation(t.Context(), databaseID)
	require.NoError(t, err)
	var fills, builds atomic.Int64
	cache.fill.observer.beforeInstall = func([]usageSourceVersion) { fills.Add(1) }
	cache.rollup.observer.beforeEnsure = func() { builds.Add(1) }
	// Benchmarks collect fixture garbage before timing their warm reads.
	runtime.GC()
	// Compare means, as the supplied base ns/op measures repeated warm reads.
	const reads = 5
	var elapsed time.Duration
	for range reads {
		started := time.Now()
		got, err := database.GetDailyUsage(t.Context(), filter)
		elapsed += time.Since(started)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	assert.Zero(t, fills.Load(), "warm reads must reuse cached facts")
	assert.Zero(t, builds.Load(), "warm reads must reuse installed daily totals")
	if base > 0 {
		assert.Less(t, elapsed/reads, time.Duration(base/2), "warm 7-day read must take under half the base time")
	}
}
