package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTrendTerms(t *testing.T) {
	got, err := ParseTrendTerms([]string{
		" load bearing | load-bearing ",
		"seam",
		"seam | Seam | seams",
		"slic",
	})
	require.NoError(t, err, "ParseTrendTerms")
	assert.Equal(t, "load bearing", got[0].Term, "term label")
	assert.Equal(t, []string{"load bearing", "load-bearing"}, got[0].Variants, "variants")
	assert.Equal(t, []string{"seam", "seams"}, got[1].Matchers, "matchers")
	assert.Equal(t, "seam", got[2].Term, "deduped term label")
	assert.Equal(t, []string{"seam", "seams"}, got[2].Variants, "deduped variants")
	assert.Equal(t, []string{"slic", "slics", "slice", "slices", "sliced", "slicing"},
		got[3].Matchers, "stem matchers")
}

func TestParseTrendTermsValidation(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		_, err := ParseTrendTerms(nil)
		require.Error(t, err)
	})

	t.Run("empty rows dropped before limit", func(t *testing.T) {
		got, err := ParseTrendTerms([]string{"", "  ", "seam"})
		require.NoError(t, err, "ParseTrendTerms")
		require.Len(t, got, 1, "terms")
		assert.Equal(t, "seam", got[0].Term)
	})

	t.Run("more than 12 terms", func(t *testing.T) {
		values := make([]string, MaxTrendTerms+1)
		for i := range values {
			values[i] = "term"
		}
		_, err := ParseTrendTerms(values)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at most 12", "want max terms")
	})

	t.Run("more than 8 variants after dedupe", func(t *testing.T) {
		_, err := ParseTrendTerms([]string{"a|b|c|d|e|f|g|h|i"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at most 8", "want max variants")
	})

	t.Run("variant limit after dedupe", func(t *testing.T) {
		got, err := ParseTrendTerms([]string{"a|A|b|c|d|e|f|g|h"})
		require.NoError(t, err, "ParseTrendTerms")
		assert.Len(t, got[0].Variants, MaxTrendTermVariants, "variant count")
	})
}

func TestCountTrendOccurrences(t *testing.T) {
	term := TrendTermInput{
		Term:     "seam",
		Variants: []string{"seam"},
		Matchers: []string{"seam", "seams"},
	}
	cases := []struct {
		name string
		text string
		want int
	}{
		{"case insensitive", "Seam seam SEAMS", 3},
		{"word boundary", "seamless seam seams", 2},
		{"overlap plural", "seams", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, countTrendOccurrences(tc.text, term))
		})
	}
}

func TestCountTrendOccurrencesSilentEStem(t *testing.T) {
	terms, err := ParseTrendTerms([]string{"slic"})
	require.NoError(t, err, "ParseTrendTerms")
	got := countTrendOccurrences(
		"slice slices sliced slicing slicer sliced-up",
		terms[0],
	)
	assert.Equal(t, 5, got)
}

func TestCountTrendOccurrencesPhrases(t *testing.T) {
	term := TrendTermInput{
		Term:     "load bearing",
		Variants: []string{"load bearing", "load-bearing"},
		Matchers: []string{"load bearing", "load-bearing"},
	}
	got := countTrendOccurrences("Load bearing and load-bearing", term)
	assert.Equal(t, 2, got)
}

func TestTrendBucketDate(t *testing.T) {
	loc := time.UTC
	cases := []struct {
		gran string
		ts   string
		want string
	}{
		{"day", "2024-06-05T12:00:00Z", "2024-06-05"},
		{"week", "2024-06-05T12:00:00Z", "2024-06-03"},
		{"month", "2024-06-05T12:00:00Z", "2024-06-01"},
	}
	for _, tc := range cases {
		parsed, _ := time.Parse(time.RFC3339, tc.ts)
		assert.Equal(t, tc.want, trendBucketDate(parsed, loc, tc.gran), tc.gran)
	}
}

func TestGetTrendsTermsSQLite(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	start := "2024-06-01T09:00:00Z"
	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.StartedAt = &start
		s.CreatedAt = start
		s.MessageCount = 3
		s.UserMessageCount = 2
	})
	insertMessages(t, d,
		Message{SessionID: "s1", Ordinal: 0, Role: "user", Content: "load bearing seam", Timestamp: "2024-06-01T09:00:00Z", ContentLength: 17},
		Message{SessionID: "s1", Ordinal: 1, Role: "assistant", Content: "load-bearing seams seam", Timestamp: "2024-06-08T09:00:00Z", ContentLength: 23},
		Message{SessionID: "s1", Ordinal: 2, Role: "user", Content: "seam system", Timestamp: "2024-06-08T09:00:00Z", ContentLength: 11, IsSystem: true},
	)
	terms, err := ParseTrendTerms([]string{"load bearing | load-bearing", "seam"})
	require.NoError(t, err)
	got, err := d.GetTrendsTerms(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-09", Timezone: "UTC",
	}, terms, "week")
	require.NoError(t, err, "GetTrendsTerms")
	assert.Equal(t, []string{"2024-05-27", "2024-06-03"},
		trendBucketDates(got.Buckets), "bucket dates")
	assert.Equal(t, []int{1, 1}, trendBucketMessageCounts(got.Buckets),
		"bucket message counts")
	assert.Equal(t, 2, got.MessageCount, "message count")
	byTerm := trendSeriesByTerm(got.Series)
	assert.Equal(t, 2, byTerm["load bearing"].Total, "load bearing total")
	assert.Equal(t, 3, byTerm["seam"].Total, "seam total")
	assert.Equal(t, []int{1, 1}, trendPointCounts(byTerm["load bearing"].Points),
		"load bearing points")
	assert.Equal(t, []int{1, 2}, trendPointCounts(byTerm["seam"].Points),
		"seam points")
}

func TestGetTrendsTermsSQLiteProjectFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	start := "2024-06-01T09:00:00Z"
	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.StartedAt = &start
		s.CreatedAt = start
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	insertSession(t, d, "s2", "proj-b", func(s *Session) {
		s.StartedAt = &start
		s.CreatedAt = start
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	insertMessages(t, d,
		Message{SessionID: "s1", Ordinal: 0, Role: "user", Content: "seam", Timestamp: start, ContentLength: 4},
		Message{SessionID: "s2", Ordinal: 0, Role: "user", Content: "seam", Timestamp: start, ContentLength: 4},
	)
	terms, err := ParseTrendTerms([]string{"seam"})
	require.NoError(t, err)
	got, err := d.GetTrendsTerms(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC", Project: "proj-a",
	}, terms, "day")
	require.NoError(t, err, "GetTrendsTerms")
	assert.Equal(t, 1, trendSeriesByTerm(got.Series)["seam"].Total,
		"project-filtered total")
}

func TestGetTrendsTermsSQLiteModelFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	start := "2024-06-01T09:00:00Z"
	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.StartedAt = &start
		s.CreatedAt = start
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	insertSession(t, d, "s2", "proj-a", func(s *Session) {
		s.StartedAt = &start
		s.CreatedAt = start
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	insertMessages(t, d,
		Message{
			SessionID: "s1", Ordinal: 0, Role: "user",
			Content: "seam", Timestamp: start, ContentLength: 4,
			Model: "gpt-4o",
		},
		Message{
			SessionID: "s2", Ordinal: 0, Role: "user",
			Content: "seam", Timestamp: start, ContentLength: 4,
			Model: "claude-3-5-sonnet",
		},
	)
	terms, err := ParseTrendTerms([]string{"seam"})
	require.NoError(t, err)
	got, err := d.GetTrendsTerms(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, terms, "day")
	require.NoError(t, err, "GetTrendsTerms")
	assert.Equal(t, 1, got.MessageCount, "message count")
	assert.Equal(t, 1, trendSeriesByTerm(got.Series)["seam"].Total,
		"model-filtered total")
}

func TestGetTrendsTermsSQLiteModelFilterStaysOnMatchingMessages(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()
	start := "2024-06-01T09:00:00Z"
	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.StartedAt = &start
		s.CreatedAt = start
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	insertMessages(t, d,
		Message{
			SessionID: "s1", Ordinal: 0, Role: "user",
			Content: "seam", Timestamp: start, ContentLength: 4,
		},
		Message{
			SessionID: "s1", Ordinal: 1, Role: "assistant",
			Content: "ready", Timestamp: start, ContentLength: 5,
			Model: "gpt-4o",
		},
		Message{
			SessionID: "s1", Ordinal: 2, Role: "assistant",
			Content: "seam seam", Timestamp: start, ContentLength: 9,
			Model: "claude-3-5-sonnet",
		},
	)
	terms, err := ParseTrendTerms([]string{"seam"})
	require.NoError(t, err)
	got, err := d.GetTrendsTerms(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, terms, "day")
	require.NoError(t, err, "GetTrendsTerms")
	assert.Equal(t, 2, got.MessageCount, "message count")
	assert.Equal(t, 1, trendSeriesByTerm(got.Series)["seam"].Total,
		"model-filtered total")
}

func TestGetTrendsTermsSQLiteUsesMessageTimestampRange(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	start := "2024-05-01T09:00:00Z"
	created := "2024-05-01T08:00:00Z"
	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.StartedAt = &start
		s.CreatedAt = created
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	insertMessages(t, d,
		Message{SessionID: "s1", Ordinal: 0, Role: "user", Content: "seam", Timestamp: "2024-06-05T09:00:00Z", ContentLength: 4},
	)
	terms, err := ParseTrendTerms([]string{"seam"})
	require.NoError(t, err)
	got, err := d.GetTrendsTerms(ctx, AnalyticsFilter{
		From: "2024-06-05", To: "2024-06-05", Timezone: "UTC",
	}, terms, "day")
	require.NoError(t, err, "GetTrendsTerms")
	assert.Equal(t, 1, trendSeriesByTerm(got.Series)["seam"].Total,
		"message timestamp total")
}

func TestGetTrendsTermsSQLiteDoesNotFilterBySessionTimestamp(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	start := "not-a-time"
	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.StartedAt = &start
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	insertMessages(t, d,
		Message{SessionID: "s1", Ordinal: 0, Role: "user", Content: "seam", Timestamp: "2024-06-05T09:00:00Z", ContentLength: 4},
	)
	terms, err := ParseTrendTerms([]string{"seam"})
	require.NoError(t, err)
	got, err := d.GetTrendsTerms(ctx, AnalyticsFilter{
		From: "2024-06-05", To: "2024-06-05", Timezone: "UTC",
	}, terms, "day")
	require.NoError(t, err, "GetTrendsTerms")
	assert.Equal(t, 1, trendSeriesByTerm(got.Series)["seam"].Total,
		"message timestamp total")
}

func TestGetTrendsTermsSQLiteAppliesDayAndHourToMessageTimestamp(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	start := "2024-06-04T08:00:00Z"
	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.StartedAt = &start
		s.MessageCount = 2
		s.UserMessageCount = 2
	})
	insertMessages(t, d,
		Message{SessionID: "s1", Ordinal: 0, Role: "user", Content: "seam", Timestamp: "2024-06-05T09:00:00Z", ContentLength: 4},
		Message{SessionID: "s1", Ordinal: 1, Role: "user", Content: "seam", Timestamp: "2024-06-05T10:00:00Z", ContentLength: 4},
	)
	terms, err := ParseTrendTerms([]string{"seam"})
	require.NoError(t, err)
	dow := 2
	hour := 9
	got, err := d.GetTrendsTerms(ctx, AnalyticsFilter{
		From:      "2024-06-05",
		To:        "2024-06-05",
		Timezone:  "UTC",
		DayOfWeek: &dow,
		Hour:      &hour,
	}, terms, "day")
	require.NoError(t, err, "GetTrendsTerms")
	assert.Equal(t, 1, trendSeriesByTerm(got.Series)["seam"].Total,
		"hour-filtered message total")
}

func TestGetTrendsTermsSQLiteTimestampFallback(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	start := "2024-06-05T09:00:00Z"
	created := "2024-06-04T08:00:00Z"
	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.StartedAt = &start
		s.CreatedAt = created
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	insertMessages(t, d,
		Message{SessionID: "s1", Ordinal: 0, Role: "user", Content: "seam", Timestamp: "not-a-time", ContentLength: 4},
	)
	terms, err := ParseTrendTerms([]string{"seam"})
	require.NoError(t, err)
	got, err := d.GetTrendsTerms(ctx, AnalyticsFilter{
		From: "2024-06-05", To: "2024-06-05", Timezone: "UTC",
	}, terms, "day")
	require.NoError(t, err, "GetTrendsTerms")
	assert.Equal(t, 1, trendSeriesByTerm(got.Series)["seam"].Total,
		"fallback timestamp total")
}

func TestGetTrendsTermsSQLiteExcludesLegacySystemPrefixes(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	start := "2024-06-01T09:00:00Z"
	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.StartedAt = &start
		s.CreatedAt = start
		s.MessageCount = 2
		s.UserMessageCount = 2
	})
	insertMessages(t, d,
		Message{SessionID: "s1", Ordinal: 0, Role: "user", Content: SystemMsgPrefixes[0] + " seam", Timestamp: start, ContentLength: 40},
		Message{SessionID: "s1", Ordinal: 1, Role: "user", Content: "seam", Timestamp: start, ContentLength: 4},
	)
	terms, err := ParseTrendTerms([]string{"seam"})
	require.NoError(t, err)
	got, err := d.GetTrendsTerms(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
	}, terms, "day")
	require.NoError(t, err, "GetTrendsTerms")
	assert.Equal(t, 1, trendSeriesByTerm(got.Series)["seam"].Total,
		"system-prefix-filtered total")
}

func trendBucketDates(buckets []TrendBucket) []string {
	dates := make([]string, len(buckets))
	for i, bucket := range buckets {
		dates[i] = bucket.Date
	}
	return dates
}

func trendBucketMessageCounts(buckets []TrendBucket) []int {
	counts := make([]int, len(buckets))
	for i, bucket := range buckets {
		counts[i] = bucket.MessageCount
	}
	return counts
}

func trendSeriesByTerm(series []TrendSeries) map[string]TrendSeries {
	byTerm := make(map[string]TrendSeries, len(series))
	for _, entry := range series {
		byTerm[entry.Term] = entry
	}
	return byTerm
}

func trendPointCounts(points []TrendPoint) []int {
	counts := make([]int, len(points))
	for i, point := range points {
		counts[i] = point.Count
	}
	return counts
}
