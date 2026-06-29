package postgres

import (
	"context"
	"fmt"
	"strings"
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
	sessionFilter.Model = ""
	pb := &paramBuilder{}
	preds := []string{buildAnalyticsWhereWithDate(
		sessionFilter, "", pb, false, "s.id",
	)}
	flt := messageScopeFilter(f)
	modelFiltering := len(flt.Models) > 0
	query := `SELECT m.session_id, m.ordinal, m.role, m.is_system,
			COALESCE(m.model, ''), m.content,
			COALESCE(TO_CHAR(m.timestamp AT TIME ZONE 'UTC',
				'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			COALESCE(TO_CHAR(s.started_at AT TIME ZONE 'UTC',
				'YYYY-MM-DD"T"HH24:MI:SS"Z"'), ''),
			COALESCE(TO_CHAR(s.created_at AT TIME ZONE 'UTC',
				'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '')
		FROM sessions s
		JOIN messages m ON m.session_id = s.id
		WHERE ` + strings.Join(preds, " AND ") + `
			AND m.role IN ('user', 'assistant')
			AND m.is_system = FALSE
			AND ` + db.PostgresSystemPrefixSQL("m.content", "m.role") + `
		ORDER BY m.session_id, m.ordinal`

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return db.TrendsTermsResponse{}, fmt.Errorf("querying trends terms: %w", err)
	}
	defer rows.Close()

	type trendRow struct {
		sessionID string
		role      string
		isSystem  bool
		model     string
		content   string
		msgTS     string
		startedAt string
		createdAt string
	}
	processRow := func(row trendRow) {
		msgTime, ok := trendMessageLocalTime(row.msgTS, row.startedAt, row.createdAt, loc)
		if !ok {
			return
		}
		msgDate := msgTime.Format("2006-01-02")
		if !inDateRange(msgDate, f.From, f.To) {
			return
		}
		bucketDate := db.TrendBucketDate(msgTime, loc, granularity)
		bucket, ok := bucketIndex[bucketDate]
		if !ok {
			return
		}
		messageCounts[bucket]++
		for i, term := range terms {
			count := db.CountTrendOccurrences(row.content, term)
			if count > 0 {
				counts[i][bucket] += count
			}
		}
	}
	rowStartedAt := make(map[string]string)
	rowCreatedAt := make(map[string]string)
	emit := func(m db.ScopedMessage) {
		processRow(trendRow{
			sessionID: m.SessionID,
			role:      m.Role,
			isSystem:  m.IsSystem,
			content:   m.Content,
			msgTS:     m.Timestamp,
			startedAt: rowStartedAt[m.SessionID],
			createdAt: rowCreatedAt[m.SessionID],
		})
	}
	reducer := db.NewScopeReducer(flt, emit)

	for rows.Next() {
		var row trendRow
		var ordinal int
		if err := rows.Scan(
			&row.sessionID, &ordinal, &row.role, &row.isSystem, &row.model,
			&row.content, &row.msgTS, &row.startedAt, &row.createdAt,
		); err != nil {
			return db.TrendsTermsResponse{}, fmt.Errorf("scanning trends term row: %w", err)
		}
		if !modelFiltering {
			msgTime, ok := trendMessageLocalTime(row.msgTS, row.startedAt, row.createdAt, loc)
			if ok && flt.MatchesDayHour(msgTime, true) {
				processRow(row)
			}
			continue
		}
		rowStartedAt[row.sessionID] = row.startedAt
		rowCreatedAt[row.sessionID] = row.createdAt
		msgTime, has := trendMessageLocalTime(row.msgTS, row.startedAt, row.createdAt, loc)
		if err := reducer.Push(db.MessageInput{
			SessionID:    row.sessionID,
			Ordinal:      ordinal,
			Role:         row.role,
			Model:        row.model,
			IsSystem:     row.isSystem,
			Timestamp:    row.msgTS,
			LocalTime:    msgTime,
			HasLocalTime: has,
			Content:      row.content,
		}); err != nil {
			return db.TrendsTermsResponse{}, err
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
