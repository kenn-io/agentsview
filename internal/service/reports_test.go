package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/service"
)

func TestHTTPReportServicePreservesFiltersAndAuthentication(t *testing.T) {
	var gotQuery, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/analytics/summary", r.URL.Path)
		gotQuery, gotAuth = r.URL.RawQuery, r.Header.Get("Authorization")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"total_sessions": 3,
			"agents":         map[string]any{"codex": map[string]int{"sessions": 3, "messages": 12}},
		}))
	}))
	t.Cleanup(server.Close)

	backend := service.NewHTTPBackend(server.URL, "report-token", false)
	reports, ok := backend.(service.ReportService)
	require.True(t, ok)
	result, err := reports.AnalyticsReport(context.Background(), service.ReportFilter{
		From: "2026-06-01", To: "2026-06-30", Timezone: "Asia/Shanghai",
		Project: "agentsview", IncludeAutomated: true,
	})

	require.NoError(t, err)
	assert.Equal(t, 3, result.TotalSessions)
	assert.Equal(t, "Bearer report-token", gotAuth)
	assert.Contains(t, gotQuery, "from=2026-06-01")
	assert.Contains(t, gotQuery, "include_automated=true")
	assert.Contains(t, gotQuery, "project=agentsview")
}

func TestReportFilterRejectsInvertedDateRange(t *testing.T) {
	backend := service.NewDirectBackend(nil, nil)
	reports, ok := backend.(service.ReportService)
	require.True(t, ok)

	_, err := reports.AnalyticsReport(context.Background(), service.ReportFilter{
		From: "2026-07-01", To: "2026-06-01",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid report date range")
}
