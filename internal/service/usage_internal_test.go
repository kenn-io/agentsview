package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/parsertest"
)

func TestUsageSummaryResultEmitsEmptyProjectsMap(t *testing.T) {
	b, err := json.Marshal(UsageSummaryResult{
		SchemaVersion: export.UsageDailySchemaVersion,
		Projects:      map[string]export.ProjectMapEntry{},
	})
	require.NoError(t, err)

	assert.Contains(t, string(b), `"projects":{}`)
}

func TestComputeCacheStats_SavingsPassThrough(t *testing.T) {
	t.Parallel()
	// SavingsVsUncached is computed per-model in the DB layer;
	// computeCacheStats just forwards totals.CacheSavings. Verify the
	// pass-through at the positive, negative, and zero boundaries so a
	// future refactor that drops the field trips a test.
	cases := []struct {
		name string
		in   float64
	}{
		{"positive", 4.65},
		{"negative", -0.75},
		{"zero", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cs := computeCacheStats(db.UsageTotals{CacheSavings: tc.in})
			assert.InDelta(t, tc.in, cs.SavingsVsUncached, 1e-9)
		})
	}
}

func TestComputeCacheStats_ZeroTotalsIsZero(t *testing.T) {
	cs := computeCacheStats(db.UsageTotals{})
	assert.Zero(t, cs.SavingsVsUncached)
	assert.Zero(t, cs.HitRate)
}

func TestComputeCacheStats_HitRate(t *testing.T) {
	// 800 cache reads, 200 uncached inputs -> 0.80 hit rate. The
	// HitRate denominator is cacheRead + input where input is already
	// the uncached portion.
	cs := computeCacheStats(db.UsageTotals{
		InputTokens:     200,
		CacheReadTokens: 800,
	})
	assert.InDelta(t, 0.80, cs.HitRate, 1e-9)
}

func TestComputeCacheStats_UncachedPassesInputThrough(t *testing.T) {
	// Anthropic's input_tokens field is the NON-cached portion of the
	// input; cache_read and cache_creation are tracked separately.
	// UncachedInputTokens must equal InputTokens directly, not input
	// minus the cache buckets (which would double-subtract).
	cs := computeCacheStats(db.UsageTotals{
		InputTokens:         100,
		CacheReadTokens:     200,
		CacheCreationTokens: 50,
	})
	assert.Equal(t, 100, cs.UncachedInputTokens)
	assert.Equal(t, 200, cs.CacheReadTokens)
	assert.Equal(t, 50, cs.CacheCreationTokens)
}

// TestUnsupportedUsageKindForAgentFilter pins Copilot branding to Copilot
// identity: an agent that merely shares Copilot's capabilities (no token
// data, AI-credits denominated) must degrade to the generic kind, not be
// described as Copilot. No t.Parallel: it stubs the parser registry.
func TestUnsupportedUsageKindForAgentFilter(t *testing.T) {
	parsertest.StubAgentDefs(t, parser.AgentDef{
		Type:        parser.AgentType("credit-note-agent"),
		DisplayName: "Credit Note Agent",
		Usage: parser.UsageCapabilities{
			NoPerMessageTokenData: true,
			AICreditsDenominated:  true,
		},
	})

	cases := []struct {
		name   string
		filter string
		want   string
	}{
		{
			name:   "all-copilot filter",
			filter: "copilot,vscode-copilot",
			want:   UnsupportedUsageKindCopilotNoTokenData,
		},
		{
			name:   "non-copilot agent with copilot capabilities",
			filter: "credit-note-agent",
			want:   UnsupportedUsageKindNoTokenData,
		},
		{
			name:   "copilot mixed with non-copilot",
			filter: "copilot,credit-note-agent",
			want:   UnsupportedUsageKindNoTokenData,
		},
		{
			name:   "empty filter",
			filter: "",
			want:   UnsupportedUsageKindNoTokenData,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want,
				UnsupportedUsageKindForAgentFilter(tc.filter))
		})
	}
}
