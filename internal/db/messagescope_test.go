package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
