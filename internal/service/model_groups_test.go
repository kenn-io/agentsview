package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
)

func TestModelGroupsExpand(t *testing.T) {
	groups := service.NewModelGroups(map[string]string{"qwen/qwen3.6": "qwen3.6", "qwen3.6-mlx": "qwen3.6"})
	for _, tt := range []struct{ input, want string }{
		{"qwen3.6", "qwen3.6,qwen/qwen3.6,qwen3.6-mlx"},
		{"qwen3.6-mlx", "qwen3.6-mlx"},
		{"unknown", "unknown"},
		{"qwen3.6, other,qwen3.6-mlx", "qwen3.6,qwen/qwen3.6,qwen3.6-mlx,other"},
		{"", ""},
	} {
		t.Run(tt.input, func(t *testing.T) { assert.Equal(t, tt.want, groups.Expand(tt.input)) })
	}
	assert.Equal(t, "a, b", (service.ModelGroups{}).Expand("a, b"))
}

func TestModelGroupsAliases(t *testing.T) {
	groups := service.NewModelGroups(map[string]string{"a": "group", "b": "group"})
	aliases := groups.Aliases()
	aliases["a"] = "changed"
	assert.Equal(t, "group", groups.Name("a"))
	assert.Nil(t, (service.ModelGroups{}).Aliases())
}

func TestModelGroupsGroupDaily(t *testing.T) {
	groups := service.NewModelGroups(map[string]string{"a": "group", "b": "group"})
	for _, tt := range []struct {
		name       string
		daily      []db.DailyUsageEntry
		breakdowns [][]db.ModelBreakdown
		models     []string
	}{
		{
			name: "aggregate tokens and costs",
			daily: []db.DailyUsageEntry{
				{Date: "2024-06-01", ModelBreakdowns: []db.ModelBreakdown{
					{ModelName: "a", InputTokens: 1, OutputTokens: 2, CacheCreationTokens: 3, CacheReadTokens: 4, Cost: money.Money{Microdollars: 11}},
					{ModelName: "b", InputTokens: 10, OutputTokens: 20, CacheCreationTokens: 30, CacheReadTokens: 40, Cost: money.Money{Microdollars: 22}},
				}},
				{Date: "2024-06-02", ModelBreakdowns: []db.ModelBreakdown{{ModelName: "b", InputTokens: 5, Cost: money.Money{Microdollars: 7}}}},
			},
			breakdowns: [][]db.ModelBreakdown{
				{{ModelName: "group", InputTokens: 11, OutputTokens: 22, CacheCreationTokens: 33, CacheReadTokens: 44, Cost: money.Money{Microdollars: 33}}},
				{{ModelName: "group", InputTokens: 5, Cost: money.Money{Microdollars: 7}}},
			},
			models: []string{"group"},
		},
		{
			name: "order by cost then name",
			daily: []db.DailyUsageEntry{{ModelBreakdowns: []db.ModelBreakdown{
				{ModelName: "zeta", Cost: money.Money{Microdollars: 44}},
				{ModelName: "other", Cost: money.Money{Microdollars: 30}},
				{ModelName: "a", Cost: money.Money{Microdollars: 22}},
				{ModelName: "b", Cost: money.Money{Microdollars: 22}},
			}}},
			breakdowns: [][]db.ModelBreakdown{{
				{ModelName: "group", Cost: money.Money{Microdollars: 44}},
				{ModelName: "zeta", Cost: money.Money{Microdollars: 44}},
				{ModelName: "other", Cost: money.Money{Microdollars: 30}},
			}},
			models: []string{"group", "zeta", "other"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, groups.GroupDaily(tt.daily))
			for i, want := range tt.breakdowns {
				assert.Equal(t, want, tt.daily[i].ModelBreakdowns)
				assert.Equal(t, tt.models, tt.daily[i].ModelsUsed)
			}
		})
	}
}
