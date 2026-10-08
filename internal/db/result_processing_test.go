package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeSessionLimit(t *testing.T) {
	for _, tc := range []struct{ input, want int }{
		{-1, DefaultSessionLimit},
		{0, DefaultSessionLimit},
		{1, 1},
		{MaxSessionLimit, MaxSessionLimit},
		{MaxSessionLimit + 1, DefaultSessionLimit},
	} {
		assert.Equal(t, tc.want, NormalizeSessionLimit(tc.input))
	}
}

func TestBuildSessionPage(t *testing.T) {
	f := SessionFilter{Limit: 1, OrderBy: "messages"}
	sessions := []Session{
		{ID: "first", MessageCount: 5},
		{ID: "extra", MessageCount: 6},
	}
	var cursor SessionCursor
	encode := func(c SessionCursor) string { cursor = c; return "encoded" }
	page := BuildSessionPage(sessions, 7, f, ResolveSort(f), encode)
	assert.Equal(t, sessions[:1], page.Sessions)
	assert.Equal(t, 7, page.Total)
	assert.Equal(t, "encoded", page.NextCursor)
	assert.Equal(t, "first", cursor.ID)
	assert.Equal(t, 7, cursor.Total)
	assert.Equal(t, "5", cursor.Value)
	for _, rows := range [][]Session{nil, {}, sessions[:1]} {
		page = BuildSessionPage(rows, 7, f, ResolveSort(f), func(SessionCursor) string {
			require.FailNow(t, "last page must not encode a cursor")
			return ""
		})
		assert.Equal(t, rows, page.Sessions)
		assert.Empty(t, page.NextCursor)
	}
}

func TestBuildHeatmapResponse(t *testing.T) {
	const from, to = "2026-01-01", "2026-01-02"
	uncovered := BuildHeatmapResponse(from, to, "output_tokens", nil)
	assert.Nil(t, uncovered.Entries)
	assert.Equal(t, HeatmapLevels{}, uncovered.Levels)
	assert.Equal(t, from, uncovered.EntriesFrom)

	covered := BuildHeatmapResponse(from, to, "output_tokens", map[string]int{from: 0})
	require.Len(t, covered.Entries, 2)
	assert.Equal(t, HeatmapEntry{Date: from}, covered.Entries[0])
	assert.Equal(t, HeatmapLevels{L1: 1, L2: 2, L3: 3, L4: 4}, covered.Levels)

	source := map[string]int{
		"2020-01-01": 1000000,
		"2026-01-01": 1,
		"2026-01-02": 2,
		"2026-01-03": 3,
		"2026-01-04": 4,
	}
	out := BuildHeatmapResponse("2020-01-01", "2026-01-04", "messages", source)
	require.Len(t, out.Entries, MaxHeatmapDays)
	assert.Equal(t, "2025-01-04", out.EntriesFrom)
	assert.Equal(t, HeatmapLevels{L1: 1, L2: 2, L3: 3, L4: 4}, out.Levels,
		"days clamped out of the range must not skew levels")
	assert.Equal(t, 3, out.Entries[len(out.Entries)-1].Level)
}

func TestTrendAccumulator(t *testing.T) {
	terms, err := ParseTrendTerms([]string{"cat"})
	require.NoError(t, err)
	at := func(date string) time.Time {
		parsed, err := time.Parse("2006-01-02", date)
		require.NoError(t, err)
		return parsed.Add(12 * time.Hour)
	}
	for _, tc := range []struct{ granularity, bucket string }{
		{"day", "2026-01-04"}, {"week", "2025-12-29"}, {"month", "2026-01-01"},
	} {
		t.Run(tc.granularity, func(t *testing.T) {
			acc := NewTrendAccumulator("2026-01-04", "2026-01-05", tc.granularity, terms)
			acc.Add("cats cat", at("2026-01-04"))
			acc.Add("unmatched", at("2026-01-05"))
			acc.Add("cat", at("2026-01-03"))
			acc.Add("cat", at("2026-01-06"))
			out := acc.Response()
			assert.Equal(t, 2, out.MessageCount)
			require.Len(t, out.Series, 1)
			assert.Equal(t, 2, out.Series[0].Total)
			assert.Equal(t, TrendPoint{Date: tc.bucket, Count: 2}, out.Series[0].Points[0])
		})
	}
	empty := NewTrendAccumulator("bad", "2026-01-05", "week", terms)
	empty.Add("cat", at("2026-01-05"))
	assert.Empty(t, empty.Response().Buckets)
	assert.Zero(t, empty.Response().MessageCount)
}

func TestMessageScopeProjections(t *testing.T) {
	rows := []ScopedMessage{
		{SessionID: "session", Role: "user", Timestamp: "2026-01-01T00:00:00Z"},
		{
			SessionID: "session", Role: "assistant", Timestamp: "2026-01-01T00:00:01.123Z",
			OutputTokens: 8, HasOutputTokens: true,
		},
	}
	scope := MessageScope{"session": rows}
	assert.Equal(t, ScopeStats(rows), scope.StatsBySession()["session"])
	assert.Equal(t, ScopeTiming(rows), scope.TimingBySession()["session"])
	hour := 12
	filter := AnalyticsFilter{Model: " a, b, a, ,", Hour: &hour}.MessageScopeFilter()
	assert.Equal(t, map[string]struct{}{"a": {}, "b": {}}, filter.Models)
	assert.Equal(t, &hour, filter.Hour)
}
