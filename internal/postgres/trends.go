package postgres

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) GetTrendsTerms(
	ctx context.Context,
	f db.AnalyticsFilter,
	terms []db.TrendTermInput,
	granularity string,
) (db.TrendsTermsResponse, error) {
	if granularity == "" {
		granularity = "week"
	}
	loc := analyticsLocation(f)
	buckets := db.TrendBucketRange(f.From, f.To, granularity)
	bucketIndex := trendBucketIndex(buckets)
	counts := make([][]int, len(terms))
	for i := range counts {
		counts[i] = make([]int, len(buckets))
	}
	messageCounts := make([]int, len(buckets))

	sessionFilter := f
	sessionFilter.From = ""
	sessionFilter.To = ""
	sessionFilter.DayOfWeek = nil
	sessionFilter.Hour = nil
	pb := &paramBuilder{}
	where := buildAnalyticsWhereWithoutDate(sessionFilter, pb)
	query := `SELECT m.content,
			COALESCE(TO_CHAR(m.timestamp AT TIME ZONE 'UTC',
				'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			COALESCE(TO_CHAR(s.started_at AT TIME ZONE 'UTC',
				'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			COALESCE(TO_CHAR(s.created_at AT TIME ZONE 'UTC',
				'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')
		FROM sessions s
		JOIN messages m ON m.session_id = s.id
		WHERE ` + where + `
			AND m.role IN ('user', 'assistant')
			AND m.is_system = FALSE
			AND ` + db.PostgresSystemPrefixSQL("m.content", "m.role")

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return db.TrendsTermsResponse{}, fmt.Errorf("querying trends terms: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var content, msgTS, startedAt, createdAt string
		if err := rows.Scan(
			&content, &msgTS, &startedAt, &createdAt,
		); err != nil {
			return db.TrendsTermsResponse{}, fmt.Errorf("scanning trends term row: %w", err)
		}
		msgTime, ok := trendMessageLocalTime(msgTS, startedAt, createdAt, loc)
		if !ok {
			continue
		}
		if f.HasTimeFilter() && !matchesTimeFilter(f, msgTime) {
			continue
		}
		msgDate := msgTime.Format("2006-01-02")
		if !inDateRange(msgDate, f.From, f.To) {
			continue
		}
		bucketDate := db.TrendBucketDate(msgTime, loc, granularity)
		bucket, ok := bucketIndex[bucketDate]
		if !ok {
			continue
		}
		messageCounts[bucket]++
		for i, term := range terms {
			count := db.CountTrendOccurrences(content, term)
			if count > 0 {
				counts[i][bucket] += count
			}
		}
	}
	if err := rows.Err(); err != nil {
		return db.TrendsTermsResponse{}, fmt.Errorf("iterating trends term rows: %w", err)
	}

	return db.BuildTrendsTermsResponse(
		f.From, f.To, granularity, buckets, terms, counts, messageCounts,
	), nil
}

func trendMessageLocalTime(
	messageTS string,
	startedAt string,
	createdAt string,
	loc *time.Location,
) (time.Time, bool) {
	for _, ts := range []string{messageTS, startedAt, createdAt} {
		if t, ok := localTime(ts, loc); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func trendBucketIndex(buckets []db.TrendBucket) map[string]int {
	index := make(map[string]int, len(buckets))
	for i, bucket := range buckets {
		index[bucket.Date] = i
	}
	return index
}
