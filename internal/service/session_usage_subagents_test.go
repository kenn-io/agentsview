package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
)

// usageRow builds one row of the kind a store's GetSessionUsageRows returns:
// already deduplicated across the whole id set and ordered by timestamp.
func usageRow(
	sessionID, model, ts string, ordinal int, cost string,
) activity.UsageRow {
	return activity.UsageRow{
		SessionID:      sessionID,
		Model:          model,
		Timestamp:      ts,
		Cost:           money.MustParseDollars(cost),
		CostSource:     export.CostSourceComputed,
		Priced:         true,
		Contributes:    true,
		UsageSource:    "message",
		MessageOrdinal: int64(ordinal),
		OutputTokens:   10,
		InputTokens:    20,
	}
}

func TestSessionUsageWithSubagentsCombinesCostAcrossDescendants(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {
				SessionID: "root", Agent: "claude", Project: "app",
				TotalOutputTokens: 10, PeakContextTokens: 900,
				HasTokenData: true, HasCost: true,
				Cost:           money.MustParseDollars("1"),
				Models:         []string{"opus"},
				BreakdownCount: 1,
			},
		},
		children: map[string][]db.Session{
			"root": {
				{
					ID: "agent-a", RelationshipType: "subagent",
					TotalOutputTokens: 10, HasTotalOutputTokens: true,
					PeakContextTokens: 2000, HasPeakContextTokens: true,
				},
				{
					ID: "agent-b", RelationshipType: "subagent",
					TotalOutputTokens: 10, HasTotalOutputTokens: true,
					PeakContextTokens: 100, HasPeakContextTokens: true,
				},
			},
		},
		rows: []activity.UsageRow{
			usageRow("root", "opus", "2026-07-30T10:00:00Z", 0, "1"),
			usageRow("agent-a", "sonnet", "2026-07-30T10:01:00Z", 0, "2"),
			usageRow("agent-b", "opus", "2026-07-30T10:02:00Z", 0, "4"),
		},
	}

	got, err := service.SessionUsageWithSubagents(
		context.Background(), store, "root", true)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, "root", got.SessionID, "identity stays the parent's")
	assert.Equal(t, "claude", got.Agent)
	assert.Equal(t, "app", got.Project)
	assert.Equal(t, 2, got.SubagentCount)
	assert.True(t, got.HasCost)
	assert.Equal(t, money.MustParseDollars("7"), got.Cost)
	// cost_usd must reflect the combined (rollup-inclusive) cost, never
	// just the root's own cost.
	require.NotNil(t, got.CostUSD)
	assert.InDelta(t, 7.0, *got.CostUSD, 1e-9)
	assert.Equal(t, export.CostSourceComputed, got.CostSource)
	assert.Equal(t, []string{"opus", "sonnet"}, got.Models)
	assert.Equal(t, 3, got.BreakdownCount)
	assert.Equal(t, 30, got.TotalOutputTokens,
		"the three complete session totals are combined")
	assert.Equal(t, 2000, got.PeakContextTokens,
		"peak context is a high-water mark, so it is maxed not summed")

	require.Len(t, got.Breakdown, 3)
	assert.Equal(t, []int{1, 2, 3}, []int{
		got.Breakdown[0].Ordinal,
		got.Breakdown[1].Ordinal,
		got.Breakdown[2].Ordinal,
	}, "ordinals renumber over the combined stream")
	assert.Empty(t, got.Breakdown[0].SubagentSessionID,
		"the parent's own rows carry no subagent id")
	assert.Equal(t, "agent-a", got.Breakdown[1].SubagentSessionID)
	assert.Equal(t, "agent-b", got.Breakdown[2].SubagentSessionID)
	assert.Equal(t, "message", got.Breakdown[1].Source,
		"subagent rows keep their real source")
	assert.Equal(t, "Prompt 1", got.Breakdown[1].Label)
	assert.Equal(t, money.MustParseDollars("2"), got.Breakdown[1].Cost)
	assert.Equal(t, 20, got.Breakdown[1].InputTokens)
	assert.Equal(t, 10, got.Breakdown[1].OutputTokens)
}

// TestSessionUsageWithSubagentsCountsRowlessSessionsFromAggregates covers the
// other half of the output-token rule: a session that produced no usage rows
// has no echo to deduplicate and nothing else to report, so its stored
// aggregate still counts.
func TestSessionUsageWithSubagentsCountsRowlessSessionsFromAggregates(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {SessionID: "root", TotalOutputTokens: 7},
		},
		children: map[string][]db.Session{
			"root": {
				{
					ID: "agent-rows", RelationshipType: "subagent",
					TotalOutputTokens: 10, HasTotalOutputTokens: true,
				},
				{
					ID: "agent-rowless", RelationshipType: "subagent",
					TotalOutputTokens: 40, HasTotalOutputTokens: true,
				},
			},
		},
		rows: []activity.UsageRow{
			usageRow("agent-rows", "opus", "2026-07-30T10:00:00Z", 0, "1"),
		},
	}

	got, err := service.SessionUsageWithSubagents(
		context.Background(), store, "root", false)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 57, got.TotalOutputTokens,
		"the rowless root (7) and subagent (40) keep their aggregates")
	assert.True(t, got.HasTokenData)
}

func TestSessionUsageWithSubagentsReturnsOwnUsageWithoutDescendants(t *testing.T) {
	own := &db.SessionUsage{
		SessionID: "root", HasCost: true,
		Cost: money.MustParseDollars("1"), BreakdownCount: 1,
	}
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{"root": own},
		rows: []activity.UsageRow{
			usageRow("root", "opus", "2026-07-30T10:00:00Z", 0, "99"),
		},
	}

	got, err := service.SessionUsageWithSubagents(
		context.Background(), store, "root", true)
	require.NoError(t, err)
	assert.Same(t, own, got,
		"without subagents the store's own-session result passes through")
	assert.Zero(t, got.SubagentCount)
}

func TestSessionUsageWithSubagentsIsMissingWhenSessionIsMissing(t *testing.T) {
	store := &rollupStore{}
	got, err := service.SessionUsageWithSubagents(
		context.Background(), store, "absent", true)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestSessionUsageWithSubagentsTerminatesOnCycles(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {SessionID: "root"},
		},
		children: map[string][]db.Session{
			"root":    {{ID: "agent-a", RelationshipType: "subagent"}},
			"agent-a": {{ID: "root", RelationshipType: "subagent"}},
		},
		rows: []activity.UsageRow{
			usageRow("agent-a", "opus", "2026-07-30T10:00:00Z", 0, "2"),
		},
	}

	got, err := service.SessionUsageWithSubagents(
		context.Background(), store, "root", false)
	require.NoError(t, err)
	assert.Equal(t, 1, got.SubagentCount,
		"the root must not be re-counted as its own descendant")
	assert.Equal(t, money.MustParseDollars("2"), got.Cost)
}

func TestSessionUsageWithSubagentsDescendsThroughNonSubagentLinks(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{"root": {SessionID: "root"}},
		children: map[string][]db.Session{
			"root": {{ID: "fork", RelationshipType: "fork"}},
			"fork": {{ID: "agent-a", RelationshipType: "subagent"}},
			"agent-a": {{
				ID: "agent-fork", RelationshipType: "fork",
				TotalOutputTokens: 5, HasTotalOutputTokens: true,
			}},
		},
		rows: []activity.UsageRow{
			usageRow("agent-a", "opus", "2026-07-30T10:00:00Z", 0, "3"),
			usageRow("agent-fork", "opus", "2026-07-30T10:01:00Z", 0, "5"),
		},
	}

	got, err := service.SessionUsageWithSubagents(
		context.Background(), store, "root", false)
	require.NoError(t, err)
	assert.Equal(t, 1, got.SubagentCount,
		"forks are never counted as additional subagents")
	assert.Equal(t, money.MustParseDollars("8"), got.Cost,
		"the root fork is traversed, while the fork inside the subagent is priced")
}

func TestSessionUsageWithSubagentsReportsTokenDataFromSubagentsOnly(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {SessionID: "root", HasTokenData: false},
		},
		children: map[string][]db.Session{
			"root": {{
				ID: "agent-a", RelationshipType: "subagent",
				TotalOutputTokens: 10, HasTotalOutputTokens: true,
			}},
		},
		rows: []activity.UsageRow{
			usageRow("agent-a", "opus", "2026-07-30T10:00:00Z", 0, "2"),
		},
	}

	got, err := service.SessionUsageWithSubagents(
		context.Background(), store, "root", false)
	require.NoError(t, err)
	assert.True(t, got.HasTokenData,
		"a parent whose only token data lives in subagents has token data")
	assert.Equal(t, 10, got.TotalOutputTokens,
		"the subagent's complete total is included")
}

func TestSessionUsageWithSubagentsWithholdsIncompleteCost(t *testing.T) {
	unpriced := usageRow("agent-a", "mystery", "2026-07-30T10:01:00Z", 0, "0")
	unpriced.Priced = false
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {
				SessionID: "root", HasCost: true,
				Cost: money.MustParseDollars("1"), BreakdownCount: 1,
			},
		},
		children: map[string][]db.Session{
			"root": {{ID: "agent-a", RelationshipType: "subagent"}},
		},
		rows: []activity.UsageRow{
			usageRow("root", "opus", "2026-07-30T10:00:00Z", 0, "1"),
			unpriced,
		},
	}

	got, err := service.SessionUsageWithSubagents(
		context.Background(), store, "root", false)
	require.NoError(t, err)
	assert.False(t, got.HasCost,
		"one unpriced subagent row must not yield a partial total")
	assert.Zero(t, got.Cost)
	assert.Nil(t, got.CostUSD,
		"cost_usd must be omitted when has_cost is false")
	assert.Equal(t, []string{"mystery"}, got.UnpricedModels)
}

func TestSessionUsageWithSubagentsCombinesCostProvenance(t *testing.T) {
	reported := usageRow("agent-a", "opus", "2026-07-30T10:01:00Z", 0, "2")
	reported.CostSource = export.CostSourceReported
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{"root": {SessionID: "root"}},
		children: map[string][]db.Session{
			"root": {{ID: "agent-a", RelationshipType: "subagent"}},
		},
		rows: []activity.UsageRow{
			usageRow("root", "opus", "2026-07-30T10:00:00Z", 0, "1"),
			reported,
		},
	}

	got, err := service.SessionUsageWithSubagents(
		context.Background(), store, "root", false)
	require.NoError(t, err)
	assert.True(t, got.HasCost)
	assert.Equal(t, export.CostSourceMixed, got.CostSource)
}

func TestSessionUsageWithSubagentsKeepsCostOnlyCarrierOutOfUsage(t *testing.T) {
	reportedTotal := money.MustParseDollars("0.03")
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {
				SessionID: "root", Agent: "copilot", HasCost: true,
				Cost: reportedTotal, CostSource: export.CostSourceReported,
			},
		},
		children: map[string][]db.Session{
			"root": {{
				ID: "agent-a", RelationshipType: "subagent",
				TotalOutputTokens: 10, HasTotalOutputTokens: true,
			}},
		},
		rows: []activity.UsageRow{
			{
				SessionID: "root", Model: "copilot", UsageSource: "session",
				SessionCost: &reportedTotal, Priced: true,
			},
			usageRow("agent-a", "sonnet", "2026-07-30T10:01:00Z", 0, "0.02"),
		},
	}

	got, err := service.SessionUsageWithSubagents(
		context.Background(), store, "root", true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.HasCost)
	assert.Equal(t, money.MustParseDollars("0.05"), got.Cost,
		"the reported carrier still settles the root cost")
	assert.Equal(t, export.CostSourceMixed, got.CostSource)
	assert.Equal(t, []string{"sonnet"}, got.Models,
		"a cost-only carrier must not surface a synthetic model")
	assert.Equal(t, 1, got.BreakdownCount)
	require.Len(t, got.Breakdown, 1)
	assert.Equal(t, "sonnet", got.Breakdown[0].Model)
}

func TestSessionUsageWithSubagentsPropagatesChildLookupError(t *testing.T) {
	store := &rollupStore{
		usages:   map[string]*db.SessionUsage{"root": {SessionID: "root"}},
		childErr: map[string]error{"root": errors.New("child lookup failed")},
	}

	got, err := service.SessionUsageWithSubagents(
		context.Background(), store, "root", false)
	assert.Nil(t, got)
	require.EqualError(t, err, "child lookup failed")
}

// TestSessionUsageWithSubagentsFallsBackToPerSessionTotals covers stores that
// expose no usage-row provider: totals still combine, they just cannot be
// deduplicated across transcripts.
func TestSessionUsageWithSubagentsFallsBackToPerSessionTotals(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {
				SessionID: "root", Agent: "claude",
				HasCost: true, Cost: money.MustParseDollars("1"),
				Models: []string{"opus"}, BreakdownCount: 1,
				Breakdown: []db.SessionUsageBreakdownEntry{
					{Ordinal: 1, Source: "message", Model: "opus"},
				},
			},
			"agent-a": {
				SessionID: "agent-a",
				HasCost:   true, Cost: money.MustParseDollars("2"),
				Models: []string{"sonnet"}, BreakdownCount: 1,
				Breakdown: []db.SessionUsageBreakdownEntry{
					{Ordinal: 1, Source: "message", Model: "sonnet"},
				},
			},
		},
		children: map[string][]db.Session{
			"root": {{ID: "agent-a", RelationshipType: "subagent"}},
		},
		// rows nil: rollupStore reports no usage-row provider data.
	}

	got, err := service.SessionUsageWithSubagents(
		context.Background(), store, "root", true)
	require.NoError(t, err)
	assert.Equal(t, 1, got.SubagentCount)
	assert.True(t, got.HasCost)
	assert.Equal(t, money.MustParseDollars("3"), got.Cost)
	require.NotNil(t, got.CostUSD)
	assert.InDelta(t, 3.0, *got.CostUSD, 1e-9)
	assert.Equal(t, []string{"opus", "sonnet"}, got.Models)
	assert.Equal(t, 2, got.BreakdownCount)
	require.Len(t, got.Breakdown, 2)
	assert.Empty(t, got.Breakdown[0].SubagentSessionID)
	assert.Equal(t, "agent-a", got.Breakdown[1].SubagentSessionID)
	assert.Equal(t, 2, got.Breakdown[1].Ordinal)
}
