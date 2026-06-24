package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// oneDayUsageRange is the from/to query for a single-day usage
// window used across the usage handler tests.
const oneDayUsageRange = "from=2024-06-01&to=2024-06-01"

type usageSummaryCountsSpy struct {
	db.Store
	dailyCalls  int
	countsCalls int
	filters     []db.UsageFilter
}

// assertUsageQueryCalls verifies how many times the usage handler
// queried the daily-usage and session-count store methods.
func assertUsageQueryCalls(
	t *testing.T, spy *usageSummaryCountsSpy,
	wantDaily, wantCounts int,
) {
	t.Helper()
	assert.Equal(t, wantDaily, spy.dailyCalls, "daily usage calls")
	assert.Equal(t, wantCounts, spy.countsCalls, "session count calls")
}

func (s *usageSummaryCountsSpy) GetDailyUsage(
	_ context.Context, f db.UsageFilter,
) (db.DailyUsageResult, error) {
	s.dailyCalls++
	s.filters = append(s.filters, f)
	return db.DailyUsageResult{
		Daily: []db.DailyUsageEntry{{
			Date:      "2024-06-01",
			TotalCost: 1,
		}},
		Totals: db.UsageTotals{TotalCost: 1},
		SessionCounts: db.UsageSessionCounts{
			Total:     1,
			ByProject: map[string]int{"proj": 1},
			ByAgent:   map[string]int{"claude": 1},
		},
	}, nil
}

func (s *usageSummaryCountsSpy) GetUsageSessionCounts(
	_ context.Context, _ db.UsageFilter,
) (db.UsageSessionCounts, error) {
	s.countsCalls++
	return db.UsageSessionCounts{}, nil
}

func TestUsageSummaryScansCurrentPeriodOnly(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s, "/api/v1/usage/summary?"+oneDayUsageRange)
	assertRecorderStatus(t, w, http.StatusOK)

	assertUsageQueryCalls(t, spy, 1, 0)
}

func TestUsageSummaryDefaultsToBreakdowns(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s, "/api/v1/usage/summary?"+oneDayUsageRange)
	assertRecorderStatus(t, w, http.StatusOK)

	require.Len(t, spy.filters, 1)
	assert.True(t, spy.filters[0].Breakdowns)
}

func TestUsageSummaryCanSkipBreakdowns(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s,
		"/api/v1/usage/summary?"+oneDayUsageRange+"&breakdowns=false")
	assertRecorderStatus(t, w, http.StatusOK)

	require.Len(t, spy.filters, 1)
	assert.False(t, spy.filters[0].Breakdowns)
}

func TestUsageSummaryDefaultsToSessionCounts(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s, "/api/v1/usage/summary?"+oneDayUsageRange)
	assertRecorderStatus(t, w, http.StatusOK)

	require.Len(t, spy.filters, 1)
	assert.False(t, spy.filters[0].SkipSessionCounts)
}

func TestUsageSummaryCanSkipSessionCounts(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s,
		"/api/v1/usage/summary?"+oneDayUsageRange+"&session_counts=false")
	assertRecorderStatus(t, w, http.StatusOK)

	require.Len(t, spy.filters, 1)
	assert.True(t, spy.filters[0].SkipSessionCounts)
}

func TestUsageComparisonScansPriorPeriodOnly(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s,
		"/api/v1/usage/comparison?"+oneDayUsageRange+"&current_cost=3")
	assertRecorderStatus(t, w, http.StatusOK)

	assertUsageQueryCalls(t, spy, 1, 0)

	var out Comparison
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "2024-05-31", out.PriorFrom)
	assert.Equal(t, "2024-05-31", out.PriorTo)
	assert.Equal(t, 1.0, out.PriorTotalCost)
	assert.Equal(t, 2.0, out.DeltaPct)
}

func TestUsageComparisonRequiresCurrentCost(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s, "/api/v1/usage/comparison?"+oneDayUsageRange)
	assertRecorderStatus(t, w, http.StatusBadRequest)

	assertUsageQueryCalls(t, spy, 0, 0)
}

func TestUsageComparisonNoDefaultRangeRequiresConcreteRange(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s,
		"/api/v1/usage/comparison?no_default_range=true&current_cost=3")
	assertRecorderStatus(t, w, http.StatusBadRequest)
	assert.Contains(t, w.Body.String(), "requires from and to")

	assertUsageQueryCalls(t, spy, 0, 0)
}

func TestUsageComparisonAllowsZeroCurrentCost(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s,
		"/api/v1/usage/comparison?"+oneDayUsageRange+"&current_cost=0")
	assertRecorderStatus(t, w, http.StatusOK)

	assertUsageQueryCalls(t, spy, 1, 0)
}

func TestComputeCacheStats_SavingsPassThrough(t *testing.T) {
	// SavingsVsUncached is now computed per-model in the DB
	// layer; computeCacheStats just forwards totals.CacheSavings.
	// Verify the pass-through at the positive, negative, and
	// zero boundaries so a future refactor that drops the field
	// trips a test.
	cases := []struct {
		name string
		in   float64
	}{
		{"positive", 4.65},
		{"negative", -0.75},
		{"zero", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := computeCacheStats(db.UsageTotals{
				CacheSavings: tc.in,
			})
			assert.InDelta(t, tc.in, cs.SavingsVsUncached, 1e-9)
		})
	}
}

func TestComputeCacheStats_ZeroTotalsIsZero(t *testing.T) {
	cs := computeCacheStats(db.UsageTotals{})
	assert.Zero(t, cs.SavingsVsUncached)
	assert.Zero(t, cs.HitRate)
}

func TestComputeCacheStats_HitRate(t *testing.T) {
	// 800 cache reads, 200 uncached inputs -> 0.80 hit rate.
	// (The HitRate denominator in this code is
	// cacheRead + input where input is already the uncached
	// portion — see the pass-through test below.)
	cs := computeCacheStats(db.UsageTotals{
		InputTokens:     200,
		CacheReadTokens: 800,
	})
	// denom = 800 + 200 = 1000; hit = 800/1000 = 0.80.
	assert.InDelta(t, 0.80, cs.HitRate, 1e-9)
}

func TestComputeCacheStats_UncachedPassesInputThrough(t *testing.T) {
	// Anthropic's input_tokens field is the NON-cached portion
	// of the input; cache_read and cache_creation are tracked
	// separately. UncachedInputTokens must therefore equal
	// InputTokens directly — not input minus the cache buckets,
	// which would double-subtract and wrongly drive the value
	// toward zero for any cached workload.
	cs := computeCacheStats(db.UsageTotals{
		InputTokens:         100,
		CacheReadTokens:     200,
		CacheCreationTokens: 50,
	})
	assert.Equal(t, 100, cs.UncachedInputTokens)
	// And the cache buckets are reported verbatim alongside it.
	assert.Equal(t, 200, cs.CacheReadTokens)
	assert.Equal(t, 50, cs.CacheCreationTokens)
}
