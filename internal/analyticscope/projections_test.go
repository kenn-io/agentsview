package analyticscope

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestStats(t *testing.T) {
	rows := []ScopedMessage{
		{Role: "user"},
		{Role: "user", IsSystem: true},
		{Role: "assistant", HasToolUse: true, HasThinking: true, HasOutputTokens: true, OutputTokens: 10},
		{Role: "assistant", HasOutputTokens: true, OutputTokens: 5},
	}
	s := Stats(rows)
	assert.Equal(t, 4, s.Messages)
	assert.Equal(t, 1, s.UserMessages) // system user not counted
	assert.Equal(t, 2, s.AssistantMessages)
	assert.Equal(t, 1, s.ToolUseMessages)
	assert.Equal(t, 1, s.ThinkingMessages)
	assert.Equal(t, 15, s.OutputTokens)
	assert.True(t, s.HasOutputTokens)
}

func TestTiming(t *testing.T) {
	now := time.Date(2024, 1, 1, 14, 0, 0, 0, time.UTC)
	rows := []ScopedMessage{
		{Role: "user", LocalTime: now, HasLocalTime: true, ContentLength: 12},
		{Role: "assistant", HasLocalTime: false, ContentLength: 0},
	}
	got := Timing(rows)
	assert.Equal(t, []TimingMessage{
		{Role: "user", Time: now, Valid: true, ContentLength: 12},
		{Role: "assistant", Time: time.Time{}, Valid: false, ContentLength: 0},
	}, got)
}
