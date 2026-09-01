package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

func (s *BunStore) GetTrendsTerms(
	ctx context.Context,
	f AnalyticsFilter,
	terms []TrendTermInput,
	granularity string,
) (TrendsTermsResponse, error) {
	if granularity == "" {
		granularity = "week"
	}
	var result TrendsTermsResponse
	err := s.consistentView(ctx, func(store bun.IDB) error {
		sessionFilter := f
		sessionFilter.From = ""
		sessionFilter.To = ""
		sessionFilter.DayOfWeek = nil
		sessionFilter.Hour = nil
		sessionFilter.Model = ""
		sessions, err := s.bunAnalyticsSessionsFrom(
			ctx, store, sessionFilter, false,
		)
		if err != nil {
			return err
		}
		result, err = buildBunTrendsTerms(
			sessions, f, terms, granularity,
			func(consume func(bunmodel.Message) error) error {
				return streamBunTrendMessages(
					ctx, store, bunAnalyticsSessionIDs(sessions), consume,
				)
			},
		)
		return err
	})
	return result, err
}

func streamBunTrendMessages(
	ctx context.Context, store bun.IDB, sessionIDs []string,
	consume func(bunmodel.Message) error,
) error {
	return queryChunkedSize(
		sessionIDs, bunAnalyticsContentSessionBatchSize,
		func(chunk []string) error {
			rows, err := store.NewSelect().Table("messages").
				Column("session_id", "ordinal", "role", "model", "is_system", "content", "timestamp").
				Where("session_id IN (?)", bun.List(chunk)).
				OrderExpr("session_id ASC, ordinal ASC").Rows(ctx)
			if err != nil {
				return err
			}
			for rows.Next() {
				var message bunmodel.Message
				var timestamp bunmodel.Timestamp
				if err := rows.Scan(
					&message.SessionID, &message.Ordinal, &message.Role, &message.Model,
					&message.IsSystem, &message.Content, &timestamp,
				); err != nil {
					_ = rows.Close()
					return fmt.Errorf("scanning Bun trend message: %w", err)
				}
				if !timestamp.IsZero() {
					message.Timestamp = &timestamp
				}
				if err := consume(message); err != nil {
					_ = rows.Close()
					return err
				}
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return fmt.Errorf("iterating Bun trend messages: %w", err)
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf("closing Bun trend messages: %w", err)
			}
			return nil
		},
	)
}

func buildBunTrendsTerms(
	sessions []bunmodel.Session,
	f AnalyticsFilter,
	terms []TrendTermInput,
	granularity string,
	stream func(func(bunmodel.Message) error) error,
) (TrendsTermsResponse, error) {
	buckets := TrendBucketRange(f.From, f.To, granularity)
	bucketIndex := trendBucketIndex(buckets)
	counts := make([][]int, len(terms))
	for i := range counts {
		counts[i] = make([]int, len(buckets))
	}
	messageCounts := make([]int, len(buckets))
	sessionMap := make(map[string]bunmodel.Session, len(sessions))
	for _, session := range sessions {
		sessionMap[session.ID] = session
	}

	process := func(sessionID, content string, localTime bunmodel.Timestamp) {
		date := localTime.Time.In(f.location()).Format("2006-01-02")
		if !inDateRange(date, f.From, f.To) {
			return
		}
		bucketDate := trendBucketDate(localTime.Time, f.location(), granularity)
		bucket, ok := bucketIndex[bucketDate]
		if !ok {
			return
		}
		messageCounts[bucket]++
		for i, term := range terms {
			counts[i][bucket] += countTrendOccurrences(content, term)
		}
		_ = sessionID
	}

	filter := f.messageScopeFilter()
	modelFiltering := strings.TrimSpace(f.Model) != ""
	if modelFiltering {
		reducer := NewScopeReducer(filter, func(message ScopedMessage) {
			if !message.HasLocalTime {
				return
			}
			process(message.SessionID, message.Content,
				bunmodel.NewTimestamp(message.LocalTime))
		})
		if err := stream(func(message bunmodel.Message) error {
			if message.Role != "user" && message.Role != "assistant" {
				return nil
			}
			if message.IsSystem || IsSystemPrefixed(message.Content, message.Role) {
				return nil
			}
			session, ok := sessionMap[message.SessionID]
			if !ok {
				return nil
			}
			local := bunAnalyticsSessionTime(session).In(f.location())
			if message.Timestamp != nil {
				local = message.Timestamp.In(f.location())
			}
			return reducer.Push(MessageInput{
				SessionID: message.SessionID, Ordinal: message.Ordinal,
				Role: message.Role, Model: message.Model, IsSystem: message.IsSystem,
				Timestamp: bunAnalyticsTimeString(message.Timestamp),
				LocalTime: local, HasLocalTime: true, Content: message.Content,
			})
		}); err != nil {
			return TrendsTermsResponse{}, err
		}
	} else {
		if err := stream(func(message bunmodel.Message) error {
			if message.Role != "user" && message.Role != "assistant" {
				return nil
			}
			if message.IsSystem || IsSystemPrefixed(message.Content, message.Role) {
				return nil
			}
			session, ok := sessionMap[message.SessionID]
			if !ok {
				return nil
			}
			local := bunAnalyticsSessionTime(session).In(f.location())
			if message.Timestamp != nil {
				local = message.Timestamp.In(f.location())
			}
			if !filter.MatchesDayHour(local, true) {
				return nil
			}
			process(message.SessionID, message.Content, bunmodel.NewTimestamp(local))
			return nil
		}); err != nil {
			return TrendsTermsResponse{}, err
		}
	}
	return BuildTrendsTermsResponse(
		f.From, f.To, granularity, buckets, terms, counts, messageCounts,
	), nil
}
