package service

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
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
		in   money.Money
	}{
		{"positive", money.MustParseDollars("4.65")},
		{"negative", money.MustParseDollars("-0.75")},
		{"zero", money.Money{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cs := computeCacheStats(db.UsageTotals{CacheSavings: tc.in})
			assert.Equal(t, tc.in, cs.SavingsVsUncached)
		})
	}
}

func TestComputeCacheStats_ZeroTotalsIsZero(t *testing.T) {
	cs := computeCacheStats(db.UsageTotals{})
	assert.Equal(t, money.Money{}, cs.SavingsVsUncached)
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

func TestBuildUsagePairwiseFilters_Validation(t *testing.T) {
	t.Parallel()

	_, _, _, _, err := buildUsagePairwiseFilters(
		UsagePairwiseComparisonRequest{
			From: "2024-06-01", To: "2024-06-02",
			LeftDimension:  "model",
			LeftValue:      "claude-sonnet-4-20250514",
			RightDimension: "machine",
			RightValue:     "host-a",
		},
		db.UsageFilter{},
	)
	require.Error(t, err)
	var ue *UsageInputError
	require.ErrorAs(t, err, &ue)
	assert.Equal(t, "right_dimension must be model or project", ue.Msg)
}

func TestBuildUsagePairwiseFilters_PreservesNonComparedBaseFilters(t *testing.T) {
	t.Parallel()

	left, leftEmpty, right, rightEmpty, err := buildUsagePairwiseFilters(
		UsagePairwiseComparisonRequest{
			From:           "2024-06-01",
			To:             "2024-06-03",
			Timezone:       "UTC",
			Model:          "model-a,model-b",
			Project:        "alpha,beta",
			LeftDimension:  "model",
			LeftValue:      "model-b",
			RightDimension: "project",
			RightValue:     "beta",
		},
		db.UsageFilter{Model: "model-a,model-b", Project: "alpha,beta"},
	)
	require.NoError(t, err)
	assert.False(t, leftEmpty)
	assert.False(t, rightEmpty)
	assert.Equal(t, "model-b", left.Model)
	assert.Equal(t, "alpha,beta", left.Project)
	assert.Equal(t, "model-a,model-b", right.Model)
	assert.Equal(t, "beta", right.Project)
}

func TestBuildUsagePairwiseFilters_ConflictingBaseFilterMarksEmptySide(t *testing.T) {
	t.Parallel()

	left, leftEmpty, right, rightEmpty, err := buildUsagePairwiseFilters(
		UsagePairwiseComparisonRequest{
			From:           "2024-06-01",
			To:             "2024-06-03",
			Model:          "gpt-4o",
			LeftDimension:  "model",
			LeftValue:      "claude-sonnet-4-20250514",
			RightDimension: "model",
			RightValue:     "gpt-4o",
		},
		db.UsageFilter{Model: "gpt-4o"},
	)
	require.NoError(t, err)
	assert.True(t, leftEmpty)
	assert.False(t, rightEmpty)
	assert.Empty(t, left.Model)
	assert.Equal(t, "gpt-4o", right.Model)
}

func TestDefaultUsagePairwiseSelection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		models   []ModelTotal
		projects []ProjectTotal
		want     [4]string
	}{
		{"models", []ModelTotal{{Model: "second-by-name"}, {Model: "first-by-name"}}, []ProjectTotal{{ProjectKey: "key-b"}, {ProjectKey: "key-a"}}, [4]string{"model", "second-by-name", "model", "first-by-name"}},
		{"projects", []ModelTotal{{Model: "only-model"}}, []ProjectTotal{{ProjectKey: "key-b", Project: "label-b"}, {ProjectKey: "key-a", Project: "label-a"}}, [4]string{"project", "key-b", "project", "key-a"}},
		{"mixed", []ModelTotal{{Model: "only-model"}}, []ProjectTotal{{ProjectKey: "key-a", Project: "label-a"}}, [4]string{"model", "only-model", "project", "key-a"}},
		{"skip empty models", []ModelTotal{{}, {Model: "model-b"}, {}, {Model: "model-a"}}, nil, [4]string{"model", "model-b", "model", "model-a"}},
		{"skip empty projects", []ModelTotal{{Model: "only-model"}}, []ProjectTotal{{}, {ProjectKey: "key-b"}, {}, {ProjectKey: "key-a"}}, [4]string{"project", "key-b", "project", "key-a"}},
		{"mixed after empty keys", []ModelTotal{{}, {Model: "only-model"}}, []ProjectTotal{{}, {ProjectKey: "key-a"}}, [4]string{"model", "only-model", "project", "key-a"}},
		{"one usable model", []ModelTotal{{}, {Model: "only-model"}}, []ProjectTotal{{}}, [4]string{}},
		{"one usable project", []ModelTotal{{}}, []ProjectTotal{{}, {ProjectKey: "key-a"}}, [4]string{}},
		{"empty keys", []ModelTotal{{}, {}}, []ProjectTotal{{}, {}}, [4]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ld, lv, rd, rv := defaultUsagePairwiseSelection(&UsageSummaryResult{ModelTotals: tc.models, ProjectTotals: tc.projects})
			assert.Equal(t, tc.want, [4]string{ld, lv, rd, rv})
		})
	}
}
