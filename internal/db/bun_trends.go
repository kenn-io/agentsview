package db

import (
	"context"
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
		messages, err := bunAnalyticsMessagesFrom(
			ctx, store, bunAnalyticsSessionIDs(sessions),
		)
		if err != nil {
			return err
		}
		result, err = buildBunTrendsTerms(sessions, messages, f, terms, granularity)
		return err
	})
	return result, err
}

func buildBunTrendsTerms(
	sessions []bunmodel.Session,
	messages []bunmodel.Message,
	f AnalyticsFilter,
	terms []TrendTermInput,
	granularity string,
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
		for _, message := range messages {
			if message.Role != "user" && message.Role != "assistant" {
				continue
			}
			if message.IsSystem || IsSystemPrefixed(message.Content, message.Role) {
				continue
			}
			session, ok := sessionMap[message.SessionID]
			if !ok {
				continue
			}
			local := bunAnalyticsSessionTime(session).In(f.location())
			if message.Timestamp != nil {
				local = message.Timestamp.In(f.location())
			}
			if err := reducer.Push(MessageInput{
				SessionID: message.SessionID, Ordinal: message.Ordinal,
				Role: message.Role, Model: message.Model, IsSystem: message.IsSystem,
				Timestamp: bunAnalyticsTimeString(message.Timestamp),
				LocalTime: local, HasLocalTime: true, Content: message.Content,
			}); err != nil {
				return TrendsTermsResponse{}, err
			}
		}
	} else {
		for _, message := range messages {
			if message.Role != "user" && message.Role != "assistant" {
				continue
			}
			if message.IsSystem || IsSystemPrefixed(message.Content, message.Role) {
				continue
			}
			session, ok := sessionMap[message.SessionID]
			if !ok {
				continue
			}
			local := bunAnalyticsSessionTime(session).In(f.location())
			if message.Timestamp != nil {
				local = message.Timestamp.In(f.location())
			}
			if !filter.MatchesDayHour(local, true) {
				continue
			}
			process(message.SessionID, message.Content, bunmodel.NewTimestamp(local))
		}
	}
	return BuildTrendsTermsResponse(
		f.From, f.To, granularity, buckets, terms, counts, messageCounts,
	), nil
}
