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
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(map[string]any{
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
	require.NoError(json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(float64(1), got["breakdown_count"], "breakdown_count")
	assert.Equal([]any{
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

func TestHandleSessionUsage_RollsUpExplicitSubagents(t *testing.T) {
	assert := assert.New(t)

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
		m.Role, m.Model = "assistant", controlledSessionUsageModel
		m.TokenUsage = jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`)
	})
	te.seedMessages(t, "child-rollup", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", controlledSessionUsageModel
		m.TokenUsage = jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`)
	})

	w := te.get(t, "/api/v1/sessions/root-rollup/usage?rollup=true")
	assertStatus(t, w, http.StatusOK)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(float64(1), got["rollup_subagent_count"])
	assert.Equal(true, got["has_rollup_cost"])
	assert.Equal(map[string]any{"microdollars": float64(21000)},
		got["rollup_cost"])
}

func TestHandleSessionUsage_RollupUsesCopilotReportedSessionCost(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(te.db.ReplaceSessionUsageEvents("copilot-rollup-root", []db.UsageEvent{
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
	require.NoError(te.db.ReplaceSessionUsageEvents("copilot-rollup-child", []db.UsageEvent{{
		Source: "provider", Model: controlledSessionUsageModel,
		Cost: &reportedChildCost, CostStatus: "exact", CostSource: "provider",
		OccurredAt: tsSeed, DedupKey: "child",
	}}))

	w := te.get(t, "/api/v1/sessions/copilot-rollup-root/usage?rollup=true")
	assertStatus(t, w, http.StatusOK)
	var got map[string]any
	require.NoError(json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(true, got["has_rollup_cost"])
	assert.Equal("reported", got["cost_source"])
	assert.Equal("reported", got["rollup_cost_source"])
	assert.Equal(map[string]any{"microdollars": float64(50_000)},
		got["rollup_cost"])
}

func TestHandleSessionUsage_RollupBreakdownIncludesRootRows(t *testing.T) {
	assert := assert.New(t)

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
		m.Role, m.Model = "assistant", controlledSessionUsageModel
		m.TokenUsage = jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`)
	})
	te.seedMessages(t, "child-rollup-breakdown", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", controlledSessionUsageModel
		m.TokenUsage = jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`)
	})

	w := te.get(t, "/api/v1/sessions/root-rollup-breakdown/usage?rollup=true&breakdown=true")
	assertStatus(t, w, http.StatusOK)

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(float64(1), got["rollup_subagent_count"])
	assert.Equal(float64(1), got["breakdown_count"])
	assert.Len(got["breakdown"], 1)
}

func TestHandleSessionUsage_RollupTraversesContinuationAndDedupesSharedRows(t *testing.T) {
	assert := assert.New(t)

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
			m.Role, m.Model = "assistant", controlledSessionUsageModel
			m.ClaudeMessageID = "shared-rollup-message"
			m.ClaudeRequestID = "shared-rollup-request"
			m.TokenUsage = jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`)
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
	assert.Equal(float64(1), got["rollup_subagent_count"])
	assert.Equal(true, got["has_rollup_cost"])
	assert.Equal(map[string]any{"microdollars": float64(21000)},
		got["rollup_cost"])
}

func TestHandleSessionUsage_RollupIncludesUntimedSubagentUsage(t *testing.T) {
	assert := assert.New(t)

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
		m.Role, m.Model = "assistant", controlledSessionUsageModel
		m.Timestamp = ""
		m.TokenUsage = jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`)
	})
	te.seedMessages(t, "child-rollup-untimed", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", controlledSessionUsageModel
		m.Timestamp = ""
		m.TokenUsage = jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`)
	})

	w := te.get(t, "/api/v1/sessions/root-rollup-untimed/usage?rollup=true")
	assertStatus(t, w, http.StatusOK)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(float64(1), got["rollup_subagent_count"])
	assert.Equal(true, got["has_rollup_cost"])
	assert.Equal(map[string]any{"microdollars": float64(21000)},
		got["rollup_cost"])
}

func TestHandleSessionUsage_RollupPrefersRootForSharedDuplicateAtSameTimestamp(t *testing.T) {
	assert := assert.New(t)

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
			m.ClaudeMessageID = "shared-rollup-attribution"
			m.ClaudeRequestID = "shared-rollup-attribution-request"
			m.TokenUsage = jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`)
		})
	}

	w := te.get(t, "/api/v1/sessions/z-root-rollup-attribution/usage?rollup=true")
	assertStatus(t, w, http.StatusOK)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(float64(1), got["rollup_subagent_count"])
	assert.Equal(false, got["has_rollup_cost"])
	_, hasRollupCost := got["rollup_cost"]
	assert.False(hasRollupCost)
	assert.Equal(map[string]any{"microdollars": float64(10500)}, got["cost"])
}

func TestHandleSessionUsage_IncompleteRollupOmitsPartialCost(t *testing.T) {
	assert := assert.New(t)

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
		m.Role, m.Model = "assistant", controlledSessionUsageModel
		m.TokenUsage = jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`)
	})
	te.seedMessages(t, "child-rollup-incomplete", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", "unknown-rollup-model"
		m.TokenUsage = jsontext.Value(`{"input_tokens":1000,"output_tokens":500}`)
	})

	w := te.get(t, "/api/v1/sessions/root-rollup-incomplete/usage?rollup=true")
	assertStatus(t, w, http.StatusOK)

	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(float64(1), got["rollup_subagent_count"])
	assert.Equal(false, got["has_rollup_cost"])
	_, hasRollupCost := got["rollup_cost"]
	assert.False(hasRollupCost)
	assert.Equal(map[string]any{"microdollars": float64(10500)}, got["cost"])
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

func TestHandleSessionUsage_BreakdownOrderingAndDedup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
				m.Model = controlledSessionUsageModel
				m.ClaudeMessageID = "msg-dup"
				m.ClaudeRequestID = "req-dup"
				m.TokenUsage = jsontext.Value(
					`{"input_tokens":1000,"output_tokens":500,` +
						`"cache_creation_input_tokens":200,` +
						`"cache_read_input_tokens":300}`,
				)
			}
		})
	ordinal := 1
	require.NoError(te.db.ReplaceSessionUsageEvents(
		"codex:usage-breakdown",
		[]db.UsageEvent{{
			SessionID:                "codex:usage-breakdown",
			MessageOrdinal:           &ordinal,
			Source:                   "step",
			Model:                    controlledSessionUsageModel,
			InputTokens:              250,
			OutputTokens:             125,
			CacheCreationInputTokens: 50,
			CacheReadInputTokens:     25,
			OccurredAt:               "2026-05-20T10:40:00Z",
			DedupKey:                 "step:1",
		}},
	), "ReplaceSessionUsageEvents")

	usage, err := te.db.GetSessionUsage(t.Context(),
		"codex:usage-breakdown", true)
	require.NoError(err, "GetSessionUsage")
	require.NotNil(usage, "usage is nil")
	require.Len(usage.Breakdown, 2)
	assert.Equal(1, usage.Breakdown[0].Ordinal)
	assert.Equal("Prompt 2", usage.Breakdown[0].Label)
	assert.Equal("message", usage.Breakdown[0].Source)
	assert.Equal(1000, usage.Breakdown[0].InputTokens)
	assert.Equal(500, usage.Breakdown[0].OutputTokens)
	assert.Equal(200, usage.Breakdown[0].CacheCreationInputTokens)
	assert.Equal(300, usage.Breakdown[0].CacheReadInputTokens)
	assert.Equal(2, usage.Breakdown[1].Ordinal)
	assert.Equal("Step 2", usage.Breakdown[1].Label)
	assert.Equal("step", usage.Breakdown[1].Source)
	assert.Equal(250, usage.Breakdown[1].InputTokens)
	assert.Equal(125, usage.Breakdown[1].OutputTokens)
	assert.Equal(50, usage.Breakdown[1].CacheCreationInputTokens)
	assert.Equal(25, usage.Breakdown[1].CacheReadInputTokens)
	assert.Equal(money.MustParseDollars("0.01416"), usage.Cost)
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

// TestHandleSessionUsage_SubagentsParamCombinesInPlace covers the additive
// `subagents=true` param the CLI uses: it folds descendant usage into the
// primary totals and breakdown rather than adding parallel rollup_* fields.
func TestHandleSessionUsage_SubagentsParamCombinesInPlace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	te := setup(t)
	seedSessionUsagePricing(t, te.db)
	te.seedSession(t, "subagents-root", "project", 1, func(s *db.Session) {
		s.Agent = "codex"
	})
	te.seedSession(t, "subagents-child", "project", 1, func(s *db.Session) {
		s.Agent = "codex"
		parent := "subagents-root"
		s.ParentSessionID = &parent
		s.RelationshipType = "subagent"
	})
	te.seedMessages(t, "subagents-root", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", controlledSessionUsageModel
		m.TokenUsage = jsontext.Value(
			`{"input_tokens":1000,"output_tokens":500}`)
	})
	te.seedMessages(t, "subagents-child", 1, func(_ int, m *db.Message) {
		m.Role, m.Model = "assistant", controlledSessionUsageModel
		m.TokenUsage = jsontext.Value(
			`{"input_tokens":1000,"output_tokens":500}`)
	})

	// Without the param the endpoint stays own-session.
	w := te.get(t, "/api/v1/sessions/subagents-root/usage?breakdown=true")
	assertStatus(t, w, http.StatusOK)
	var own map[string]any
	require.NoError(json.Unmarshal(w.Body.Bytes(), &own))
	assert.Equal(map[string]any{"microdollars": float64(10500)},
		own["cost"])
	assert.Equal(float64(1), own["breakdown_count"])
	assert.NotContains(own, "subagent_count",
		"a request without the param must not gain the new field")

	w = te.get(t,
		"/api/v1/sessions/subagents-root/usage?breakdown=true&subagents=true")
	assertStatus(t, w, http.StatusOK)
	var got map[string]any
	require.NoError(json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(map[string]any{"microdollars": float64(21000)},
		got["cost"], "cost covers the root and its subagent")
	assert.Equal(0.021, got["cost_usd"],
		"cost_usd must reflect the combined subagent-inclusive cost")
	assert.Equal(float64(1), got["subagent_count"])
	assert.Equal(float64(2), got["breakdown_count"])
	assert.NotContains(got, "rollup_cost",
		"subagents=true must not emit the rollup fields the SPA reads")

	rows, ok := got["breakdown"].([]any)
	require.True(ok)
	require.Len(rows, 2)
	rootRow, ok := rows[0].(map[string]any)
	require.True(ok)
	assert.NotContains(rootRow, "subagent_session_id")
	childRow, ok := rows[1].(map[string]any)
	require.True(ok)
	assert.Equal("subagents-child", childRow["subagent_session_id"])
	assert.Equal("message", childRow["source"])
}

func TestHandleSessionUsage_SubagentsRefreshesNewLocalTranscript(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	te := setup(t)
	projectDir := filepath.Join(te.claudeDir, "-home-proj")
	subagentsDir := filepath.Join(
		projectDir, "parent-uuid", "subagents")
	require.NoError(os.MkdirAll(subagentsDir, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(projectDir, "parent-uuid.jsonl"),
		[]byte(testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-05-20T10:00:00Z", "delegate this").
			AddClaudeAssistant("2026-05-20T10:00:05Z", "on it").
			String()),
		0o644,
	))

	ctx := t.Context()
	require.NoError(te.engine.SyncSingleSessionContext(ctx, "parent-uuid"))
	require.NoError(os.WriteFile(
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
	require.NoError(json.Unmarshal(w.Body.Bytes(), &got))
	assert.NotContains(got, "subagent_count")
	child, err := te.db.GetSession(ctx, "agent-worker1")
	require.NoError(err)
	assert.Nil(child, "usage GET unexpectedly synced the subagent transcript")

	body := `{"id":"parent-uuid","subagents":true}`
	foreign := httptest.NewRequestWithContext(ctx,
		http.MethodPost, "/api/v1/sessions/sync", strings.NewReader(body))
	foreign.Header.Set("Content-Type", "application/json")
	foreign.Header.Set("Origin", "http://evil-site.com")
	blocked := httptest.NewRecorder()
	te.handler.ServeHTTP(blocked, foreign)
	assertStatus(t, blocked, http.StatusForbidden)
	child, err = te.db.GetSession(ctx, "agent-worker1")
	require.NoError(err)
	assert.Nil(child, "foreign-origin sync unexpectedly wrote archive data")

	synced := te.post(t, "/api/v1/sessions/sync", body)
	assertStatus(t, synced, http.StatusOK)

	w = te.get(t,
		"/api/v1/sessions/parent-uuid/usage?subagents=true")
	assertStatus(t, w, http.StatusOK)
	got = nil
	require.NoError(json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(float64(1), got["subagent_count"])

	child, err = te.db.GetSession(ctx, "agent-worker1")
	require.NoError(err)
	require.NotNil(child, "subagent transcript was not synced")
	require.NotNil(child.ParentSessionID)
	assert.Equal("parent-uuid", *child.ParentSessionID)
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
	assert.Equal(t, float64(1), got["breakdown_count"])
}

// TestHandleSessionUsage_BreakdownRoundTripsWebSearchRequests pins that the
// REST breakdown rows carry `web_search_requests`, matching the CLI/local
// (db.SessionUsageBreakdownEntry) shape documented in docs/session-api.md.
// Before this fix, sessionUsageBreakdownResponse had no field to copy it
// into, so the REST endpoint silently dropped the count on every row.
func TestHandleSessionUsage_BreakdownRoundTripsWebSearchRequests(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(json.Unmarshal(w.Body.Bytes(), &got))
	rows, ok := got["breakdown"].([]any)
	require.True(ok)
	require.Len(rows, 2)

	searchedRow, ok := rows[0].(map[string]any)
	require.True(ok)
	assert.Equal(float64(3), searchedRow["web_search_requests"],
		"a row with billed web searches must round-trip the count")

	noSearchRow, ok := rows[1].(map[string]any)
	require.True(ok)
	assert.NotContains(noSearchRow, "web_search_requests",
		"a row with no web searches must omit the field, not send zero")
}
