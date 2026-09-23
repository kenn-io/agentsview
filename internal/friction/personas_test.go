package friction

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonaDisplayKey(t *testing.T) {
	tests := []struct {
		name string
		key  PersonaKey
		want string
	}{
		{"persona_and_channel", PersonaKey{"helper", "general"}, "helper@general"},
		{"bare_persona_without_channel", PersonaKey{"reviewer", ""}, "reviewer"},
		{"digest_dims_sanitize_hostile_channel_names", PersonaKey{"helper", "general`\ninjected"}, "helper@general' injected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, PersonaDisplayKey(tt.key))
		})
	}
}

// Ports display_keyed_personas_disambiguates_colliding_keys
// (digest.rs:1830-1851).
func TestDisplayKeyedPersonas(t *testing.T) {
	t.Run("display_keyed_personas_disambiguates_colliding_keys", func(t *testing.T) {
		m := map[PersonaKey]*PersonaCounts{
			{"helper", "q&a@hq"}: {Sessions: 1},
			{"helper@q&a", "hq"}: {Sessions: 3},
		}
		got := DisplayKeyedPersonas(m)
		require.Len(t, got, 2, "colliding display keys must stay separate")
		assert.Equal(t, "helper@q&a@hq", got[0].Key)
		assert.Equal(t, "helper", got[0].PersonaKey.Persona)
		assert.Equal(t, 1, got[0].Counts.Sessions)
		assert.Equal(t, "helper@q&a@hq (2)", got[1].Key)
		assert.Equal(t, "helper@q&a", got[1].PersonaKey.Persona)
		assert.Equal(t, 3, got[1].Counts.Sessions)
	})
	t.Run("sorted_bytewise_by_display_key", func(t *testing.T) {
		m := map[PersonaKey]*PersonaCounts{
			{"zed", ""}:       {Sessions: 1},
			{"Alpha", "b"}:    {Sessions: 1},
			{"alpha", ""}:     {Sessions: 1},
			{"alpha", "chan"}: {Sessions: 1},
		}
		keys := []string{}
		for _, e := range DisplayKeyedPersonas(m) {
			keys = append(keys, e.Key)
		}
		assert.Equal(t, []string{"Alpha@b", "alpha", "alpha@chan", "zed"}, keys)
	})
	t.Run("third_collision_gets_3", func(t *testing.T) {
		m := map[PersonaKey]*PersonaCounts{
			{"a", "b@c"}:  {Sessions: 1},
			{"a@b", "c"}:  {Sessions: 2},
			{"a@b@c", ""}: {Sessions: 3},
		}
		got := DisplayKeyedPersonas(m)
		require.Len(t, got, 3)
		assert.Equal(t, []string{"a@b@c", "a@b@c (2)", "a@b@c (3)"},
			[]string{got[0].Key, got[1].Key, got[2].Key})
		assert.Equal(t, []int{1, 2, 3},
			[]int{got[0].Counts.Sessions, got[1].Counts.Sessions, got[2].Counts.Sessions})
	})
	t.Run("nil_counts_are_zero", func(t *testing.T) {
		got := DisplayKeyedPersonas(map[PersonaKey]*PersonaCounts{{"x", ""}: nil})
		require.Len(t, got, 1)
		assert.Equal(t, PersonaCounts{}, got[0].Counts)
	})
}

// Fold-level halves of personas_rollup_carries_tokens_and_optional_cost
// (digest.rs:1986-2060) and stats_only_session_counts_toward_spend_and_personas
// (:2206-2238). PR 5 ports the run_review halves.
func TestPersonaCountsFold(t *testing.T) {
	t.Run("personas_rollup_carries_tokens_and_optional_cost", func(t *testing.T) {
		cell := &PersonaCounts{Sessions: 1}
		cell.AddSignals([]Signal{{Kind: KindCorrection}})
		cell.AddUsage(SessionUsage{SubjectID: "sess-cell", InputTokens: 12000, OutputTokens: 300})
		assert.Equal(t, uint64(12000), cell.InputTokens)
		assert.Equal(t, uint64(300), cell.OutputTokens)
		assert.Nil(t, cell.CostUSD, "cell sessions must stay tokens-only")

		priced := &PersonaCounts{Sessions: 1}
		priced.AddSignals(nil)
		priced.AddUsage(SessionUsage{SubjectID: "sess-priced", InputTokens: 800, OutputTokens: 40, CostUSD: usdPtr(mustUSD(t, "1.5"))})
		require.NotNil(t, priced.CostUSD)
		assert.Equal(t, "1.5", priced.CostUSD.String())
		assert.Equal(t, 1, priced.Sessions)
	})
	t.Run("stats_only_session_counts_toward_spend_and_personas", func(t *testing.T) {
		c := &PersonaCounts{}
		c.Sessions++ // the stats-only path counts the session itself
		c.AddUsage(SessionUsage{SubjectID: "s", InputTokens: 5, OutputTokens: 1})
		assert.Equal(t, 1, c.Sessions)
		assert.Equal(t, 0, c.signalTotal())
		assert.True(t, c.hasUsage())
	})
	t.Run("add_signals_counts_each_kind", func(t *testing.T) {
		c := &PersonaCounts{}
		c.AddSignals([]Signal{
			{Kind: KindCorrection}, {Kind: KindCorrection}, {Kind: KindError},
			{Kind: KindWorkaround}, {Kind: KindDeferral}, {Kind: KindPattern},
			// D36: persona counts stay jilog's five kinds.
			{Kind: KindFrustration}, {Kind: KindInterruption},
		})
		assert.Equal(t, PersonaCounts{Corrections: 2, Errors: 1, Workarounds: 1, Deferrals: 1, Patterns: 1}, *c)
		assert.Equal(t, 6, c.signalTotal())
	})
	t.Run("cost_sums_across_sessions", func(t *testing.T) {
		c := &PersonaCounts{}
		c.AddUsage(SessionUsage{CostUSD: usdPtr(mustUSD(t, "0.25"))})
		c.AddUsage(SessionUsage{CostUSD: usdPtr(mustUSD(t, "0.25"))})
		require.NotNil(t, c.CostUSD)
		assert.Equal(t, "0.50", c.CostUSD.String())
	})
}
