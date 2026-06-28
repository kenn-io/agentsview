//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestStoreGetTrendsTerms(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_trends_terms_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		) VALUES (
			'trends-pg-001', 'test-machine', 'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz,
			'2024-06-01T10:00:00Z'::timestamptz,
			3, 2
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system
		) VALUES
			('trends-pg-001', 0, 'user', 'load bearing seam',
			 '2024-06-01T09:00:00Z'::timestamptz, 17, FALSE),
			('trends-pg-001', 1, 'assistant', 'load-bearing seams seam',
			 '2024-06-08T09:00:00Z'::timestamptz, 23, FALSE),
			('trends-pg-001', 2, 'user', 'seam system',
			 '2024-06-08T09:00:00Z'::timestamptz, 11, TRUE)`)
	require.NoError(t, err, "insert messages")
	terms, err := db.ParseTrendTerms([]string{"load bearing | load-bearing", "seam"})
	require.NoError(t, err, "ParseTrendTerms")
	got, err := store.GetTrendsTerms(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-09", Timezone: "UTC",
	}, terms, "week")
	require.NoError(t, err, "GetTrendsTerms")
	assert.Equal(t, []string{"2024-05-27", "2024-06-03"},
		trendBucketDates(got.Buckets))
	assert.Equal(t, []int{1, 1},
		trendBucketMessageCounts(got.Buckets))
	assert.Equal(t, 2, got.MessageCount)
	byTerm := trendSeriesByTerm(got.Series)
	assert.Equal(t, 2, byTerm["load bearing"].Total)
	assert.Equal(t, 3, byTerm["seam"].Total)
	assert.Equal(t, []int{1, 1},
		trendPointCounts(byTerm["load bearing"].Points))
	assert.Equal(t, []int{1, 2},
		trendPointCounts(byTerm["seam"].Points))
}

func TestStoreGetTrendsTermsUsesMessageTimestampFilters(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_trends_terms_message_filters_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'trends-pg-message-filters-001', 'test-machine',
			'alpha', 'claude',
			'2024-06-04T08:00:00Z'::timestamptz, 2, 2
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system
		) VALUES
			('trends-pg-message-filters-001', 0, 'user', 'seam',
			 '2024-06-05T09:00:00Z'::timestamptz, 4, FALSE),
			('trends-pg-message-filters-001', 1, 'user', 'seam',
			 '2024-06-05T10:00:00Z'::timestamptz, 4, FALSE)`)
	require.NoError(t, err, "insert messages")
	terms, err := db.ParseTrendTerms([]string{"seam"})
	require.NoError(t, err, "ParseTrendTerms")
	dow := 2
	hour := 9
	got, err := store.GetTrendsTerms(ctx, db.AnalyticsFilter{
		From:      "2024-06-05",
		To:        "2024-06-05",
		Timezone:  "UTC",
		DayOfWeek: &dow,
		Hour:      &hour,
	}, terms, "day")
	require.NoError(t, err, "GetTrendsTerms")
	assert.Equal(t, 1, got.MessageCount)
	assert.Equal(t, 1, trendSeriesByTerm(got.Series)["seam"].Total)
}

func TestStoreGetTrendsTermsModelFilterStaysOnMatchingMessages(
	t *testing.T,
) {
	_, store := prepareUsageSchema(t, "agentsview_trends_terms_model_messages_test")
	ctx := context.Background()
	_, err := store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at,
			message_count, user_message_count
		) VALUES (
			'trends-pg-model-messages-001', 'test-machine',
			'alpha', 'claude',
			'2024-06-01T09:00:00Z'::timestamptz, 3, 1
		)`)
	require.NoError(t, err, "insert session")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, is_system, model
		) VALUES
			('trends-pg-model-messages-001', 0, 'user', 'seam',
			 '2024-06-01T09:00:00Z'::timestamptz, 4, FALSE, ''),
			('trends-pg-model-messages-001', 1, 'assistant', 'ready',
			 '2024-06-01T09:01:00Z'::timestamptz, 5, FALSE, 'gpt-4o'),
			('trends-pg-model-messages-001', 2, 'assistant', 'seam seam',
			 '2024-06-01T09:05:00Z'::timestamptz, 9, FALSE, 'claude-3-5-sonnet')`)
	require.NoError(t, err, "insert messages")
	terms, err := db.ParseTrendTerms([]string{"seam"})
	require.NoError(t, err, "ParseTrendTerms")
	got, err := store.GetTrendsTerms(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, terms, "day")
	require.NoError(t, err, "GetTrendsTerms")
	assert.Equal(t, 2, got.MessageCount)
	assert.Equal(t, 1, trendSeriesByTerm(got.Series)["seam"].Total)
}

func trendBucketDates(buckets []db.TrendBucket) []string {
	dates := make([]string, len(buckets))
	for i, bucket := range buckets {
		dates[i] = bucket.Date
	}
	return dates
}

func trendBucketMessageCounts(buckets []db.TrendBucket) []int {
	counts := make([]int, len(buckets))
	for i, bucket := range buckets {
		counts[i] = bucket.MessageCount
	}
	return counts
}

func trendSeriesByTerm(series []db.TrendSeries) map[string]db.TrendSeries {
	byTerm := make(map[string]db.TrendSeries, len(series))
	for _, entry := range series {
		byTerm[entry.Term] = entry
	}
	return byTerm
}

func trendPointCounts(points []db.TrendPoint) []int {
	counts := make([]int, len(points))
	for i, point := range points {
		counts[i] = point.Count
	}
	return counts
}
