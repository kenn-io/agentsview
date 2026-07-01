package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
)

// tokenUsageJSON is a valid token_usage blob for test messages.
var tokenUsageJSON = json.RawMessage(
	`{"input_tokens":100,"output_tokens":50,` +
		`"cache_creation_input_tokens":10,` +
		`"cache_read_input_tokens":20}`,
)

func TestParseUsageFilterDefaults(t *testing.T) {
	te := setup(t)

	// No params at all -> defaults should kick in.
	w := te.get(t, "/api/v1/usage/summary")
	assertStatus(t, w, http.StatusOK)

	resp := decode[service.UsageSummaryResult](t, w)
	assert.NotEmpty(t, resp.From, "expected defaulted From")
	assert.NotEmpty(t, resp.To, "expected defaulted To")
	// from should be ~30 days before to.
	assert.Less(t, resp.From, resp.To)
}

func TestParseUsageFilterExplicit(t *testing.T) {
	te := setup(t)

	w := te.get(t, buildPathURL("/api/v1/usage/summary",
		map[string]string{
			"from":     "2024-06-01",
			"to":       "2024-06-15",
			"timezone": "America/New_York",
			"project":  "myproj",
			"agent":    "claude",
			"model":    "claude-sonnet-4-20250514",
		}))
	assertStatus(t, w, http.StatusOK)

	resp := decode[service.UsageSummaryResult](t, w)
	assert.Equal(t, "2024-06-01", resp.From)
	assert.Equal(t, "2024-06-15", resp.To)
}

func TestParseUsageFilterDefaultsIncludeOneShot(t *testing.T) {
	te := setup(t)

	te.seedSession(t, "usage-one-shot", "alpha", 1,
		func(sess *db.Session) {
			ts := "2024-06-01T09:00:00Z"
			sess.Agent = "claude"
			sess.StartedAt = &ts
			sess.EndedAt = &ts
			sess.UserMessageCount = 1
		},
	)
	te.seedMessages(t, "usage-one-shot", 1,
		func(_ int, m *db.Message) {
			m.Role = "assistant"
			m.Timestamp = "2024-06-01T09:00:00Z"
			m.Model = "claude-sonnet-4-20250514"
			m.TokenUsage = tokenUsageJSON
		},
	)

	w := te.get(t, buildPathURL("/api/v1/usage/summary",
		map[string]string{
			"from":     "2024-06-01",
			"to":       "2024-06-02",
			"timezone": "UTC",
		}))
	assertStatus(t, w, http.StatusOK)

	resp := decode[service.UsageSummaryResult](t, w)
	require.Equal(t, 1, resp.SessionCounts.Total)
}

func TestParseUsageFilterInvalidDate(t *testing.T) {
	te := setup(t)

	w := te.get(t, buildPathURL("/api/v1/usage/summary",
		map[string]string{"from": "yesterday"}))
	assertStatus(t, w, http.StatusBadRequest)
}

// seedUsageEnv populates sessions with token_usage data for
// usage endpoint testing.
func seedUsageEnv(t *testing.T, te *testEnv) {
	t.Helper()

	type entry struct {
		id, project, agent, started string
		msgs                        int
	}
	entries := []entry{
		{"u1", "alpha", "claude", "2024-06-01T09:00:00Z", 4},
		{"u2", "beta", "codex", "2024-06-02T10:00:00Z", 4},
	}

	for _, e := range entries {
		te.seedSession(t, e.id, e.project, e.msgs,
			func(sess *db.Session) {
				sess.Agent = e.agent
				sess.StartedAt = &e.started
				sess.EndedAt = &e.started
				sess.FirstMessage = new("Usage test")
			},
		)
		started := e.started
		te.seedMessages(t, e.id, e.msgs,
			func(i int, m *db.Message) {
				m.Timestamp = started
				if m.Role == "assistant" {
					m.Model = "claude-sonnet-4-20250514"
					m.TokenUsage = tokenUsageJSON
				}
			},
		)
	}
}

func seedCopilotNoTokenSession(t *testing.T, te *testEnv, id string) {
	t.Helper()
	te.seedSession(t, id, "alpha", 1, func(sess *db.Session) {
		ts := "2024-06-01T09:00:00Z"
		sess.Agent = "copilot"
		sess.StartedAt = &ts
		sess.EndedAt = &ts
		sess.UserMessageCount = 1
	})
	te.seedMessages(t, id, 1, func(_ int, m *db.Message) {
		m.Role = "assistant"
		m.Timestamp = "2024-06-01T09:00:00Z"
		m.Model = "gpt-5.3-codex"
		m.TokenUsage = nil
	})
}

func TestHandleUsageSummaryJSONShape(t *testing.T) {
	te := setup(t)
	seedUsageEnv(t, te)

	w := te.get(t, buildPathURL("/api/v1/usage/summary",
		map[string]string{
			"from":     "2024-06-01",
			"to":       "2024-06-03",
			"timezone": "UTC",
		}))
	assertStatus(t, w, http.StatusOK)

	// Verify all expected top-level keys exist.
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))

	required := []string{
		"from", "to", "totals", "daily",
		"projectTotals", "modelTotals", "agentTotals",
		"sessionCounts", "cacheStats",
	}
	for _, key := range required {
		assert.Contains(t, raw, key, "missing key in response")
	}

	resp := decode[service.UsageSummaryResult](t, w)
	assert.NotEmpty(t, resp.Daily)
	assert.NotEmpty(t, resp.ProjectTotals)
	assert.NotEmpty(t, resp.ModelTotals)
	assert.NotEmpty(t, resp.AgentTotals)
}

func TestHandleUsageSummaryIncludesUnsupportedCopilotSignal(t *testing.T) {
	te := setup(t)
	seedCopilotNoTokenSession(t, te, "copilot-unsupported")

	w := te.get(t, buildPathURL("/api/v1/usage/summary",
		map[string]string{
			"from":     "2024-06-01",
			"to":       "2024-06-01",
			"timezone": "UTC",
			"agent":    "copilot",
		}))
	assertStatus(t, w, http.StatusOK)

	resp := decode[server.UsageSummaryResponse](t, w)
	require.NotNil(t, resp.UnsupportedUsage)
	assert.Equal(t, service.UnsupportedUsageKindCopilotNoTokenData, resp.UnsupportedUsage.Kind)
	assert.Equal(t, 0, resp.SessionCounts.Total)
	assert.Equal(t, 0.0, resp.Totals.TotalCost)
}

func TestHandleUsageSummarySkipsUnsupportedCopilotSignalForMixedFilters(t *testing.T) {
	te := setup(t)
	seedCopilotNoTokenSession(t, te, "copilot-unsupported")
	seedUsageEnv(t, te)

	w := te.get(t, buildPathURL("/api/v1/usage/summary",
		map[string]string{
			"from":     "2024-06-01",
			"to":       "2024-06-02",
			"timezone": "UTC",
			"agent":    "copilot,claude",
		}))
	assertStatus(t, w, http.StatusOK)

	resp := decode[server.UsageSummaryResponse](t, w)
	assert.Nil(t, resp.UnsupportedUsage)
}

func TestHandleUsageSummaryIncludesCursorUsageEvents(t *testing.T) {
	te := setup(t)

	require.NoError(t, te.db.InsertCursorUsageEvents([]db.CursorUsageEvent{{
		OccurredAt:       "2026-05-14T10:05:00Z",
		Model:            "claude-4.6-opus-high-thinking",
		Kind:             "USAGE_EVENT_KIND_USAGE_BASED",
		InputTokens:      1234,
		OutputTokens:     567,
		CacheWriteTokens: 0,
		CacheReadTokens:  8901,
		ChargedCents:     15.66,
		CursorTokenFee:   3.32,
		UserID:           "152683922",
		UserEmail:        "member@example.com",
		IsHeadless:       false,
	}}))

	w := te.get(t, buildPathURL("/api/v1/usage/summary",
		map[string]string{
			"from":     "2026-05-14",
			"to":       "2026-05-14",
			"timezone": "UTC",
		}))
	assertStatus(t, w, http.StatusOK)

	resp := decode[server.UsageSummaryResponse](t, w)
	require.Len(t, resp.Daily, 1)
	assert.InDelta(t, 0.1566, resp.Totals.TotalCost, 1e-9)
	require.NotEmpty(t, resp.AgentTotals)
	assert.Equal(t, "cursor", resp.AgentTotals[0].Agent)
}

func TestHandleUsageTopSessionsEmpty(t *testing.T) {
	te := setup(t)

	w := te.get(t, buildPathURL(
		"/api/v1/usage/top-sessions",
		map[string]string{
			"from":     "2024-06-01",
			"to":       "2024-06-03",
			"timezone": "UTC",
		}))
	assertStatus(t, w, http.StatusOK)

	var entries []db.TopSessionEntry
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &entries))
	assert.NotNil(t, entries, "expected non-null JSON array")
}

func TestHandleUsageTopSessionsLimit(t *testing.T) {
	te := setup(t)
	seedUsageEnv(t, te)

	w := te.get(t, buildPathURL(
		"/api/v1/usage/top-sessions",
		map[string]string{
			"from":     "2024-06-01",
			"to":       "2024-06-03",
			"timezone": "UTC",
			"limit":    "1",
		}))
	assertStatus(t, w, http.StatusOK)

	var entries []db.TopSessionEntry
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &entries))
	assert.LessOrEqual(t, len(entries), 1)
}

// TestUsageSummaryErrorRedaction verifies internal errors
// don't leak DB details.
func TestUsageSummaryErrorRedaction(t *testing.T) {
	te := setup(t)
	te.db.Close()

	w := te.get(t, buildPathURL("/api/v1/usage/summary",
		map[string]string{
			"from": "2024-06-01",
			"to":   "2024-06-03",
		}))
	assertStatus(t, w, http.StatusInternalServerError)
}

// Verify the route is actually registered by checking we
// don't get a 404 for usage endpoints.
func TestUsageRoutesRegistered(t *testing.T) {
	te := setup(t)

	endpoints := []string{
		"/api/v1/usage/summary",
		"/api/v1/usage/top-sessions",
	}
	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			req := httptest.NewRequest(
				http.MethodGet, ep, nil,
			)
			w := httptest.NewRecorder()
			te.handler.ServeHTTP(w, req)
			assert.NotEqual(t, http.StatusNotFound, w.Code, "%s returned 404", ep)
		})
	}
}
