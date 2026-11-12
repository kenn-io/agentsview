package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestUsageLimitsReturnsLatestReportPerAccount(t *testing.T) {
	te := setup(t)
	empty := te.get(t, "/api/v1/usage/limits")
	require.Equal(t, http.StatusOK, empty.Code)
	assert.JSONEq(t, `{"limits":[]}`, empty.Body.String())

	weekly := func(used float64) *db.RateLimitWindow {
		minutes := int64(10080)
		return &db.RateLimitWindow{UsedPercent: used, WindowMinutes: &minutes, ResetsAt: "2026-11-17T04:43:21.000Z"}
	}
	require.NoError(t, te.db.InsertRateLimitSnapshots(t.Context(), []db.RateLimitSnapshot{
		{Agent: "codex", AccountKey: "k1", LimitID: "codex", PlanType: "pro", SessionID: "codex:a", ObservedAt: "2026-11-10T09:00:00.000Z", Primary: weekly(10)},
		{Agent: "codex", AccountKey: "k1", LimitID: "codex", PlanType: "pro", SessionID: "codex:b", ObservedAt: "2026-11-11T09:00:00.000Z", Primary: weekly(12)},
	}))

	w := te.get(t, "/api/v1/usage/limits")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"limits":[{
		"agent":"codex","account_key":"k1","limit_id":"codex","plan_type":"pro",
		"session_id":"codex:b","observed_at":"2026-11-11T09:00:00.000Z",
		"primary":{"used_percent":12,"window_minutes":10080,"resets_at":"2026-11-17T04:43:21.000Z"}
	}]}`, w.Body.String())
}
