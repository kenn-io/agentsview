package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

type usageSummaryCountsSpy struct {
	db.Store
	dailyCalls  int
	countsCalls int
}

func (s *usageSummaryCountsSpy) GetDailyUsage(
	_ context.Context, _ db.UsageFilter,
) (db.DailyUsageResult, error) {
	s.dailyCalls++
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
	s := &Server{
		cfg: config.Config{Host: "127.0.0.1"},
		db:  spy,
		mux: http.NewServeMux(),
	}
	s.routes()

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/usage/summary?from=2024-06-01&to=2024-06-01",
		nil,
	)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	assert.Equal(t, 1, spy.dailyCalls, "current summary only")
	assert.Zero(t, spy.countsCalls)
}

func TestUsageComparisonScansPriorPeriodOnly(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := &Server{
		cfg: config.Config{Host: "127.0.0.1"},
		db:  spy,
		mux: http.NewServeMux(),
	}
	s.routes()

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/usage/comparison?from=2024-06-01&to=2024-06-01&current_cost=3",
		nil,
	)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	assert.Equal(t, 1, spy.dailyCalls, "prior comparison only")
	assert.Zero(t, spy.countsCalls)

	var out Comparison
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, "2024-05-31", out.PriorFrom)
	assert.Equal(t, "2024-05-31", out.PriorTo)
	assert.Equal(t, 1.0, out.PriorTotalCost)
	assert.Equal(t, 2.0, out.DeltaPct)
}

func TestUsageComparisonRequiresCurrentCost(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := &Server{
		cfg: config.Config{Host: "127.0.0.1"},
		db:  spy,
		mux: http.NewServeMux(),
	}
	s.routes()

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/usage/comparison?from=2024-06-01&to=2024-06-01",
		nil,
	)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	assert.Zero(t, spy.dailyCalls)
	assert.Zero(t, spy.countsCalls)
}

func TestUsageComparisonAllowsZeroCurrentCost(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := &Server{
		cfg: config.Config{Host: "127.0.0.1"},
		db:  spy,
		mux: http.NewServeMux(),
	}
	s.routes()

	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/usage/comparison?from=2024-06-01&to=2024-06-01&current_cost=0",
		nil,
	)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	assert.Equal(t, 1, spy.dailyCalls, "prior comparison only")
	assert.Zero(t, spy.countsCalls)
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
