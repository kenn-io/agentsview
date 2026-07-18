package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

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
			m.Model = "gpt-5.1"
			m.TokenUsage = json.RawMessage(
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
		"cost_usd":            0.01134,
		"has_cost":            true,
		"models":              []any{"gpt-5.1"},
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
	assert.Equal(t, float64(1), got["breakdown_count"], "breakdown_count")
	assert.Equal(t, []any{
		map[string]any{
			"ordinal":                     float64(1),
			"message_ordinal":             float64(1),
			"source":                      "message",
			"label":                       "Prompt 2",
			"timestamp":                   tsSeed,
			"model":                       "gpt-5.1",
			"input_tokens":                float64(1000),
			"output_tokens":               float64(500),
			"cache_creation_input_tokens": float64(200),
			"cache_read_input_tokens":     float64(300),
			"cost_usd":                    0.01134,
			"has_cost":                    true,
		},
	}, got["breakdown"], "breakdown rows with ?breakdown=true")
}

func TestHandleSessionUsage_RollsUpExplicitSubagents(t *testing.T) {
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
	te.seedMessages(t, "root-rollup", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", "gpt-5.1"
		m.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	})
	te.seedMessages(t, "child-rollup", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", "gpt-5.1"
		m.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	})

	w := te.get(t, "/api/v1/sessions/root-rollup/usage?rollup=true")
	assertStatus(t, w, http.StatusOK)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, float64(1), got["rollup_subagent_count"])
	assert.Equal(t, true, got["has_rollup_cost"])
	assert.InDelta(t, 0.021, got["rollup_cost_usd"], 1e-9)
}

func TestHandleSessionUsage_RollupBreakdownIncludesRootRows(t *testing.T) {
	te := setup(t)
	seedSessionUsagePricing(t, te.db)
	te.seedSession(t, "root-rollup-breakdown", "project", 1, func(s *db.Session) {
		s.Agent = "codex"
	})
	te.seedSession(t, "child-rollup-breakdown", "project", 1, func(s *db.Session) {
		s.Agent = "codex"
		parent := "root-rollup-breakdown"
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
	})
	te.seedMessages(t, "root-rollup-breakdown", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", "gpt-5.1"
		m.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	})
	te.seedMessages(t, "child-rollup-breakdown", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", "gpt-5.1"
		m.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	})

	w := te.get(t, "/api/v1/sessions/root-rollup-breakdown/usage?rollup=true&breakdown=true")
	assertStatus(t, w, http.StatusOK)

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, float64(1), got["rollup_subagent_count"])
	assert.Equal(t, float64(1), got["breakdown_count"])
	assert.Len(t, got["breakdown"], 1)
}

func TestHandleSessionUsage_RollupTraversesContinuationAndDedupesSharedRows(t *testing.T) {
	te := setup(t)
	seedSessionUsagePricing(t, te.db)
	te.seedSession(t, "root-rollup-rework", "project", 1, func(s *db.Session) {
		s.Agent = "codex"
	})
	te.seedSession(t, "continuation-rollup-rework", "project", 1, func(s *db.Session) {
		parent := "root-rollup-rework"
		s.ParentSessionID = &parent
		s.RelationshipType = "continuation"
	})
	te.seedSession(t, "nested-rollup-rework", "project", 2, func(s *db.Session) {
		parent := "continuation-rollup-rework"
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
	})
	for _, id := range []string{"root-rollup-rework", "nested-rollup-rework"} {
		te.seedMessages(t, id, 2, func(i int, m *db.Message) {
			m.Role, m.Model = "assistant", "gpt-5.1"
			m.ClaudeMessageID = "shared-rollup-message"
			m.ClaudeRequestID = "shared-rollup-request"
			m.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
			if id == "nested-rollup-rework" && i == 1 {
				m.ClaudeMessageID = "unique-rollup-message"
				m.ClaudeRequestID = "unique-rollup-request"
			}
		})
	}

	w := te.get(t, "/api/v1/sessions/root-rollup-rework/usage?rollup=true")
	assertStatus(t, w, http.StatusOK)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, float64(1), got["rollup_subagent_count"])
	assert.Equal(t, true, got["has_rollup_cost"])
	assert.InDelta(t, 0.021, got["rollup_cost_usd"], 1e-9)
}

func TestHandleSessionUsage_RollupIncludesUntimedSubagentUsage(t *testing.T) {
	te := setup(t)
	seedSessionUsagePricing(t, te.db)
	te.seedSession(t, "root-rollup-untimed", "project", 1, func(s *db.Session) {
		s.Agent = "codex"
	})
	te.seedSession(t, "child-rollup-untimed", "project", 1, func(s *db.Session) {
		s.Agent = "codex"
		parent := "root-rollup-untimed"
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
	})
	te.seedMessages(t, "root-rollup-untimed", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", "gpt-5.1"
		m.Timestamp = ""
		m.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	})
	te.seedMessages(t, "child-rollup-untimed", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", "gpt-5.1"
		m.Timestamp = ""
		m.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	})

	w := te.get(t, "/api/v1/sessions/root-rollup-untimed/usage?rollup=true")
	assertStatus(t, w, http.StatusOK)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, float64(1), got["rollup_subagent_count"])
	assert.Equal(t, true, got["has_rollup_cost"])
	assert.InDelta(t, 0.021, got["rollup_cost_usd"], 1e-9)
}

func TestHandleSessionUsage_RollupPrefersRootForSharedDuplicateAtSameTimestamp(t *testing.T) {
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
			m.Role, m.Model = "assistant", "gpt-5.1"
			m.Timestamp = "2026-03-12T10:00:00Z"
			m.ClaudeMessageID = "shared-rollup-attribution"
			m.ClaudeRequestID = "shared-rollup-attribution-request"
			m.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
		})
	}

	w := te.get(t, "/api/v1/sessions/z-root-rollup-attribution/usage?rollup=true")
	assertStatus(t, w, http.StatusOK)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, float64(1), got["rollup_subagent_count"])
	assert.Equal(t, false, got["has_rollup_cost"])
	_, hasRollupCost := got["rollup_cost_usd"]
	assert.False(t, hasRollupCost)
	assert.InDelta(t, 0.0105, got["cost_usd"], 1e-9)
}

func TestHandleSessionUsage_IncompleteRollupOmitsPartialCost(t *testing.T) {
	te := setup(t)
	seedSessionUsagePricing(t, te.db)
	te.seedSession(t, "root-rollup-incomplete", "project", 1, func(s *db.Session) {
		s.Agent = "codex"
	})
	te.seedSession(t, "child-rollup-incomplete", "project", 1, func(s *db.Session) {
		s.Agent = "codex"
		parent := "root-rollup-incomplete"
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
	})
	te.seedMessages(t, "root-rollup-incomplete", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", "gpt-5.1"
		m.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	})
	te.seedMessages(t, "child-rollup-incomplete", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", "unknown-rollup-model"
		m.TokenUsage = json.RawMessage(`{"input_tokens":1000,"output_tokens":500}`)
	})

	w := te.get(t, "/api/v1/sessions/root-rollup-incomplete/usage?rollup=true")
	assertStatus(t, w, http.StatusOK)

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, float64(1), got["rollup_subagent_count"])
	assert.Equal(t, false, got["has_rollup_cost"])
	_, hasRollupCost := got["rollup_cost_usd"]
	assert.False(t, hasRollupCost)
	assert.InDelta(t, 0.0105, got["cost_usd"], 1e-9)
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
		"cost_usd":            float64(0),
		"has_cost":            false,
		"models":              []any{},
		"unpriced_models":     []any{},
		"breakdown_count":     float64(0),
		"breakdown":           []any{},
		"server_running":      true,
	}, got)
}

func TestHandleSessionUsage_BreakdownOrderingAndDedup(t *testing.T) {
	te := setup(t)
	seedSessionUsagePricing(t, te.db)
	te.seedSession(t, "codex:usage-breakdown", "my-project", 3,
		func(s *db.Session) {
			s.Agent = "codex"
			s.TotalOutputTokens = 1500
			s.PeakContextTokens = 4000
			s.HasTotalOutputTokens = true
			s.HasPeakContextTokens = true
		})
	te.seedMessages(t, "codex:usage-breakdown", 3,
		func(i int, m *db.Message) {
			switch i {
			case 0, 1:
				m.Role = "assistant"
				m.Model = "gpt-5.1"
				m.ClaudeMessageID = "msg-dup"
				m.ClaudeRequestID = "req-dup"
				m.TokenUsage = json.RawMessage(
					`{"input_tokens":1000,"output_tokens":500,` +
						`"cache_creation_input_tokens":200,` +
						`"cache_read_input_tokens":300}`,
				)
			}
		})
	ordinal := 1
	require.NoError(t, te.db.ReplaceSessionUsageEvents(
		"codex:usage-breakdown",
		[]db.UsageEvent{{
			SessionID:                "codex:usage-breakdown",
			MessageOrdinal:           &ordinal,
			Source:                   "step",
			Model:                    "gpt-5.1",
			InputTokens:              250,
			OutputTokens:             125,
			CacheCreationInputTokens: 50,
			CacheReadInputTokens:     25,
			OccurredAt:               "2026-05-20T10:40:00Z",
			DedupKey:                 "step:1",
		}},
	), "ReplaceSessionUsageEvents")

	usage, err := te.db.GetSessionUsage(context.Background(),
		"codex:usage-breakdown", true)
	require.NoError(t, err, "GetSessionUsage")
	require.NotNil(t, usage, "usage is nil")
	require.Len(t, usage.Breakdown, 2)
	assert.Equal(t, 1, usage.Breakdown[0].Ordinal)
	assert.Equal(t, "Prompt 1", usage.Breakdown[0].Label)
	assert.Equal(t, "message", usage.Breakdown[0].Source)
	assert.Equal(t, 1000, usage.Breakdown[0].InputTokens)
	assert.Equal(t, 500, usage.Breakdown[0].OutputTokens)
	assert.Equal(t, 200, usage.Breakdown[0].CacheCreationInputTokens)
	assert.Equal(t, 300, usage.Breakdown[0].CacheReadInputTokens)
	assert.Equal(t, 2, usage.Breakdown[1].Ordinal)
	assert.Equal(t, "Step 2", usage.Breakdown[1].Label)
	assert.Equal(t, "step", usage.Breakdown[1].Source)
	assert.Equal(t, 250, usage.Breakdown[1].InputTokens)
	assert.Equal(t, 125, usage.Breakdown[1].OutputTokens)
	assert.Equal(t, 50, usage.Breakdown[1].CacheCreationInputTokens)
	assert.Equal(t, 25, usage.Breakdown[1].CacheReadInputTokens)
	assert.InDelta(t, 0.01416, usage.CostUSD, 1e-9)
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
		ModelPattern:         "gpt-5.1",
		InputPerMTok:         3.0,
		OutputPerMTok:        15.0,
		CacheCreationPerMTok: 3.75,
		CacheReadPerMTok:     0.30,
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
