package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
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

func TestUsageComparisonCopiesGitBranchFilterToPriorPeriod(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)
	branch := db.EncodeBranchFilterToken("alpha", "main")

	w := serveGet(t, s,
		"/api/v1/usage/comparison?"+oneDayUsageRange+"&current_cost=3&git_branch="+url.QueryEscape(branch))
	assertRecorderStatus(t, w, http.StatusOK)

	require.Len(t, spy.filters, 1)
	assert.Equal(t, branch, spy.filters[0].GitBranch)
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
