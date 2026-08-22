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

type rollupStore struct {
	db.Store
	usages   map[string]*db.SessionUsage
	children map[string][]db.Session
	usageErr map[string]error
	childErr map[string]error
	rows     []activity.UsageRow
	rowsErr  error
}

func (s *rollupStore) GetSessionUsageRows(
	ctx context.Context, ids []string,
) (*activity.SessionUsageRows, error) {
	if s.rows == nil {
		return nil, nil
	}
	included := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		included[id] = struct{}{}
	}
	rows := make([]activity.UsageRow, 0, len(s.rows))
	coverageRows := make([]activity.UsageRow, 0, len(s.rows))
	rawOutputTokensBySession := make(map[string]int)
	for _, row := range s.rows {
		sourceSessionID := row.SourceSessionID
		if sourceSessionID == "" {
			sourceSessionID = row.SessionID
		}
		if _, ok := included[sourceSessionID]; ok {
			rows = append(rows, row)
			row.SessionID = sourceSessionID
			coverageRows = append(coverageRows, row)
			rawOutputTokensBySession[sourceSessionID] += row.OutputTokens
		}
	}
	coverage, err := activity.CanonicalSessionTokenCoverageContext(ctx, coverageRows)
	if err != nil {
		return nil, err
	}
	return &activity.SessionUsageRows{
		Rows:                            rows,
		RawOutputTokensBySession:        rawOutputTokensBySession,
		CanonicalTokenCoverageBySession: coverage,
	}, s.rowsErr
}

func (s *rollupStore) GetSessionUsage(
	_ context.Context, id string, _ bool,
) (*db.SessionUsage, error) {
	if err := s.usageErr[id]; err != nil {
		return nil, err
	}
	return s.usages[id], nil
}

func (s *rollupStore) GetSession(
	_ context.Context, id string,
) (*db.Session, error) {
	if usage := s.usages[id]; usage != nil {
		return &db.Session{
			ID:                id,
			TotalOutputTokens: usage.TotalOutputTokens,
		}, nil
	}
	for _, children := range s.children {
		for i := range children {
			if children[i].ID == id {
				child := children[i]
				return &child, nil
			}
		}
	}
	return nil, nil
}

func (s *rollupStore) GetChildSessions(
	_ context.Context, id string,
) ([]db.Session, error) {
	if err := s.childErr[id]; err != nil {
		return nil, err
	}
	return s.children[id], nil
}

func TestGetSessionUsageRollupIncludesOnlyPricedSubagentsOnce(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {SessionID: "root", HasCost: true, Cost: money.MustParseDollars("1"), BreakdownCount: 1},
			"a":    {SessionID: "a", HasCost: true, Cost: money.MustParseDollars("2"), BreakdownCount: 1},
			"b":    {SessionID: "b", HasCost: true, Cost: money.MustParseDollars("4"), BreakdownCount: 1},
			"u":    {SessionID: "u", HasCost: false, BreakdownCount: 1},
		},
		children: map[string][]db.Session{
			"root": {
				{ID: "a", RelationshipType: "subagent"},
				{ID: "fork", RelationshipType: "fork"},
				{ID: "continuation", RelationshipType: "continuation"},
				{ID: "a", RelationshipType: "subagent"},
			},
			"a": {{ID: "b", RelationshipType: "subagent"}, {ID: "root", RelationshipType: "subagent"}},
			"b": {{ID: "u", RelationshipType: "subagent"}},
		},
	}

	got, err := service.GetSessionUsageRollup(context.Background(), store, "root", false)
	require.NoError(t, err)
	require.Equal(t, 3, got.SubagentCount)
	require.Zero(t, got.Cost)
	require.False(t, got.HasCost, "unpriced contributing row must make the aggregate incomplete")
}

func TestGetSessionUsageRollupIncludesNestedPricedSubagents(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {SessionID: "root", HasCost: true, Cost: money.MustParseDollars("1"), BreakdownCount: 1},
			"a":    {SessionID: "a", HasCost: true, Cost: money.MustParseDollars("2"), BreakdownCount: 1},
			"b":    {SessionID: "b", HasCost: true, Cost: money.MustParseDollars("4"), BreakdownCount: 1},
		},
		children: map[string][]db.Session{
			"root": {{ID: "a", RelationshipType: "subagent"}},
			"a":    {{ID: "b", RelationshipType: "subagent"}},
		},
	}

	got, err := service.GetSessionUsageRollup(context.Background(), store, "root", false)
	require.NoError(t, err)
	require.Equal(t, 2, got.SubagentCount)
	require.Equal(t, money.MustParseDollars("7"), got.Cost)
	require.True(t, got.HasCost)
}

func TestSessionUsageWithSubagentsFallbackMarksRowlessTokensIncomplete(
	t *testing.T,
) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {
				SessionID: "root", HasTokenData: true,
				TotalOutputTokens: 500, HasCost: true,
				Cost: money.MustParseDollars("1"), BreakdownCount: 1,
			},
			"child": {
				SessionID: "child", HasTokenData: true,
				TotalOutputTokens: 200,
			},
		},
		children: map[string][]db.Session{
			"root": {{
				ID: "child", RelationshipType: "subagent",
				TotalOutputTokens: 200, HasTotalOutputTokens: true,
			}},
		},
	}

	got, err := service.SessionUsageWithSubagents(
		context.Background(), store, "root", false)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 700, got.TotalOutputTokens)
	assert.False(t, got.HasCost,
		"rowless child tokens make the fallback aggregate incomplete")
}

func TestGetSessionUsageRollupCountsEmptySubagentAndTerminatesCycle(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {SessionID: "root"},
		},
		children: map[string][]db.Session{
			"root":  {{ID: "empty", RelationshipType: "subagent"}},
			"empty": {{ID: "root", RelationshipType: "subagent"}},
		},
	}

	got, err := service.GetSessionUsageRollup(context.Background(), store, "root", false)
	require.NoError(t, err)
	require.Equal(t, 1, got.SubagentCount)
	require.False(t, got.HasCost)
}

func TestGetSessionUsageRollupRequiresContributingSubagentForHasCost(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root":  {SessionID: "root", HasCost: true, Cost: money.MustParseDollars("1"), BreakdownCount: 1},
			"empty": {SessionID: "empty"},
		},
		children: map[string][]db.Session{
			"root": {{ID: "empty", RelationshipType: "subagent"}},
		},
	}

	got, err := service.GetSessionUsageRollup(context.Background(), store, "root", false)
	require.NoError(t, err)
	require.Equal(t, 1, got.SubagentCount)
	require.Zero(t, got.Cost)
	require.False(t, got.HasCost, "root-only priced usage must not be labeled as a total")
}

func TestGetSessionUsageRollupReturnsChildSessionError(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {SessionID: "root", HasCost: true, Cost: money.MustParseDollars("1"), BreakdownCount: 1},
		},
		childErr: map[string]error{
			"root": errors.New("child lookup failed"),
		},
	}

	got, err := service.GetSessionUsageRollup(context.Background(), store, "root", false)
	require.Nil(t, got)
	require.EqualError(t, err, "child lookup failed")
}

func TestGetSessionUsageRollupReturnsChildUsageError(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {SessionID: "root", HasCost: true, Cost: money.MustParseDollars("1"), BreakdownCount: 1},
		},
		children: map[string][]db.Session{
			"root": {{ID: "child", RelationshipType: "subagent"}},
		},
		usageErr: map[string]error{
			"child": errors.New("child usage failed"),
		},
	}

	got, err := service.GetSessionUsageRollup(context.Background(), store, "root", false)
	require.Nil(t, got)
	require.EqualError(t, err, "child usage failed")
}

func TestGetSessionUsageRollupTraversesNonSubagentAndDedupesRowsAcrossSessions(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root":   {SessionID: "root", HasCost: true, Cost: money.MustParseDollars("1"), BreakdownCount: 1},
			"nested": {SessionID: "nested", HasCost: true, Cost: money.MustParseDollars("2"), BreakdownCount: 2},
		},
		children: map[string][]db.Session{
			"root":         {{ID: "continuation", RelationshipType: "continuation"}},
			"continuation": {{ID: "nested", RelationshipType: "subagent"}},
		},
		rows: []activity.UsageRow{
			{SessionID: "root", Cost: money.MustParseDollars("1"), Priced: true, Contributes: true, ClaudeMessageID: "shared", ClaudeRequestID: "request"},
			{SessionID: "nested", Cost: money.MustParseDollars("2"), Priced: true, Contributes: true, ClaudeMessageID: "unique", ClaudeRequestID: "request"},
		},
	}

	got, err := service.GetSessionUsageRollup(context.Background(), store, "root", false)
	require.NoError(t, err)
	require.Equal(t, 1, got.SubagentCount)
	require.True(t, got.HasCost)
	require.Equal(t, money.MustParseDollars("3"), got.Cost)
}

func TestGetSessionUsageRollupIncludesForkInsideSubagentTree(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {SessionID: "root"},
		},
		children: map[string][]db.Session{
			"root": {
				{ID: "agent", RelationshipType: "subagent"},
				{ID: "root-fork", RelationshipType: "fork"},
			},
			"agent": {
				{ID: "agent-fork", RelationshipType: "fork"},
			},
		},
		rows: []activity.UsageRow{
			{SessionID: "root", Cost: money.MustParseDollars("1"), Priced: true, Contributes: true},
			{SessionID: "agent", Cost: money.MustParseDollars("2"), Priced: true, Contributes: true},
			{SessionID: "agent-fork", Cost: money.MustParseDollars("4"), Priced: true, Contributes: true},
			{SessionID: "root-fork", Cost: money.MustParseDollars("8"), Priced: true, Contributes: true},
		},
	}

	got, err := service.GetSessionUsageRollup(
		context.Background(), store, "root", false)
	require.NoError(t, err)
	require.Equal(t, 1, got.SubagentCount)
	require.True(t, got.HasCost)
	require.Equal(t, money.MustParseDollars("7"), got.Cost,
		"the delegated fork is included without including the root fork")
}

func TestGetSessionUsageRollupCombinesProvenanceAcrossSessions(t *testing.T) {
	rootSessionCost := money.MustParseDollars("1")
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {
				SessionID: "root", HasCost: true, Cost: rootSessionCost,
				CostSource: export.CostSourceReported, BreakdownCount: 1,
			},
			"child": {
				SessionID: "child", HasCost: true, Cost: money.MustParseDollars("2"),
				CostSource: export.CostSourceComputed, BreakdownCount: 1,
			},
		},
		children: map[string][]db.Session{
			"root": {{ID: "child", RelationshipType: "subagent"}},
		},
		rows: []activity.UsageRow{
			{
				SessionID: "root", Cost: money.MustParseDollars("10"),
				SessionCost: &rootSessionCost,
				CostSource:  export.CostSourceComputed,
				Priced:      true, Contributes: true,
			},
			{
				SessionID: "child", Cost: money.MustParseDollars("2"),
				CostSource: export.CostSourceComputed,
				Priced:     true, Contributes: true,
			},
		},
	}

	got, err := service.GetSessionUsageRollup(context.Background(), store, "root", false)
	require.NoError(t, err)
	require.True(t, got.HasCost)
	require.Equal(t, money.MustParseDollars("3"), got.Cost)
	require.Equal(t, export.CostSourceMixed, got.CostSource)
}

func TestGetSessionUsageRollupDoesNotLabelDedupedRootCostAsTotal(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root":   {SessionID: "root", HasCost: true, Cost: money.MustParseDollars("1"), BreakdownCount: 1},
			"nested": {SessionID: "nested", HasCost: true, Cost: money.MustParseDollars("1"), BreakdownCount: 1},
		},
		children: map[string][]db.Session{
			"root": {{ID: "nested", RelationshipType: "subagent"}},
		},
		rows: []activity.UsageRow{
			{SessionID: "root", Cost: money.MustParseDollars("1"), Priced: true, Contributes: true, ClaudeMessageID: "shared", ClaudeRequestID: "request"},
		},
	}

	got, err := service.GetSessionUsageRollup(context.Background(), store, "root", false)
	require.NoError(t, err)
	require.Equal(t, 1, got.SubagentCount)
	require.False(t, got.HasCost)
	require.Zero(t, got.Cost)
}
