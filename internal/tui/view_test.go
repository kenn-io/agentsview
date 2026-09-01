package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
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

func TestAdaptiveMarkdownShowsContinuationForLargeMessage(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.width, m.height, m.focus = 100, 24, 2
	m.detail = &service.SessionDetail{Session: db.Session{
		ID: "session", MessageCount: 1,
	}}
	m.messages = []db.Message{{
		ID: 1, SessionID: "session", Role: "assistant",
		Content: "## Result\n\n" + strings.Repeat("visible content ", 1<<16) +
			"\nEND-OF-LARGE-MESSAGE",
	}}

	view := m.View().Content

	assert.Contains(t, view, "Result")
	assert.Contains(t, view, "message continues")
	assert.NotContains(t, view, "END-OF-LARGE-MESSAGE")
}

func TestAdaptiveMarkdownKeepsCompleteSmallMessage(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	message := db.Message{
		ID: 1, SessionID: "session", Role: "assistant",
		Content: "## Complete\n\nAll content is visible.",
	}

	lines := m.renderMessageLines(0, message, 80, 20)
	rendered := strings.Join(lines, "\n")

	assert.Contains(t, rendered, "All content is")
	assert.Contains(t, rendered, "visible.")
	assert.NotContains(t, rendered, "message continues")
	require.Len(t, m.renderedMessages, 1)
	for _, cached := range m.renderedMessages {
		assert.True(t, cached.complete)
	}
}

func TestAdaptiveMarkdownAllocationsAreBoundedByViewport(t *testing.T) {
	makeModel := func(size int) *model {
		m := newModel(context.Background(), &fakeDataClient{}, Options{})
		m.width, m.height, m.focus = 100, 30, 2
		m.detail = &service.SessionDetail{Session: db.Session{
			ID: "session", MessageCount: 1,
		}}
		m.messages = []db.Message{{
			ID: 1, SessionID: "session", Role: "assistant",
			Content: "## Result\n\n" + strings.Repeat("word ", size/5),
		}}
		_ = m.View()
		return m
	}
	allocations := func(m *model) float64 {
		return testing.AllocsPerRun(3, func() {
			m.clearRenderedMessages()
			benchmarkViewContent = m.View().Content
		})
	}
	small := allocations(makeModel(12 << 10))
	large := allocations(makeModel(1 << 20))

	assert.Less(t, large, small*2,
		"1 MiB transcript must not allocate twice as much as 12 KiB")
}

func TestRenderedMessageCacheIsByteBounded(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	line := strings.Repeat("x", 64<<10)
	for i := range 100 {
		key := renderedMessageKey{messageID: int64(i + 1), index: i}
		m.cacheRenderedMessage(key, line, renderedMessage{
			lines: []string{line}, complete: true,
		})
	}

	assert.LessOrEqual(t, m.renderedMessageBytes, renderCacheMaxBytes)
	assert.Less(t, len(m.renderedMessages), 100)
}

func TestRenderedMessageCacheSeparatesRenderInputs(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	message := db.Message{
		ID: 7, SessionID: "session", Role: "assistant",
		Content: strings.Repeat("content ", 100),
	}
	m.sessionLoadGeneration = 1
	_ = m.renderMessageLines(0, message, 80, 5)
	_ = m.renderMessageLines(0, message, 81, 5)
	_ = m.renderMessageLines(0, message, 81, 6)
	m.theme = "light"
	_ = m.renderMessageLines(0, message, 81, 6)
	m.messageLayout = "compact"
	_ = m.renderMessageLines(0, message, 81, 6)
	m.sessionLoadGeneration = 2
	_ = m.renderMessageLines(0, message, 81, 6)

	assert.Len(t, m.renderedMessages, 6)
}
func TestSingleLineToolPayloadWorkIsBoundedByViewport(t *testing.T) {
	makeModel := func(result string) *model {

		m := newModel(context.Background(), &fakeDataClient{}, Options{})
		m.width, m.height, m.focus, m.showTools = 100, 30, 2, true
		m.detail = &service.SessionDetail{Session: db.Session{
			ID: "session", MessageCount: 1,
		}}
		m.messages = []db.Message{{
			ID: 1, SessionID: "session", Role: "assistant", Content: "done",
			ToolCalls: []db.ToolCall{{
				ToolName: "Read", ResultContent: result,
			}},
		}}
		return m
	}
	small := makeModel(strings.Repeat("x", 1<<10))
	large := makeModel(strings.Repeat("x", 1<<20))
	smallAllocs := testing.AllocsPerRun(3, func() { _ = small.View() })
	largeAllocs := testing.AllocsPerRun(3, func() { _ = large.View() })

	assert.Less(t, largeAllocs, smallAllocs*2,
		"single-line tool payload work must not scale with hidden bytes")
}

func TestTranscriptRenderWorkIsBoundedByViewport(t *testing.T) {
	makeModel := func(result string) *model {
		m := newModel(context.Background(), &fakeDataClient{}, Options{})
		m.width, m.height, m.focus, m.showTools = 100, 30, 2, true
		m.detail = &service.SessionDetail{Session: db.Session{ID: "session", MessageCount: 1}}
		m.messages = []db.Message{{
			ID: 1, SessionID: "session", Role: "assistant", Content: "done",
			ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: result}},
		}}
		return m
	}
	small := makeModel(strings.Repeat("tool result\n", 100))
	large := makeModel(strings.Repeat("tool result\n", 10_000))

	smallView := small.View().Content
	largeView := large.View().Content
	smallAllocs := testing.AllocsPerRun(3, func() { _ = small.View() })
	largeAllocs := testing.AllocsPerRun(3, func() { _ = large.View() })

	assert.Contains(t, smallView, "tool Read")
	assert.Contains(t, largeView, "tool Read")
	assert.Less(t, largeAllocs, smallAllocs*2,
		"100x more off-screen tool output must keep per-redraw work bounded")
}

func TestCachedReportRenderWorkIsBoundedByResultSize(t *testing.T) {
	makeModel := func(rowCount int) *model {
		m := newModel(context.Background(), &fakeDataClient{}, Options{})
		m.width, m.height, m.focus, m.page = 100, 30, 1, PageActivity
		m.pageData.Activity = &activity.Report{BySession: make([]activity.SessionRow, rowCount)}
		for i := range m.pageData.Activity.BySession {
			m.pageData.Activity.BySession[i] = activity.SessionRow{
				SessionID: fmt.Sprintf("session-%d", i),
				Title:     fmt.Sprintf("Session %d", i),
			}
		}
		_ = m.View()
		return m
	}
	small := makeModel(100)
	large := makeModel(10_000)

	smallAllocs := testing.AllocsPerRun(3, func() { _ = small.View() })
	largeAllocs := testing.AllocsPerRun(3, func() { _ = large.View() })

	assert.Less(t, largeAllocs, smallAllocs*2,
		"100x more cached off-screen report rows must keep per-redraw work bounded")
}

func TestPageLoadInvalidatesCachedReport(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.width, m.height, m.focus, m.page, m.generation = 100, 30, 1, PageActivity, 1
	m.pageData.Activity = &activity.Report{BySession: []activity.SessionRow{{Title: "old row"}}}
	assert.Contains(t, m.View().Content, "old row")

	next, _ := m.Update(pageLoadedMsg{
		generation: 1,
		page:       PageActivity,
		data: PageData{Activity: &activity.Report{
			BySession: []activity.SessionRow{{Title: "new row"}},
		}},
	})

	view := next.(*model).View().Content
	assert.Contains(t, view, "new row")
	assert.NotContains(t, view, "old row")
}

func TestUsageRendersCompletedRankingWhileSummaryLoads(t *testing.T) {
	m := newModel(context.Background(), &fakeDataClient{}, Options{})
	m.width, m.height, m.focus, m.page, m.loading = 100, 30, 1, PageUsage, true
	m.pageData.UsageTopSessions = []db.TopSessionEntry{{
		DisplayName: "fast result",
		Agent:       "codex",
		TotalTokens: 7,
	}}

	report := m.renderReport(80, 20)

	assert.Contains(t, report, "Top sessions by cost")
	assert.Contains(t, report, "fast result")
	assert.NotContains(t, report, m.strings.Loading)
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

func BenchmarkModelViewAdaptiveMarkdown(b *testing.B) {
	for _, size := range []int{12 << 10, 100 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("bytes-%d", size), func(b *testing.B) {
			m := newModel(context.Background(), &fakeDataClient{}, Options{})
			m.width, m.height, m.focus = 100, 30, 2
			m.detail = &service.SessionDetail{Session: db.Session{
				ID: "session", MessageCount: 1,
			}}
			m.messages = []db.Message{{
				ID: 1, SessionID: "session", Role: "assistant",
				Content: "## Result\n\n" + strings.Repeat("word ", size/5),
			}}
			_ = m.View()

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				m.clearRenderedMessages()
				benchmarkViewContent = m.View().Content
			}
		})
	}
}
func BenchmarkModelViewToolResult(b *testing.B) {
	for _, lineCount := range []int{100, 10_000} {
		b.Run(fmt.Sprintf("lines-%d", lineCount), func(b *testing.B) {
			m := newModel(context.Background(), &fakeDataClient{}, Options{})
			m.width, m.height, m.focus, m.showTools = 100, 30, 2, true
			m.detail = &service.SessionDetail{Session: db.Session{ID: "session", MessageCount: 1}}
			m.messages = []db.Message{{
				ID: 1, SessionID: "session", Role: "assistant", Content: "done",
				ToolCalls: []db.ToolCall{{
					ToolName: "Read", ResultContent: strings.Repeat("tool result\n", lineCount),
				}},
			}}
			_ = m.View()

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				benchmarkViewContent = m.View().Content
			}
		})
	}
}

func BenchmarkModelViewCachedActivityReport(b *testing.B) {
	for _, rowCount := range []int{100, 10_000} {
		b.Run(fmt.Sprintf("rows-%d", rowCount), func(b *testing.B) {
			m := newModel(context.Background(), &fakeDataClient{}, Options{})
			m.width, m.height, m.focus, m.page = 100, 30, 1, PageActivity
			m.pageData.Activity = &activity.Report{BySession: make([]activity.SessionRow, rowCount)}
			for i := range m.pageData.Activity.BySession {
				m.pageData.Activity.BySession[i].Title = fmt.Sprintf("Session %d", i)
			}
			_ = m.View()

			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				benchmarkViewContent = m.View().Content
			}
		})
	}
}
