//go:build fts5

package db

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
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
	// Project keys include archive identity, so separate runs need the same scope.
	require.NoError(tb, database.SetArchiveIdentityForTest(tb.Context(), "usage-page-fixture", "0000000000000000000000000000000000000000000000000000000000000001"))

	require.NoError(tb, database.UpsertModelPricing([]ModelPricing{{ModelPattern: "journal-model", InputPerMTok: money.Money{Microdollars: 1_000_000}, OutputPerMTok: money.Money{Microdollars: 2_000_000}}, {ModelPattern: "journal-other", InputPerMTok: money.Money{Microdollars: 1_000_000}, OutputPerMTok: money.Money{Microdollars: 2_000_000}}}))
	// Stable catalog timestamps make separate base/head runs byte-comparable.
	_, err := database.getWriter().ExecContext(tb.Context(), `UPDATE model_pricing SET updated_at='2026-08-01T00:00:00Z' WHERE model_pattern IN ('journal-model', 'journal-other')`)
	require.NoError(tb, err)

	for start := 0; start < count; start += 256 {
		var writes []SessionBatchWrite
		for i := start; i < min(start+256, count); i++ {
			id := fmt.Sprintf("journal-%05d", i)
			day := time.Date(2026, 8, 4+i%5, 12, 0, 0, 0, time.UTC)
			started := day.Format(time.RFC3339)
			write := SessionBatchWrite{Session: Session{ID: id, Project: fmt.Sprintf("project-%d", i%4), Machine: fmt.Sprintf("machine-%d", i/4%2), Agent: []string{"codex", "claude"}[i/8%2], GitBranch: fmt.Sprintf("branch-%d", i%2), StartedAt: &started, UserMessageCount: 1 + i%3, IsAutomated: i%7 == 0}}
			for j := range 3 {
				offset := 0
				if dailyRows == 3 {
					offset = j
				} else if i%33 == 0 && j == 2 {
					offset = 1
				}
				write.Messages = append(write.Messages, Message{SessionID: id, Ordinal: j, Role: "assistant", Timestamp: day.AddDate(0, 0, offset).Add(time.Duration(j) * time.Minute).Format(time.RFC3339), Model: []string{"journal-model", "journal-other"}[i/16%2], TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20}`)})
			}
			writes = append(writes, write)
		}
		_, err := database.WriteSessionBatchAtomic(tb.Context(), writes)
		require.NoError(tb, err)
	}
	filter := UsageFilter{From: "2026-08-04", To: "2026-08-10", Timezone: "UTC", SkipSessionCounts: true}
	want, err := database.GetDailyUsage(tb.Context(), filter)
	require.NoError(tb, err)
	require.NoError(tb, database.StartUsageCacheBackfill(tb.Context()))
	require.NoError(tb, database.WaitUsageCacheBackfill(tb.Context()))
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

// The five reads represent Summary, Pairwise ranking and sides, and Comparison.
func usageDayTotalsPageFilters(allHistory bool) []UsageFilter {
	filter := UsageFilter{From: "2026-08-04", To: "2026-08-10", Timezone: "UTC"}
	if allHistory {
		filter.From, filter.To = "", ""
	}
	summary, ranking, left, right, prior := filter, filter, filter, filter, filter
	summary.Breakdowns = true
	ranking.SkipSessionCounts = true
	left.Agent, right.Agent = "codex", "claude"
	prior.From, prior.To, prior.SkipSessionCounts = "2026-07-28", "2026-08-03", true
	return []UsageFilter{summary, ranking, left, right, prior}
}

func seedUsageDayTotalsPage(tb testing.TB, count int) *DB {
	tb.Helper()
	database, _, _ := seedUsageJournalBenchmark(tb, count, 1)
	// A prior-period row makes Comparison exercise real data too.
	started := "2026-08-03T23:30:00Z"
	_, err := database.WriteSessionBatchAtomic(tb.Context(), []SessionBatchWrite{
		{Session: Session{ID: "reported", Project: "project-0", Machine: "machine-0", Agent: "copilot", StartedAt: &started, UserMessageCount: 2}, Messages: []Message{
			{SessionID: "reported", Ordinal: 0, Role: "assistant", Timestamp: started, Model: "journal-model", TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20}`)},
			{SessionID: "reported", Ordinal: 1, Role: "assistant", Timestamp: "2026-08-04T00:30:00Z", Model: "journal-model", TokenUsage: []byte(`{"input_tokens":200,"output_tokens":30}`)},
		}},
		{Session: Session{ID: "duplicate-a", Project: "project-0", Agent: "claude", StartedAt: &started, UserMessageCount: 1}, Messages: []Message{{SessionID: "duplicate-a", Ordinal: 0, Role: "assistant", Timestamp: started, Model: "journal-model", TokenUsage: []byte(`{"output_tokens":10}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"}}},
		{Session: Session{ID: "duplicate-b", Project: "project-1", Agent: "claude", StartedAt: &started, UserMessageCount: 2}, Messages: []Message{{SessionID: "duplicate-b", Ordinal: 0, Role: "assistant", Timestamp: started, Model: "journal-model", TokenUsage: []byte(`{"output_tokens":20}`), ClaudeMessageID: "shared", ClaudeRequestID: "request"}}},
	})
	require.NoError(tb, err)
	cost := money.Money{Microdollars: 1234567}
	require.NoError(tb, database.ReplaceSessionUsageEvents(tb.Context(), "reported", []UsageEvent{{Source: "shutdown", Model: "journal-model", InputTokens: 10, Cost: &cost, CostStatus: "exact", CostSource: CopilotReportedCostSource, OccurredAt: "2026-08-04T01:00:00Z", DedupKey: "reported"}}))
	require.NoError(tb, database.StartUsageCacheBackfill(tb.Context()))
	require.NoError(tb, database.WaitUsageCacheBackfill(tb.Context()))
	database.StopUsageCacheBackfill()
	return database
}

func BenchmarkUsageDayTotalsPage(b *testing.B) {
	count := 300
	if raw := os.Getenv("AGENTSVIEW_BENCH_SESSIONS"); raw != "" {
		var err error
		count, err = strconv.Atoi(raw)
		require.NoError(b, err)
		require.Positive(b, count)
	}
	database := seedUsageDayTotalsPage(b, count)
	for _, all := range []bool{false, true} {
		name := "7-days"
		if all {
			name = "all-history"
		}
		b.Run(name, func(b *testing.B) {
			filters := usageDayTotalsPageFilters(all)
			want := make([][]byte, len(filters))
			for i, filter := range filters {
				result, err := database.getDailyUsageLegacy(b.Context(), filter)
				require.NoError(b, err)
				want[i], err = json.Marshal(result)
				require.NoError(b, err)
				// Warm each requested path before timing.
				_, err = database.GetDailyUsage(b.Context(), filter)
				require.NoError(b, err)
				b.Logf("read-%d sha256=%x", i, sha256.Sum256(want[i]))
				if dir := os.Getenv("AGENTSVIEW_USAGE_RESULTS_DIR"); dir != "" {
					require.NoError(b, os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%d.json", name, i)), want[i], 0600))
				}
			}
			runtime.GC()
			b.ResetTimer()
			for range b.N {
				results := make([]DailyUsageResult, len(filters))
				errs := make([]error, len(filters))
				var wg sync.WaitGroup
				for i, filter := range filters {
					wg.Go(func() { results[i], errs[i] = database.GetDailyUsage(b.Context(), filter) })
				}
				wg.Wait()
				b.StopTimer()
				for i, result := range results {
					require.NoError(b, errs[i])
					got, err := json.Marshal(result)
					require.NoError(b, err)
					require.Equal(b, string(want[i]), string(got))
					if dir := os.Getenv("AGENTSVIEW_USAGE_RESULTS_DIR"); dir != "" {
						require.NoError(b, os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%d.json", name, i)), got, 0600))
					}
				}
				b.StartTimer()
			}
		})
	}
}

func TestUsageDayTotalsPageBudget(t *testing.T) {
	if os.Getenv("AGENTSVIEW_USAGE_PAGE_GATE") == "" {
		t.Skip("set AGENTSVIEW_USAGE_PAGE_GATE for the 30k-session timing gate")
	}
	database := seedUsageDayTotalsPage(t, 30000)
	for _, all := range []bool{false, true} {
		filters := usageDayTotalsPageFilters(all)
		want := make([]DailyUsageResult, len(filters))
		for i, filter := range filters {
			var err error
			want[i], err = database.getDailyUsageLegacy(t.Context(), filter)
			require.NoError(t, err)
			_, err = database.GetDailyUsage(t.Context(), filter)
			require.NoError(t, err)
		}
		runtime.GC()
		got := make([]DailyUsageResult, len(filters))
		errs := make([]error, len(filters))
		var wg sync.WaitGroup
		start := time.Now()
		for i, filter := range filters {
			wg.Go(func() { got[i], errs[i] = database.GetDailyUsage(t.Context(), filter) })
		}
		wg.Wait()
		elapsed := time.Since(start)
		t.Logf("all-history=%t warm page=%s", all, elapsed)
		assert.Less(t, elapsed, time.Second/2)
		for i := range filters {
			require.NoError(t, errs[i])
			require.Equal(t, mustUsageJSON(t, want[i]), mustUsageJSON(t, got[i]))
		}
	}
}

func mustUsageJSON(tb testing.TB, result DailyUsageResult) string {
	tb.Helper()
	raw, err := json.Marshal(result)
	require.NoError(tb, err)
	return string(raw)
}
