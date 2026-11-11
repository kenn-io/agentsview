package servicehttp_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

// The daemon client rebuilds the cost comparison from the breakdown and
// decodes the agent's session total, so daemon and direct reads agree.
func TestSessionUsageKeepsAgentFiguresOverHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !assert.Equal(t, "/api/v1/sessions/claude:s1/usage", r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"session_id":"claude:s1","agent":"claude","project":"p",
			"total_output_tokens":500,"peak_context_tokens":1000,"has_token_data":true,
			"cost":{"microdollars":2000},"has_cost":true,"models":["m"],
			"unpriced_models":[],"breakdown_count":1,"server_running":true,
			"breakdown":[{"ordinal":1,"source":"message","label":"Prompt 1",
				"timestamp":"2026-08-12T10:00:00Z","model":"m","input_tokens":1000,
				"output_tokens":500,"cache_creation_input_tokens":0,
				"cache_read_input_tokens":0,"cost":{"microdollars":2000},"has_cost":true,
				"recorded_cost":{"microdollars":9000}}],
			"session_total":{"source":"claude-cost-state","subagent_scope":"included",
				"cost":{"microdollars":750000},"incomplete":true,
				"models":[{"model":"m","input_tokens":1500,"output_tokens":530,
					"cache_creation_input_tokens":0,"cache_read_input_tokens":0,
					"cost":{"microdollars":750000}}]}
		}`))
	}))
	t.Cleanup(server.Close)
	backend := servicehttp.NewHTTPBackend(server.URL, "", true, "")

	got, err := backend.SessionUsage(t.Context(), service.SessionUsageRequest{ID: "claude:s1", OwnOnly: true})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, &db.SessionRecordedCost{
		Recorded: money.Money{Microdollars: 9000}, Computed: money.Money{Microdollars: 2000},
		Difference: money.Money{Microdollars: -7000}, Rows: 1,
	}, got.RecordedCost)
	assert.Equal(t, &db.SessionUsageTotal{
		Source: "claude-cost-state", SubagentScope: "included",
		Cost: &money.Money{Microdollars: 750000}, Incomplete: true,
		Models: []db.SessionUsageTotalModel{{
			Model: "m", InputTokens: 1500, OutputTokens: 530,
			Cost: &money.Money{Microdollars: 750000},
		}},
	}, got.SessionTotal)
}
