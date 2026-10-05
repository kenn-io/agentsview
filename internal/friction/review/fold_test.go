package review

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/friction"
)

func TestSpendFold(t *testing.T) {
	cost := func(micros int64) *friction.USD { u := friction.USDFromMicros(micros); return &u }
	t.Run("absent_without_stats", func(t *testing.T) {
		assert.Nil(t, newSpendFold().summary())
	})
	t.Run("roles_models_and_optional_cost", func(t *testing.T) {
		f := newSpendFold()
		f.addUsage(nil, friction.SessionUsage{
			Role: "(root)", InputTokens: 100, OutputTokens: 10,
			CostUSD: cost(3100000), ModelCosts: map[string]friction.USD{"m1": *cost(3100000)},
		})
		f.addUsage(nil, friction.SessionUsage{
			Role: "subagent", InputTokens: 5, OutputTokens: 1,
			CostUSD: cost(1100000), ModelCosts: map[string]friction.USD{"m1": *cost(1100000)},
		})
		f.addUsage(nil, friction.SessionUsage{Role: "(root)", InputTokens: 7, OutputTokens: 2})
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
		f.addUsage(nil, friction.SessionUsage{Role: "(root)", InputTokens: 10, OutputTokens: 5})
		s := f.summary()
		require.NotNil(t, s)
		assert.Nil(t, s.Total)
		assert.Equal(t, 0, s.SessionsWithCost)
	})
	t.Run("persona_counts_sessions_and_usage", func(t *testing.T) {
		f := newSpendFold()
		key := &friction.PersonaKey{Persona: "helper", Channel: "general"}
		// Persona lines stay jilog-exact: frustration and interruption are
		// not counted there (spec §9.1).
		f.countSession(key, []friction.Signal{
			{Kind: friction.KindCorrection},
			{Kind: friction.KindPattern},
			{Kind: friction.KindFrustration},
			{Kind: friction.KindInterruption},
		})
		f.addUsage(key, friction.SessionUsage{Role: "(root)", InputTokens: 12000, OutputTokens: 300})
		pc := f.personas[*key]
		require.NotNil(t, pc)
		assert.Equal(t, friction.PersonaCounts{
			Sessions: 1, Corrections: 1, Patterns: 1,
			InputTokens: 12000, OutputTokens: 300,
		}, *pc)
	})
}
