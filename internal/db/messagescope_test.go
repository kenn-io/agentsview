package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScopeFilterMatchesDayHour(t *testing.T) {
	// 2024-01-01 is a Monday (ISO day 0); hour 14 UTC.
	mon14 := time.Date(2024, 1, 1, 14, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		f    ScopeFilter
		t    time.Time
		has  bool
		want bool
	}{
		{"no constraint matches even unparsed", ScopeFilter{}, time.Time{}, false, true},
		{"day match", ScopeFilter{DayOfWeek: new(0)}, mon14, true, true},
		{"day mismatch", ScopeFilter{DayOfWeek: new(1)}, mon14, true, false},
		{"hour match", ScopeFilter{Hour: new(14)}, mon14, true, true},
		{"hour mismatch", ScopeFilter{Hour: new(9)}, mon14, true, false},
		{"constraint but unparsed never matches", ScopeFilter{Hour: new(14)}, time.Time{}, false, false},
		{"day and hour both match", ScopeFilter{DayOfWeek: new(0), Hour: new(14)}, mon14, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.f.MatchesDayHour(tc.t, tc.has))
		})
	}
}

func TestScopeStats(t *testing.T) {
	rows := []ScopedMessage{
		{Role: "user"},
		{Role: "user", IsSystem: true},
		{Role: "assistant", HasToolUse: true, HasThinking: true, HasOutputTokens: true, OutputTokens: 10},
		{Role: "assistant", HasOutputTokens: true, OutputTokens: 5},
	}
	s := ScopeStats(rows)
	assert.Equal(t, 4, s.Messages)
	assert.Equal(t, 1, s.UserMessages) // system user not counted
	assert.Equal(t, 2, s.AssistantMessages)
	assert.Equal(t, 1, s.ToolUseMessages)
	assert.Equal(t, 1, s.ThinkingMessages)
	assert.Equal(t, 15, s.OutputTokens)
	assert.True(t, s.HasOutputTokens)
}

func TestScopeTiming(t *testing.T) {
	now := time.Date(2024, 1, 1, 14, 0, 0, 0, time.UTC)
	rows := []ScopedMessage{
		{Role: "user", LocalTime: now, HasLocalTime: true, ContentLength: 12},
		{Role: "assistant", HasLocalTime: false, ContentLength: 0},
	}
	got := ScopeTiming(rows)
	assert.Equal(t, []TimingMessage{
		{Role: "user", Time: now, Valid: true, ContentLength: 12},
		{Role: "assistant", Time: time.Time{}, Valid: false, ContentLength: 0},
	}, got)
}

func TestScopeTimingRestoresOrdinalOrderAfterReducer(t *testing.T) {
	// The reviewer's case: an empty-model user (ord 0) is buffered behind a
	// model-bearing user (ord 1) that emits immediately, then the selected
	// assistant (ord 2) flushes the buffer. The reducer therefore emits out of
	// ordinal order, but velocity pairs each prompt with the following response
	// by position, so ScopeTiming must restore conversation order.
	f := ScopeFilter{Models: map[string]struct{}{"sonnet": {}}}
	t0 := time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)
	rows := []MessageInput{
		{SessionID: "s1", Ordinal: 0, Role: "user", HasLocalTime: true, LocalTime: t0, ContentLength: 10},
		{SessionID: "s1", Ordinal: 1, Role: "user", Model: "sonnet", HasLocalTime: true, LocalTime: t0.Add(time.Minute), ContentLength: 20},
		{SessionID: "s1", Ordinal: 2, Role: "assistant", Model: "sonnet", HasLocalTime: true, LocalTime: t0.Add(2 * time.Minute), ContentLength: 30},
	}
	emitted, err := collectScopeRows(t, f, rows)
	require.NoError(t, err)
	require.Len(t, emitted, 3)
	require.Equal(t, []int{1, 0, 2},
		[]int{emitted[0].Ordinal, emitted[1].Ordinal, emitted[2].Ordinal},
		"reducer emits the model-bearing user ahead of the buffered user")

	got := ScopeTiming(emitted)
	assert.Equal(t, []TimingMessage{
		{Role: "user", Time: t0, Valid: true, ContentLength: 10},
		{Role: "user", Time: t0.Add(time.Minute), Valid: true, ContentLength: 20},
		{Role: "assistant", Time: t0.Add(2 * time.Minute), Valid: true, ContentLength: 30},
	}, got, "velocity timing restored to ordinal order")
}
