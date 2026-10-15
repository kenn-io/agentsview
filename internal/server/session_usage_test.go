package server_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const controlledSessionUsageModel = "controlled-session-usage-model"

func TestHandleSessionUsage_PricedSession(t *testing.T) {
	te := setup(t)
	seedSessionUsagePricing(t, te.db)
	te.seedSession(t, "codex:usage-priced", "my-project", 2,
		func(s *db.Session) {
			s.Agent = "codex"
			s.TotalOutputTokens = 1234
			s.PeakContextTokens = 56789
			s.HasTotalOutputTokens = true
			s.HasPeakContextTokens = true
		})
	te.seedMessages(t, "codex:usage-priced", 2,
		func(i int, m *db.Message) {
			if i != 1 {
				return
			}
			m.Role = "assistant"
			m.Model = controlledSessionUsageModel
			m.TokenUsage = jsontext.Value(
				`{"input_tokens":1000,"output_tokens":500,` +
					`"cache_creation_input_tokens":200,` +
					`"cache_read_input_tokens":300}`,
			)
		})

	// Without ?breakdown=true the response carries only the count.
	w := te.get(t, "/api/v1/sessions/codex:usage-priced/usage")
	assertStatus(t, w, http.StatusOK)

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, map[string]any{
		"session_id":          "codex:usage-priced",
		"agent":               "codex",
		"project":             "my-project",
		"total_output_tokens": float64(1234),
		"peak_context_tokens": float64(56789),
		"has_token_data":      true,
		"cost":                map[string]any{"microdollars": float64(11340)},
		"has_cost":            true,
		"cost_usd":            0.01134,
		"cost_source":         "computed",
		"models":              []any{controlledSessionUsageModel},
		"unpriced_models":     []any{},
		"breakdown_count":     float64(1),
		"breakdown":           []any{},
		"server_running":      true,
	}, got)

	w = te.get(t,
		"/api/v1/sessions/codex:usage-priced/usage?breakdown=true")
	assertStatus(t, w, http.StatusOK)

	got = map[string]any{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.InDelta(t, float64(1), got["breakdown_count"], 0, "breakdown_count")
	assert.Equal(t, []any{
		map[string]any{
			"ordinal":                     float64(1),
			"message_ordinal":             float64(1),
			"source":                      "message",
			"label":                       "Prompt 2",
			"timestamp":                   tsSeed,
			"model":                       controlledSessionUsageModel,
			"input_tokens":                float64(1000),
			"output_tokens":               float64(500),
			"cache_creation_input_tokens": float64(200),
			"cache_read_input_tokens":     float64(300),
			"cost": map[string]any{
				"microdollars": float64(11340),
			},
			"has_cost": true,
		},
	}, got["breakdown"], "breakdown rows with ?breakdown=true")
}

func TestHandleSessionUsage_SubagentResponseModes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		query   string
		untimed bool
	}{
		{name: "untimed rollup", query: "rollup=true", untimed: true},
		{name: "breakdown rollup", query: "rollup=true&breakdown=true"},
		{name: "root only", query: "breakdown=true"},
		{name: "combined subagents", query: "breakdown=true&subagents=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := setup(t)
			seedSessionUsagePricing(t, te.db)
			te.seedSession(t, "root-rollup", "project", 1, func(s *db.Session) {
				s.Agent = "codex"
			})
			te.seedSession(t, "child-rollup", "project", 1, func(s *db.Session) {
				s.Agent = "codex"
				parent := "root-rollup"
				s.ParentSessionID = &parent
				s.RelationshipType = "subagent"
			})
			for _, id := range []string{"root-rollup", "child-rollup"} {
				te.seedMessages(t, id, 1, func(_ int, m *db.Message) {
					m.Role, m.Model = "assistant", controlledSessionUsageModel
					if tc.untimed {
						m.Timestamp = ""
					}
					m.TokenUsage = jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`)
				})
			}

			w := te.get(t, "/api/v1/sessions/root-rollup/usage?"+tc.query)
			assertStatus(t, w, http.StatusOK)
			var got map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
			switch tc.name {
			case "untimed rollup", "breakdown rollup":
				assert.InDelta(t, float64(1), got["rollup_subagent_count"], 0)
				assert.Equal(t, true, got["has_rollup_cost"])
				assert.Equal(t, map[string]any{"microdollars": float64(21000)}, got["rollup_cost"])
				if !tc.untimed {
					assert.InDelta(t, float64(1), got["breakdown_count"], 0)
					assert.Len(t, got["breakdown"], 1)
				}
			case "root only":
				assert.Equal(t, map[string]any{"microdollars": float64(10500)}, got["cost"])
				assert.InDelta(t, float64(1), got["breakdown_count"], 0)
				assert.NotContains(t, got, "subagent_count", "a request without the param must not gain the new field")
			case "combined subagents":
				assert.Equal(t, map[string]any{"microdollars": float64(21000)}, got["cost"], "cost covers the root and its subagent")
				assert.InDelta(t, 0.021, got["cost_usd"], 0, "cost_usd must reflect the combined subagent-inclusive cost")
				assert.InDelta(t, float64(1), got["subagent_count"], 0)
				assert.InDelta(t, float64(2), got["breakdown_count"], 0)
				assert.NotContains(t, got, "rollup_cost", "subagents=true must not emit the rollup fields the SPA reads")
				rows, ok := got["breakdown"].([]any)
				require.True(t, ok)
				require.Len(t, rows, 2)
				rootRow, ok := rows[0].(map[string]any)
				require.True(t, ok)
				assert.NotContains(t, rootRow, "subagent_session_id")
				childRow, ok := rows[1].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, "child-rollup", childRow["subagent_session_id"])
				assert.Equal(t, "message", childRow["source"])
			}
		})
	}
}

func TestHandleSessionUsage_RollupUsesCopilotReportedSessionCost(t *testing.T) {
	te := setup(t)
	seedSessionUsagePricing(t, te.db)
	te.seedSession(t, "copilot-rollup-root", "project", 1, func(s *db.Session) {
		s.Agent = "copilot"
	})
	te.seedSession(t, "copilot-rollup-child", "project", 1, func(s *db.Session) {
		s.Agent = "copilot"
		parent := "copilot-rollup-root"
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
	})
	reportedRootCost := money.MustParseDollars("0.03")
	reportedChildCost := money.MustParseDollars("0.02")
	require.NoError(t, te.db.ReplaceSessionUsageEvents(t.Context(), "copilot-rollup-root", []db.UsageEvent{
		{
			Source: "shutdown", Model: controlledSessionUsageModel,
			InputTokens: 1000, OutputTokens: 500,
			OccurredAt: tsSeed, DedupKey: "first",
		},
		{
			Source: "shutdown", Model: controlledSessionUsageModel,
			InputTokens: 1000, OutputTokens: 500,
			Cost: &reportedRootCost, CostStatus: "exact",
			CostSource: db.CopilotReportedCostSource,
			OccurredAt: tsSeed, DedupKey: "final",
		},
	}))
	require.NoError(t, te.db.ReplaceSessionUsageEvents(t.Context(), "copilot-rollup-child", []db.UsageEvent{{
		Source: "provider", Model: controlledSessionUsageModel,
		Cost: &reportedChildCost, CostStatus: "exact", CostSource: "provider",
		OccurredAt: tsSeed, DedupKey: "child",
	}}))

	w := te.get(t, "/api/v1/sessions/copilot-rollup-root/usage?rollup=true")
	assertStatus(t, w, http.StatusOK)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, true, got["has_rollup_cost"])
	assert.Equal(t, "reported", got["cost_source"])
	assert.Equal(t, "reported", got["rollup_cost_source"])
	assert.Equal(t, map[string]any{"microdollars": float64(50_000)},
		got["rollup_cost"])
}

func TestHandleSessionUsage_RollupSelectsRootFirstForSharedSourceUUID(t *testing.T) {
	te := setup(t)
	seedSessionUsagePricing(t, te.db)
	te.seedSession(t, "z-root-rollup-attribution", "project", 1, func(s *db.Session) {
		s.Agent = "codex"
	})
	te.seedSession(t, "a-child-rollup-attribution", "project", 1, func(s *db.Session) {
		s.Agent = "codex"
		parent := "z-root-rollup-attribution"
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
	})
	for _, id := range []string{"z-root-rollup-attribution", "a-child-rollup-attribution"} {
		te.seedMessages(t, id, 1, func(_ int, m *db.Message) {
			m.Role, m.Model = "assistant", controlledSessionUsageModel
			m.Timestamp = "2026-03-12T10:00:00Z"
			m.SourceUUID = "shared-rollup-attribution"
			m.TokenUsage = jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`)
		})
	}

	w := te.get(t, "/api/v1/sessions/z-root-rollup-attribution/usage?rollup=true")
	assertStatus(t, w, http.StatusOK)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, false, got["has_rollup_cost"])
	_, hasRollupCost := got["rollup_cost"]
	assert.False(t, hasRollupCost)
	assert.Equal(t, map[string]any{"microdollars": float64(10500)}, got["cost"])
}

func TestHandleSessionUsage_NoTokenOrCostData(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "codex:usage-empty", "quiet-project", 1,
		func(s *db.Session) {
			s.Agent = "codex"
		})

	w := te.get(t, "/api/v1/sessions/codex:usage-empty/usage")
	assertStatus(t, w, http.StatusOK)

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, map[string]any{
		"session_id":          "codex:usage-empty",
		"agent":               "codex",
		"project":             "quiet-project",
		"total_output_tokens": float64(0),
		"peak_context_tokens": float64(0),
		"has_token_data":      false,
		"cost":                map[string]any{"microdollars": float64(0)},
		"has_cost":            false,
		"models":              []any{},
		"unpriced_models":     []any{},
		"breakdown_count":     float64(0),
		"breakdown":           []any{},
		"server_running":      true,
	}, got)
}

func TestHandleSessionUsage_NotFound(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/sessions/missing/usage")
	assertStatus(t, w, http.StatusNotFound)
	assertSessionUsageError(t, w, "session_not_found", "session not found")
}

func TestHandleSessionUsage_RollupNotFound(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/sessions/missing/usage?rollup=true")
	assertStatus(t, w, http.StatusNotFound)
	assertSessionUsageError(t, w, "session_not_found", "session not found")
}

func TestHandleSessionUsage_DBError(t *testing.T) {
	te := setup(t)
	require.NoError(t, te.db.Close())

	w := te.get(t, "/api/v1/sessions/codex:usage-error/usage")
	assertStatus(t, w, http.StatusInternalServerError)
	assertSessionUsageError(t, w, "usage_query_failed", "failed to query session usage")
}

func TestHandleSessionUsage_RollupDBError(t *testing.T) {
	te := setup(t)
	require.NoError(t, te.db.Close())

	w := te.get(t, "/api/v1/sessions/codex:usage-error/usage?rollup=true")
	assertStatus(t, w, http.StatusInternalServerError)
	assertSessionUsageError(t, w, "usage_query_failed", "failed to query session usage")
}

func seedSessionUsagePricing(t *testing.T, d *db.DB) {
	t.Helper()
	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         controlledSessionUsageModel,
		InputPerMTok:         money.MustParseDollars("3.0"),
		OutputPerMTok:        money.MustParseDollars("15.0"),
		CacheCreationPerMTok: money.MustParseDollars("3.75"),
		CacheReadPerMTok:     money.MustParseDollars("0.30"),
	}}))
}

func assertSessionUsageError(
	t *testing.T,
	w *httptest.ResponseRecorder,
	code string,
	message string,
) {
	t.Helper()

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}, got)
}

func TestHandleSessionUsage_SubagentsRefreshesNewLocalTranscript(t *testing.T) {
	te := setup(t)
	projectDir := filepath.Join(te.claudeDir, "-home-proj")
	subagentsDir := filepath.Join(
		projectDir, "parent-uuid", "subagents")
	require.NoError(t, os.MkdirAll(subagentsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(projectDir, "parent-uuid.jsonl"),
		[]byte(testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-05-20T10:00:00Z", "delegate this").
			AddClaudeAssistant("2026-05-20T10:00:05Z", "on it").
			String()),
		0o644,
	))

	ctx := t.Context()
	require.NoError(t, te.engine.SyncSingleSessionContext(ctx, "parent-uuid"))
	require.NoError(t, os.WriteFile(
		filepath.Join(subagentsDir, "agent-worker1.jsonl"),
		[]byte(testjsonl.NewSessionBuilder().
			AddClaudeUserWithSessionID(
				"2026-05-20T10:01:00Z", "do the subtask", "parent-uuid").
			AddClaudeAssistant("2026-05-20T10:01:30Z", "subtask done").
			String()),
		0o644,
	))

	// The usage GET must stay side-effect-free even though it asks for
	// already-archived subagents to be included in the aggregate.
	w := te.get(t,
		"/api/v1/sessions/parent-uuid/usage?subagents=true")
	assertStatus(t, w, http.StatusOK)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.NotContains(t, got, "subagent_count")
	child, err := te.db.GetSession(ctx, "agent-worker1")
	require.NoError(t, err)
	assert.Nil(t, child, "usage GET unexpectedly synced the subagent transcript")

	body := `{"id":"parent-uuid","subagents":true}`
	foreign := httptest.NewRequestWithContext(ctx,
		http.MethodPost, "/api/v1/sessions/sync", strings.NewReader(body))
	foreign.Header.Set("Content-Type", "application/json")
	foreign.Header.Set("Origin", "http://evil-site.com")
	blocked := httptest.NewRecorder()
	te.handler.ServeHTTP(blocked, foreign)
	assertStatus(t, blocked, http.StatusForbidden)
	child, err = te.db.GetSession(ctx, "agent-worker1")
	require.NoError(t, err)
	assert.Nil(t, child, "foreign-origin sync unexpectedly wrote archive data")

	synced := te.post(t, "/api/v1/sessions/sync", body)
	assertStatus(t, synced, http.StatusOK)

	w = te.get(t,
		"/api/v1/sessions/parent-uuid/usage?subagents=true")
	assertStatus(t, w, http.StatusOK)
	got = nil
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.InDelta(t, float64(1), got["subagent_count"], 0)

	child, err = te.db.GetSession(ctx, "agent-worker1")
	require.NoError(t, err)
	require.NotNil(t, child, "subagent transcript was not synced")
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "parent-uuid", *child.ParentSessionID)
}

func TestHandleSessionUsage_SubagentRefreshFailureUsesArchivedUsage(t *testing.T) {
	te := setup(t)
	missingPath := filepath.Join(
		te.claudeDir, "-home-proj", "missing-parent.jsonl")
	te.seedSession(t, "missing-parent", "project", 1, func(s *db.Session) {
		s.Agent = "claude"
		s.FilePath = &missingPath
	})
	te.seedMessages(t, "missing-parent", 1, func(_ int, m *db.Message) {
		m.Role = "assistant"
		m.Model = "claude-sonnet-4-5"
		m.TokenUsage = jsontext.Value(
			`{"input_tokens":1000,"output_tokens":500}`)
	})

	w := te.get(t,
		"/api/v1/sessions/missing-parent/usage?subagents=true")
	assertStatus(t, w, http.StatusOK)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "missing-parent", got["session_id"])
	assert.InDelta(t, float64(1), got["breakdown_count"], 0)
}

// TestHandleSessionUsage_BreakdownRoundTripsWebSearchRequests pins that the
// REST breakdown rows carry `web_search_requests`, matching the CLI/local
// (db.SessionUsageBreakdownEntry) shape documented in docs/session-api.md.
// Before this fix, sessionUsageBreakdownResponse had no field to copy it
// into, so the REST endpoint silently dropped the count on every row.
func TestHandleSessionUsage_BreakdownRoundTripsWebSearchRequests(t *testing.T) {
	te := setup(t)
	seedSessionUsagePricing(t, te.db)
	te.seedSession(t, "codex:usage-websearch", "my-project", 2,
		func(s *db.Session) {
			s.Agent = "codex"
		})
	te.seedMessages(t, "codex:usage-websearch", 2,
		func(i int, m *db.Message) {
			m.Role = "assistant"
			m.Model = controlledSessionUsageModel
			if i == 0 {
				m.TokenUsage = jsontext.Value(
					`{"input_tokens":1000,"output_tokens":500,` +
						`"server_tool_use":{"web_search_requests":3,` +
						`"web_fetch_requests":0}}`)
				return
			}
			m.TokenUsage = jsontext.Value(
				`{"input_tokens":1000,"output_tokens":500}`)
		})

	w := te.get(t,
		"/api/v1/sessions/codex:usage-websearch/usage?breakdown=true")
	assertStatus(t, w, http.StatusOK)

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	rows, ok := got["breakdown"].([]any)
	require.True(t, ok)
	require.Len(t, rows, 2)

	searchedRow, ok := rows[0].(map[string]any)
	require.True(t, ok)
	assert.InDelta(t, float64(3), searchedRow["web_search_requests"], 0,
		"a row with billed web searches must round-trip the count")

	noSearchRow, ok := rows[1].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, noSearchRow, "web_search_requests",
		"a row with no web searches must omit the field, not send zero")
}
