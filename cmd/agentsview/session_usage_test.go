package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestRenderSessionUsageHuman_WithCost(t *testing.T) {
	out := &sessionUsageOutput{
		SessionUsage: db.SessionUsage{
			SessionID: "claude:s1", Agent: "claude-code", Project: "proj",
			TotalOutputTokens: 28800, PeakContextTokens: 118000,
			HasTokenData: true, CostUSD: 0.42, HasCost: true,
			Models: []string{"claude-opus-4-6"},
		},
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
		"cost_usd":0.03,
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
		"cost_usd":0.03,
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
		SessionUsage: db.SessionUsage{
			SessionID: "claude:s3", Agent: "claude-code",
			HasTokenData: true, HasCost: false,
		},
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
		SessionUsage: db.SessionUsage{
			SessionID: "claude:s2", Agent: "claude-code",
			HasTokenData: true, HasCost: false,
			UnpricedModels: []string{"local-llama-99"},
		},
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
		SessionUsage: db.SessionUsage{
			SessionID:         "copilot:s1",
			Agent:             "copilot",
			Project:           "proj",
			TotalOutputTokens: 2000,
			PeakContextTokens: 5000,
			HasTokenData:      true,
			CostUSD:           10.00,
			HasCost:           true,
			AICredits:         1000.0,
			Models:            []string{"gpt-4"},
		},
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
		SessionUsage: db.SessionUsage{
			SessionID:         "claude:s1",
			Agent:             "claude-code",
			Project:           "proj",
			TotalOutputTokens: 1000,
			PeakContextTokens: 5000,
			HasTokenData:      true,
			CostUSD:           0.42,
			HasCost:           true,
			Models:            []string{"claude-opus"},
		},
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
		SessionUsage: db.SessionUsage{
			SessionID:      "copilot:s2",
			Agent:          "copilot",
			HasTokenData:   true,
			HasCost:        false,
			AICredits:      0,
			UnpricedModels: []string{"gpt-4"},
		},
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
	out := &sessionUsageOutput{
		SessionUsage: db.SessionUsage{
			SessionID:         "codex:abc",
			Agent:             "codex",
			Project:           "my-project",
			TotalOutputTokens: 123,
			PeakContextTokens: 456,
			HasTokenData:      true,
			CostUSD:           0.42,
			HasCost:           true,
			Models:            []string{"gpt-5.1"},
			UnpricedModels:    []string{"local-model"},
		},
		ServerRunning: true,
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
		"breakdown":           nil,
		"total_output_tokens": float64(123),
		"peak_context_tokens": float64(456),
		"has_token_data":      true,
		"cost_usd":            0.42,
		"has_cost":            true,
		"models":              []any{"gpt-5.1"},
		"unpriced_models":     []any{"local-model"},
		"server_running":      true,
	}, raw)
}
