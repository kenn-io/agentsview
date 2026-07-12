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
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/parsertest"
	"go.kenn.io/agentsview/internal/service"
)

// oneDayUsageRange is the from/to query for a single-day usage
// window used across the usage handler tests.
const oneDayUsageRange = "from=2024-06-01&to=2024-06-01"

func TestUsageInputAPIErrorPreservesMachineReadableCode(t *testing.T) {
	err := usageInputAPIError(&service.UsageInputError{
		Code: service.UsageErrorCodeUnknownProjectKey,
		Msg:  "wording may change",
	})
	var response *apiErrorResponse
	require.ErrorAs(t, err, &response)
	assert.Equal(t, http.StatusBadRequest, response.Status)
	assert.Equal(t, service.UsageErrorCodeUnknownProjectKey, response.Code)
	assert.Equal(t, "wording may change", response.Message)
}

type usageSummaryCountsSpy struct {
	db.Store
	dailyCalls           int
	countsCalls          int
	matchingSessionCalls int
	matchingSessionCount int
	filters              []db.UsageFilter
	result               db.DailyUsageResult
}

func TestUsageSummaryResponseEmitsEmptyProjectsMap(t *testing.T) {
	b, err := json.Marshal(UsageSummaryResponse{
		SchemaVersion: 1,
		Projects:      map[string]export.ProjectMapEntry{},
	})
	require.NoError(t, err)

	assert.Contains(t, string(b), `"projects":{}`)
}

// assertUsageQueryCalls verifies how many times the usage handler
// queried the daily-usage and session-count store methods.
func assertUsageQueryCalls(
	t *testing.T, spy *usageSummaryCountsSpy,
	wantDaily, wantCounts, wantMatching int,
) {
	t.Helper()
	assert.Equal(t, wantDaily, spy.dailyCalls, "daily usage calls")
	assert.Equal(t, wantCounts, spy.countsCalls, "session count calls")
	assert.Equal(t, wantMatching, spy.matchingSessionCalls, "matching session calls")
}

func (s *usageSummaryCountsSpy) GetDailyUsage(
	_ context.Context, f db.UsageFilter,
) (db.DailyUsageResult, error) {
	s.dailyCalls++
	s.filters = append(s.filters, f)
	if len(s.result.Daily) == 0 && s.result.Totals == (db.UsageTotals{}) &&
		s.result.SessionCounts.Total == 0 && s.result.SessionCounts.ByProject == nil &&
		s.result.SessionCounts.ByAgent == nil {
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
	return s.result, nil
}

func (s *usageSummaryCountsSpy) GetUsageSessionCounts(
	_ context.Context, _ db.UsageFilter,
) (db.UsageSessionCounts, error) {
	s.countsCalls++
	return db.UsageSessionCounts{}, nil
}

func (s *usageSummaryCountsSpy) GetUsageMatchingSessionCount(
	_ context.Context, _ db.UsageFilter,
) (int, error) {
	s.matchingSessionCalls++
	return s.matchingSessionCount, nil
}

func TestUsageSummaryScansCurrentPeriodOnly(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s, "/api/v1/usage/summary?"+oneDayUsageRange)
	assertRecorderStatus(t, w, http.StatusOK)

	assertUsageQueryCalls(t, spy, 1, 0, 0)
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

	assertUsageQueryCalls(t, spy, 1, 0, 0)

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

	assertUsageQueryCalls(t, spy, 0, 0, 0)
}

func TestUsageComparisonNoDefaultRangeRequiresConcreteRange(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s,
		"/api/v1/usage/comparison?no_default_range=true&current_cost=3")
	assertRecorderStatus(t, w, http.StatusBadRequest)
	assert.Contains(t, w.Body.String(), "requires from and to")

	assertUsageQueryCalls(t, spy, 0, 0, 0)
}

func TestUsageComparisonAllowsZeroCurrentCost(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s,
		"/api/v1/usage/comparison?"+oneDayUsageRange+"&current_cost=0")
	assertRecorderStatus(t, w, http.StatusOK)

	assertUsageQueryCalls(t, spy, 1, 0, 0)
}

func TestUsageSummarySetsUnsupportedUsageForCopilotNoTokenData(t *testing.T) {
	spy := &usageSummaryCountsSpy{
		matchingSessionCount: 2,
		result: db.DailyUsageResult{
			Daily:  []db.DailyUsageEntry{{Date: "2024-06-01"}},
			Totals: db.UsageTotals{},
			SessionCounts: db.UsageSessionCounts{
				Total:     0,
				ByProject: map[string]int{},
				ByAgent:   map[string]int{},
			},
		},
	}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s,
		"/api/v1/usage/summary?"+oneDayUsageRange+"&agent=copilot")
	assertRecorderStatus(t, w, http.StatusOK)

	var resp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.UnsupportedUsage)
	assert.Equal(t,
		service.UnsupportedUsageKindCopilotNoTokenData,
		resp.UnsupportedUsage.Kind,
	)
	assertUsageQueryCalls(t, spy, 1, 0, 1)
}

func TestUsageSummarySetsUnsupportedUsageFromAgentCapability(t *testing.T) {
	parsertest.StubAgentDefs(t, parser.AgentDef{
		Type:        parser.AgentType("no-token-agent"),
		DisplayName: "No Token Agent",
		Usage: parser.UsageCapabilities{
			NoPerMessageTokenData: true,
		},
	})

	spy := &usageSummaryCountsSpy{
		matchingSessionCount: 1,
		result: db.DailyUsageResult{
			Daily:  []db.DailyUsageEntry{{Date: "2024-06-01"}},
			Totals: db.UsageTotals{},
			SessionCounts: db.UsageSessionCounts{
				Total:     0,
				ByProject: map[string]int{},
				ByAgent:   map[string]int{},
			},
		},
	}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s,
		"/api/v1/usage/summary?"+oneDayUsageRange+"&agent=no-token-agent")
	assertRecorderStatus(t, w, http.StatusOK)

	var resp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.UnsupportedUsage)
	assert.Equal(t,
		service.UnsupportedUsageKindNoTokenData,
		resp.UnsupportedUsage.Kind,
	)
	assertUsageQueryCalls(t, spy, 1, 0, 1)
}

// TestUsageSummaryKeepsGenericKindForNonCopilotAICreditAgent pins the
// Copilot-branded kind to Copilot identity: an agent that shares both of
// Copilot's usage capabilities must still surface the generic kind.
func TestUsageSummaryKeepsGenericKindForNonCopilotAICreditAgent(t *testing.T) {
	parsertest.StubAgentDefs(t, parser.AgentDef{
		Type:        parser.AgentType("credit-note-agent"),
		DisplayName: "Credit Note Agent",
		Usage: parser.UsageCapabilities{
			NoPerMessageTokenData: true,
			AICreditsDenominated:  true,
		},
	})

	spy := &usageSummaryCountsSpy{
		matchingSessionCount: 1,
		result: db.DailyUsageResult{
			Daily:  []db.DailyUsageEntry{{Date: "2024-06-01"}},
			Totals: db.UsageTotals{},
			SessionCounts: db.UsageSessionCounts{
				Total:     0,
				ByProject: map[string]int{},
				ByAgent:   map[string]int{},
			},
		},
	}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s,
		"/api/v1/usage/summary?"+oneDayUsageRange+"&agent=credit-note-agent")
	assertRecorderStatus(t, w, http.StatusOK)

	var resp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotNil(t, resp.UnsupportedUsage)
	assert.Equal(t,
		service.UnsupportedUsageKindNoTokenData,
		resp.UnsupportedUsage.Kind,
	)
	assertUsageQueryCalls(t, spy, 1, 0, 1)
}

func TestUsageSummarySkipsUnsupportedUsageForMixedAgentFilters(t *testing.T) {
	spy := &usageSummaryCountsSpy{
		matchingSessionCount: 2,
		result: db.DailyUsageResult{
			Daily:  []db.DailyUsageEntry{{Date: "2024-06-01"}},
			Totals: db.UsageTotals{},
			SessionCounts: db.UsageSessionCounts{
				Total:     0,
				ByProject: map[string]int{},
				ByAgent:   map[string]int{},
			},
		},
	}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s,
		"/api/v1/usage/summary?"+oneDayUsageRange+"&agent=copilot,claude")
	assertRecorderStatus(t, w, http.StatusOK)

	var resp UsageSummaryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Nil(t, resp.UnsupportedUsage)
	assertUsageQueryCalls(t, spy, 1, 0, 0)
}

func TestUsagePairwiseComparisonScansTwoDailyFilters(t *testing.T) {
	spy := &usageSummaryCountsSpy{}
	s := newRoutedTestServerWithStore(t, spy)

	w := serveGet(t, s,
		"/api/v1/usage/pairwise-comparison?"+oneDayUsageRange+
			"&left_dimension=model&left_value=claude-sonnet-4-20250514"+
			"&right_dimension=project&right_value=beta")
	assertRecorderStatus(t, w, http.StatusOK)

	assert.Equal(t, 2, spy.dailyCalls)
	require.Len(t, spy.filters, 2)
	assert.Equal(t, "claude-sonnet-4-20250514", spy.filters[0].Model)
	assert.Equal(t, "", spy.filters[0].Project)
	assert.Equal(t, "", spy.filters[1].Model)
	assert.Equal(t, "beta", spy.filters[1].Project)
	assert.False(t, spy.filters[0].SkipSessionCounts)
	assert.False(t, spy.filters[1].SkipSessionCounts)
}

func TestUsagePairwiseComparisonOpenAPIRequiresSideParams(t *testing.T) {
	s := testServer(t, 30)
	spec := readOpenAPISpec(t, s.Handler())
	op := requireOpenAPIOperation(t, spec, "get", "/api/v1/usage/pairwise-comparison")

	required := map[string]bool{}
	for _, parameter := range op.Parameters {
		if parameter.In == "query" {
			required[parameter.Name] = parameter.Required
		}
	}

	assert.True(t, required["left_dimension"])
	assert.True(t, required["left_value"])
	assert.True(t, required["right_dimension"])
	assert.True(t, required["right_value"])
}
