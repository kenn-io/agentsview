package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

type rollupStore struct {
	db.Store
	usages   map[string]*db.SessionUsage
	children map[string][]db.Session
	usageErr map[string]error
	childErr map[string]error
}

func (s *rollupStore) GetSessionUsage(
	_ context.Context, id string, _ bool,
) (*db.SessionUsage, error) {
	if err := s.usageErr[id]; err != nil {
		return nil, err
	}
	return s.usages[id], nil
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
			"root": {SessionID: "root", HasCost: true, CostUSD: 1, BreakdownCount: 1},
			"a":    {SessionID: "a", HasCost: true, CostUSD: 2, BreakdownCount: 1},
			"b":    {SessionID: "b", HasCost: true, CostUSD: 4, BreakdownCount: 1},
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
	require.Zero(t, got.CostUSD)
	require.False(t, got.HasCost, "unpriced contributing row must make the aggregate incomplete")
}

func TestGetSessionUsageRollupIncludesNestedPricedSubagents(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {SessionID: "root", HasCost: true, CostUSD: 1, BreakdownCount: 1},
			"a":    {SessionID: "a", HasCost: true, CostUSD: 2, BreakdownCount: 1},
			"b":    {SessionID: "b", HasCost: true, CostUSD: 4, BreakdownCount: 1},
		},
		children: map[string][]db.Session{
			"root": {{ID: "a", RelationshipType: "subagent"}},
			"a":    {{ID: "b", RelationshipType: "subagent"}},
		},
	}

	got, err := service.GetSessionUsageRollup(context.Background(), store, "root", false)
	require.NoError(t, err)
	require.Equal(t, 2, got.SubagentCount)
	require.Equal(t, 7.0, got.CostUSD)
	require.True(t, got.HasCost)
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
			"root":  {SessionID: "root", HasCost: true, CostUSD: 1, BreakdownCount: 1},
			"empty": {SessionID: "empty"},
		},
		children: map[string][]db.Session{
			"root": {{ID: "empty", RelationshipType: "subagent"}},
		},
	}

	got, err := service.GetSessionUsageRollup(context.Background(), store, "root", false)
	require.NoError(t, err)
	require.Equal(t, 1, got.SubagentCount)
	require.Zero(t, got.CostUSD)
	require.False(t, got.HasCost, "root-only priced usage must not be labeled as a total")
}

func TestGetSessionUsageRollupReturnsChildSessionError(t *testing.T) {
	store := &rollupStore{
		usages: map[string]*db.SessionUsage{
			"root": {SessionID: "root", HasCost: true, CostUSD: 1, BreakdownCount: 1},
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
			"root": {SessionID: "root", HasCost: true, CostUSD: 1, BreakdownCount: 1},
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
