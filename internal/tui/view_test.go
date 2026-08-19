package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

var benchmarkViewContent string

func TestTruncateWidthPreservesVisibleWidth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
		width int
		want  string
	}{
		{name: "already fits", value: "abc", width: 3, want: "abc"},
		{name: "ascii", value: "abcdef", width: 4, want: "abc…"},
		{name: "wide unicode", value: "世界hello", width: 4, want: "世…"},
		{name: "ellipsis only", value: "abcdef", width: 1, want: "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, truncateWidth(tt.value, tt.width))
		})
	}
}

func TestTruncateWidthScalesLinearlyWithInputLength(t *testing.T) {
	smallInput := strings.Repeat("x", 256)
	largeInput := strings.Repeat("x", 4096)
	var smallOutput, largeOutput string

	smallStart := time.Now()
	for range 16 {
		smallOutput = truncateWidth(smallInput, 8)
	}
	smallDuration := time.Since(smallStart)
	largeStart := time.Now()
	largeOutput = truncateWidth(largeInput, 8)
	largeDuration := time.Since(largeStart)

	assert.Equal(t, "xxxxxxx…", smallOutput)
	assert.Equal(t, "xxxxxxx…", largeOutput)
	assert.Less(t, largeDuration, smallDuration*3,
		"16x more input must not cause quadratic truncation work")
}

func TestViewRefreshesCachedMessageContent(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.width, m.height, m.focus = 100, 30, 2
	m.detail = &service.SessionDetail{Session: db.Session{ID: "session", MessageCount: 1}}
	m.messages = []db.Message{{ID: 1, SessionID: "session", Role: "assistant", Content: "first value"}}

	first := m.View().Content
	m.messages[0].Content = "replacement value"
	second := m.View().Content

	assert.Contains(t, first, "first")
	assert.Contains(t, second, "replacement")
	assert.NotContains(t, second, "first")
}

func BenchmarkModelViewLongTranscript(b *testing.B) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.width, m.height, m.focus = 160, 50, 2
	m.detail = &service.SessionDetail{Session: db.Session{
		ID: "large-session", DisplayName: new("Large session"), MessageCount: 200,
	}}
	content := "## Result\n\n" + strings.Repeat("performance-sensitive transcript content ", 128)
	m.messages = make([]db.Message, 200)
	for i := range m.messages {
		m.messages[i] = db.Message{
			ID: int64(i + 1), SessionID: "large-session", Ordinal: i,
			Role: "assistant", Content: fmt.Sprintf("%s\n\nmessage %d", content, i),
		}
	}
	m.messageSelected = 100

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkViewContent = m.View().Content
	}
}
