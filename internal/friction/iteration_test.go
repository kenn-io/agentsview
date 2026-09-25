package friction

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/signals"
)

// runawayInput translates ordered user turns and tool calls into the separate
// slices carried by PatternInput. Ordinal gaps stand for assistant text rows.
type runawayStep struct {
	user   bool
	minute int
}

func calls(n, startMinute int) []runawayStep {
	out := make([]runawayStep, n)
	for i := range out {
		out[i] = runawayStep{minute: startMinute + i}
	}
	return out
}

func runawayInput(groups ...[]runawayStep) PatternInput {
	var in PatternInput
	ordinal := 0
	for _, group := range groups {
		for _, step := range group {
			if step.user {
				in.UserOrdinals = append(in.UserOrdinals, ordinal)
			} else {
				in.Calls = append(in.Calls, signals.ToolCallRow{
					ToolName: "bash", InputJSON: fmt.Sprintf(`{"step":%d}`, ordinal), MessageOrdinal: ordinal,
				})
				in.CallTimes = append(in.CallTimes, fixtureBase.Add(time.Duration(step.minute)*time.Minute))
			}
			ordinal += 2
		}
	}
	return in
}

// These cases port jilog's iteration_runaway tests in health.rs:521-590.
func TestDetectIterationRunaway(t *testing.T) {
	userTurn := []runawayStep{{user: true}}
	tests := []struct {
		name     string
		subAgent bool
		input    PatternInput
		fires    bool
		evidence string
	}{
		{"iteration_runaway_fires_at_threshold", false, runawayInput(calls(150, 0)), true, "150 tool calls without a user message 09:00-11:29"},
		{"iteration_runaway_silent_below_threshold", false, runawayInput(calls(149, 0)), false, ""},
		{"iteration_runaway_root_session_fires_at_150/100", false, runawayInput(calls(100, 0)), false, ""},
		{"iteration_runaway_root_session_fires_at_150/149", false, runawayInput(calls(149, 0)), false, ""},
		{"iteration_runaway_root_session_fires_at_150/150", false, runawayInput(calls(150, 0)), true, "150 tool calls without a user message 09:00-11:29"},
		{"iteration_runaway_exempts_sub_agent_sessions/sub_agent", true, runawayInput(calls(200, 0)), false, ""},
		{"iteration_runaway_exempts_sub_agent_sessions/root", false, runawayInput(calls(200, 0)), true, "200 tool calls without a user message 09:00-12:19"},
		{"iteration_runaway_user_message_resets_count", false, runawayInput(calls(149, 0), userTurn, calls(149, 150)), false, ""},
		{"iteration_runaway_llm_responses_do_not_reset", false, runawayInput(calls(150, 0)), true, "150 tool calls without a user message 09:00-11:29"},
		{"iteration_runaway_reports_longest_stretch", false, runawayInput(calls(151, 0), userTurn, calls(155, 152)), true, "155 tool calls without a user message 11:32-14:06"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sig, ok := detectIterationRunaway("s1", tt.subAgent, tt.input)
			require.Equal(t, tt.fires, ok)
			if !ok {
				assert.Equal(t, Signal{}, sig)
				return
			}
			assert.Equal(t, PatternIterationRunaway, sig.Label)
			assert.Equal(t, tt.evidence, sig.Evidence)
		})
	}
	assert.Equal(t, 150, IterationRunawayMinToolCalls)
}

func TestDetectIterationRunawayFields(t *testing.T) {
	in := runawayInput(calls(151, 0), []runawayStep{{user: true}}, calls(155, 152))
	sig, ok := detectIterationRunaway("sess", false, in)
	require.True(t, ok)
	assert.Equal(t, KindPattern, sig.Kind)
	assert.Equal(t, SubjectSession, sig.SubjectKind)
	assert.Equal(t, "sess", sig.SubjectID)
	assert.Equal(t, "pattern.iteration_runaway", sig.Detector)
	assert.Equal(t, "iteration_runaway", sig.Label)
	assert.Equal(t, "iteration runaway: 155 tool calls with no intervening user message", sig.Text)
	assert.Equal(t, "155 tool calls without a user message 11:32-14:06", sig.Evidence)
	require.NotNil(t, sig.Ordinal)
	assert.Equal(t, in.Calls[151].MessageOrdinal, *sig.Ordinal)
	assert.Nil(t, sig.CallIndex)
	assert.Equal(t, in.CallTimes[151], sig.OccurredAt)
}

func TestDetectIterationRunawayTieKeepsFirst(t *testing.T) {
	in := runawayInput(calls(150, 0), []runawayStep{{user: true}}, calls(150, 200))
	sig, ok := detectIterationRunaway("s1", false, in)
	require.True(t, ok)
	assert.Equal(t, "150 tool calls without a user message 09:00-11:29", sig.Evidence)
	require.NotNil(t, sig.Ordinal)
	assert.Equal(t, 0, *sig.Ordinal)
}

func TestDetectIterationRunawayOrdersByOrdinal(t *testing.T) {
	in := runawayInput(calls(151, 0), []runawayStep{{user: true}}, calls(155, 152))
	// Keep each call paired with its timestamp while scrambling input order
	// across the user turn and within the longest stretch.
	for _, pair := range [][2]int{{0, 305}, {151, 200}} {
		in.Calls[pair[0]], in.Calls[pair[1]] = in.Calls[pair[1]], in.Calls[pair[0]]
		in.CallTimes[pair[0]], in.CallTimes[pair[1]] = in.CallTimes[pair[1]], in.CallTimes[pair[0]]
	}
	sig, ok := detectIterationRunaway("s1", false, in)
	require.True(t, ok)
	assert.Equal(t, "iteration runaway: 155 tool calls with no intervening user message", sig.Text)
	assert.Equal(t, "155 tool calls without a user message 11:32-14:06", sig.Evidence)
	require.NotNil(t, sig.Ordinal)
	assert.Equal(t, 304, *sig.Ordinal)
	assert.Equal(t, time.Date(2026, 1, 1, 11, 32, 0, 0, time.UTC), sig.OccurredAt)
}

func TestDetectIterationRunawayUserFirstOnTie(t *testing.T) {
	in := runawayInput(calls(150, 0))
	in.UserOrdinals = []int{in.Calls[0].MessageOrdinal}
	sig, ok := detectIterationRunaway("s1", false, in)
	require.True(t, ok)
	assert.Equal(t, "150 tool calls without a user message 09:00-11:29", sig.Evidence)
}

func TestFormatRange(t *testing.T) {
	tokyo := time.FixedZone("JST", 9*3600)
	first := fixtureBase.Add(time.Minute)
	last := fixtureBase.Add(8 * time.Minute)
	assert.Equal(t, "09:01-09:08", formatRange(first, last))
	assert.Equal(t, "09:01-09:08", formatRange(first.In(tokyo), last.In(tokyo)))
}

func TestDetectIterationRunawayMissingTimes(t *testing.T) {
	in := runawayInput(calls(150, 0))
	in.CallTimes = nil
	sig, ok := detectIterationRunaway("s1", false, in)
	require.True(t, ok)
	assert.Equal(t, "150 tool calls without a user message", sig.Evidence)
	assert.True(t, sig.OccurredAt.IsZero())
}
