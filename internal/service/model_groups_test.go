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
	assert.Equal(t, "qwen3.6", groups.Name("qwen/qwen3.6"))
	assert.Equal(t, "unknown", groups.Name("unknown"))
}

func TestModelGroupsGroupDaily(t *testing.T) {
	groups := service.NewModelGroups(map[string]string{"a": "group", "b": "group"})
	daily := []db.DailyUsageEntry{
		{Date: "2024-06-01", ModelBreakdowns: []db.ModelBreakdown{
			{ModelName: "a", InputTokens: 1, OutputTokens: 2, CacheCreationTokens: 3, CacheReadTokens: 4, Cost: money.Money{Microdollars: 11}},
			{ModelName: "b", InputTokens: 10, OutputTokens: 20, CacheCreationTokens: 30, CacheReadTokens: 40, Cost: money.Money{Microdollars: 22}},
		}},
		{Date: "2024-06-02", ModelBreakdowns: []db.ModelBreakdown{{ModelName: "b", InputTokens: 5, Cost: money.Money{Microdollars: 7}}}},
		{Date: "2024-06-03"},
	}
	require.NoError(t, groups.GroupDaily(daily))
	assert.Equal(t, []db.ModelBreakdown{{ModelName: "group", InputTokens: 11, OutputTokens: 22, CacheCreationTokens: 33, CacheReadTokens: 44, Cost: money.Money{Microdollars: 33}}}, daily[0].ModelBreakdowns)
	assert.Equal(t, []string{"group"}, daily[0].ModelsUsed)
	assert.Equal(t, []db.ModelBreakdown{{ModelName: "group", InputTokens: 5, Cost: money.Money{Microdollars: 7}}}, daily[1].ModelBreakdowns)
	assert.Nil(t, daily[2].ModelBreakdowns)
	assert.Nil(t, daily[2].ModelsUsed)
	raw := []db.DailyUsageEntry{{ModelBreakdowns: []db.ModelBreakdown{{ModelName: "a"}}}}
	require.NoError(t, (service.ModelGroups{}).GroupDaily(raw))
	assert.Equal(t, "a", raw[0].ModelBreakdowns[0].ModelName)
}
