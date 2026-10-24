package review

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
)

func TestSpendFold(t *testing.T) {
	cost := func(micros int64) *friction.USD { u := friction.USDFromMicros(micros); return &u }
	t.Run("absent_without_stats", func(t *testing.T) {
		assert.Nil(t, newSpendFold().summary())
	})
	t.Run("roles_models_and_optional_cost", func(t *testing.T) {
		f := newSpendFold()
		f.addUsage(db.FrictionSessionUsage{
			Role: "(root)", InputTokens: 100, OutputTokens: 10,
			CostUSD: cost(3100000), ModelCosts: map[string]friction.USD{"m1": *cost(3100000)},
		})
		f.addUsage(db.FrictionSessionUsage{
			Role: "subagent", InputTokens: 5, OutputTokens: 1,
			CostUSD: cost(1100000), ModelCosts: map[string]friction.USD{"m1": *cost(1100000)},
		})
		f.addUsage(db.FrictionSessionUsage{Role: "(root)", InputTokens: 7, OutputTokens: 2})
		s := f.summary()
		require.NotNil(t, s)
		assert.Equal(t, 3, s.SessionsWithStats)
		assert.Equal(t, 2, s.SessionsWithCost)
		assert.Equal(t, uint64(112), s.InputTokens)
		assert.Equal(t, uint64(13), s.OutputTokens)
		assert.Equal(t, "4.200000", s.Total.String())
		assert.Equal(t, "3.100000", s.RoleCosts["(root)"].String())
		assert.Equal(t, "1.100000", s.RoleCosts["subagent"].String())
		assert.Equal(t, "4.200000", s.ModelCosts["m1"].String())
	})
	t.Run("all_unpriced_has_no_total", func(t *testing.T) {
		f := newSpendFold()
		f.addUsage(db.FrictionSessionUsage{Role: "(root)", InputTokens: 10, OutputTokens: 5})
		s := f.summary()
		require.NotNil(t, s)
		assert.Nil(t, s.Total)
		assert.Equal(t, 0, s.SessionsWithCost)
	})
}
