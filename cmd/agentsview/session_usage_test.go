package main

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

func TestRenderSessionUsageHuman_WithCost(t *testing.T) {
	out := &sessionUsageOutput{
		SessionID: "claude:s1", Agent: "claude-code", Project: "proj",
		TotalOutputTokens: 28800, PeakContextTokens: 118000,
		HasTokenData: true, Cost: money.MustParseDollars("0.42"), HasCost: true,
		Models: []string{"claude-opus-4-6"},
	}
	var b strings.Builder
	require.NoError(t, renderSessionUsageHuman(&b, out))
	s := b.String()
	assert.Contains(t, s, "~$0.42", "output missing cost")
	assert.Contains(t, s, "claude-opus-4-6", "output missing model")
}

func TestRenderSessionUsageHuman_ReportedCostOmitsEstimateMarker(t *testing.T) {
	var out sessionUsageOutput
	require.NoError(t, json.Unmarshal([]byte(`{
		"session_id":"hermes:s1",
		"agent":"hermes",
		"cost":{"microdollars":30000},
		"has_cost":true,
		"cost_source":"reported",
		"models":["model-a"]
	}`), &out))

	var b strings.Builder
	require.NoError(t, renderSessionUsageHuman(&b, &out))
	assert.Contains(t, b.String(), "$0.03 (model-a)")
	assert.NotContains(t, b.String(), "~$0.03")
}

func TestRenderSessionUsageHuman_AuthoritativeCostWithoutModelsOmitsEstimateMarker(
	t *testing.T,
) {
	var out sessionUsageOutput
	require.NoError(t, json.Unmarshal([]byte(`{
		"session_id":"copilot:cost-only",
		"agent":"copilot",
		"cost":{"microdollars":30000},
		"has_cost":true,
		"cost_source":"reported",
		"models":[]
	}`), &out))

	var b strings.Builder
	require.NoError(t, renderSessionUsageHuman(&b, &out))
	assert.Contains(t, b.String(), "$0.03")
	assert.NotContains(t, b.String(), "~$0.03")
	assert.NotContains(t, b.String(), "()")
}

func TestRenderSessionUsageHuman_NoCostNoModels(t *testing.T) {
	out := &sessionUsageOutput{
		SessionID: "claude:s3", Agent: "claude-code",
		HasTokenData: true, HasCost: false,
	}
	var b strings.Builder
	require.NoError(t, renderSessionUsageHuman(&b, out))
	s := b.String()
	assert.Contains(t, s, "n/a", "expected bare 'n/a' cost line")
	assert.NotContains(t, s, "unpriced",
		"should not mention unpriced when none")
}

func TestRenderSessionUsageHuman_NoCost(t *testing.T) {
	out := &sessionUsageOutput{
		SessionID: "claude:s2", Agent: "claude-code",
		HasTokenData: true, HasCost: false,
		UnpricedModels: []string{"local-llama-99"},
	}
	var b strings.Builder
	require.NoError(t, renderSessionUsageHuman(&b, out))
	s := b.String()
	assert.NotContains(t, s, "$", "no-cost output should not contain '$'")
	assert.Contains(t, s, "local-llama-99",
		"output should note unpriced model")
}

func TestRenderSessionUsageHuman_CopilotWithAICredits(t *testing.T) {
	out := &sessionUsageOutput{
		SessionID:         "copilot:s1",
		Agent:             "copilot",
		Project:           "proj",
		TotalOutputTokens: 2000,
		PeakContextTokens: 5000,
		HasTokenData:      true,
		Cost:              money.MustParseDollars("10.00"),
		HasCost:           true,
		AICredits:         1000.0,
		Models:            []string{"gpt-4"},
	}
	var b strings.Builder
	require.NoError(t, renderSessionUsageHuman(&b, out))
	s := b.String()
	assert.Contains(t, s, "~$10.00", "output missing cost")
	assert.Contains(t, s, "1000", "output missing AI Credits")
	assert.Contains(t, s, "AI Credits", "output missing AI Credits label")
}

func TestRenderSessionUsageHuman_NonCopilotNoAICredits(t *testing.T) {
	out := &sessionUsageOutput{
		SessionID:         "claude:s1",
		Agent:             "claude-code",
		Project:           "proj",
		TotalOutputTokens: 1000,
		PeakContextTokens: 5000,
		HasTokenData:      true,
		Cost:              money.MustParseDollars("0.42"),
		HasCost:           true,
		Models:            []string{"claude-opus"},
	}
	var b strings.Builder
	require.NoError(t, renderSessionUsageHuman(&b, out))
	s := b.String()
	assert.Contains(t, s, "~$0.42", "output missing cost")
	assert.NotContains(t, s, "AI Credits",
		"non-Copilot sessions should not show AI Credits")
}

func TestRenderSessionUsageHuman_CopilotNoCost(t *testing.T) {
	out := &sessionUsageOutput{
		SessionID:      "copilot:s2",
		Agent:          "copilot",
		HasTokenData:   true,
		HasCost:        false,
		AICredits:      0,
		UnpricedModels: []string{"gpt-4"},
	}
	var b strings.Builder
	require.NoError(t, renderSessionUsageHuman(&b, out))
	s := b.String()
	assert.Contains(t, s, "n/a (unpriced: gpt-4)",
		"unpriced Copilot sessions should report n/a cost")
	assert.NotContains(t, s, "AI Credits",
		"unpriced Copilot sessions should not show AI Credits")
}

func TestSessionUsageJSONSchemaIncludesCostContract(t *testing.T) {
	cost := money.MustParseDollars("0.42")
	out := &sessionUsageOutput{
		SessionID:         "codex:abc",
		Agent:             "codex",
		Project:           "my-project",
		TotalOutputTokens: 123,
		PeakContextTokens: 456,
		HasTokenData:      true,
		Cost:              cost,
		HasCost:           true,
		// A caller normally gets CostUSD from the shared
		// db.CostUSDFromCost helper (called by every backend and
		// the subagent combiner); set here directly since this
		// test pins the JSON contract, not the computation.
		CostUSD:        db.CostUSDFromCost(true, cost),
		Models:         []string{"gpt-5.1"},
		UnpricedModels: []string{"local-model"},
		ServerRunning:  true,
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	assert.Equal(t, map[string]any{
		"session_id":          "codex:abc",
		"agent":               "codex",
		"project":             "my-project",
		"breakdown_count":     float64(0),
		"breakdown":           []any{},
		"total_output_tokens": float64(123),
		"peak_context_tokens": float64(456),
		"has_token_data":      true,
		"cost": map[string]any{
			"microdollars": float64(420000),
		},
		"has_cost":        true,
		"cost_usd":        0.42,
		"models":          []any{"gpt-5.1"},
		"unpriced_models": []any{"local-model"},
		"server_running":  true,
	}, raw, "a session with no subagents must omit subagent-only fields")
}

// TestSessionUsageJSONSchemaOmitsCostUSDWhenNoCost pins the other half of
// the cost_usd compatibility contract: roborev treats has_cost=true with a
// missing/zero cost_usd as the pre-fix bug case, so a session with no cost
// at all must omit the field entirely rather than emit cost_usd: 0.
func TestSessionUsageJSONSchemaOmitsCostUSDWhenNoCost(t *testing.T) {
	out := &sessionUsageOutput{
		SessionID: "claude:no-cost",
		Agent:     "claude-code",
		HasCost:   false,
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	assert.NotContains(t, raw, "cost_usd",
		"cost_usd must be omitted entirely when has_cost is false")
}

// TestSessionUsageJSONSchemaIncludesSubagentContract pins the two keys the
// subagent rollup adds. Both are omitempty, which is what keeps the
// no-subagent contract above byte-identical.
func TestSessionUsageJSONSchemaIncludesSubagentContract(t *testing.T) {
	out := &sessionUsageOutput{
		SessionID:      "parent-1",
		Agent:          "claude",
		Project:        "my-project",
		Models:         []string{"claude-opus-4-7"},
		BreakdownCount: 2,
		SubagentCount:  1,
		Breakdown: []db.SessionUsageBreakdownEntry{
			{Ordinal: 1, Source: "message", Label: "usage"},
			{
				Ordinal:           2,
				Source:            "message",
				Label:             "usage",
				SubagentSessionID: "agent-9f2c",
			},
		},
	}

	data, err := json.Marshal(out)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))

	assert.Equal(t, float64(1), raw["subagent_count"])
	rows, ok := raw["breakdown"].([]any)
	require.True(t, ok, "breakdown must serialize as an array")
	require.Len(t, rows, 2)

	own, ok := rows[0].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, own, "subagent_session_id",
		"the parent's own rows must not gain the field")

	child, ok := rows[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "agent-9f2c", child["subagent_session_id"])
	assert.Equal(t, "message", child["source"],
		"subagent rows keep their real source")
}

func TestRenderSessionUsageHuman_SubagentLine(t *testing.T) {
	tests := []struct {
		name          string
		subagentCount int
		wantLine      bool
	}{
		{name: "own session omits the line", subagentCount: 0},
		{name: "combined session names the count",
			subagentCount: 2, wantLine: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := &sessionUsageOutput{
				SessionID: "claude:s1", Agent: "claude-code",
				TotalOutputTokens: 100, HasTokenData: true,
				SubagentCount: tt.subagentCount,
			}
			var b strings.Builder
			require.NoError(t, renderSessionUsageHuman(&b, out))
			if tt.wantLine {
				assert.Contains(t, b.String(), "Subagents:     2 (included)")
				return
			}
			assert.NotContains(t, b.String(), "Subagents")
		})
	}
}
