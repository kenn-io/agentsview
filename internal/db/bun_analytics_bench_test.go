package db

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	benchmarkTrendsTermsResult TrendsTermsResponse
	benchmarkSignalsResult     SignalsAnalyticsResponse
)

func BenchmarkBunContentAnalyticsStreaming(b *testing.B) {
	database := testDB(b)
	const (
		sessionCount       = 128
		messagesPerSession = 8
	)
	started := "2026-08-04T12:00:00Z"
	content := strings.Repeat("this is broken again seam ", 256)
	messages := make([]Message, 0, sessionCount*messagesPerSession)
	for sessionIndex := range sessionCount {
		id := fmt.Sprintf("content-bench-%03d", sessionIndex)
		require.NoError(b, database.UpsertSession(Session{
			ID: id, Project: "content-bench", Machine: "host", Agent: "codex",
			CreatedAt: started, StartedAt: &started,
			MessageCount: messagesPerSession, UserMessageCount: messagesPerSession,
		}))
		for ordinal := range messagesPerSession {
			messages = append(messages, Message{
				SessionID: id, Ordinal: ordinal, Role: "user", Content: content,
				ContentLength: len(content), Timestamp: started,
			})
		}
	}
	require.NoError(b, database.InsertMessages(messages))
	_, err := database.getWriter().Exec(
		"UPDATE sessions SET quality_signal_version = ?",
		CurrentQualitySignalVersion,
	)
	require.NoError(b, err)
	terms, err := ParseTrendTerms([]string{"seam"})
	require.NoError(b, err)
	filter := AnalyticsFilter{
		Project: "content-bench", From: "2026-08-04", To: "2026-08-04",
		Timezone: "UTC",
	}
	bytesPerOperation := int64(len(content) * len(messages))

	b.Run("trends", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(bytesPerOperation)
		for range b.N {
			result, err := database.GetTrendsTerms(b.Context(), filter, terms, "day")
			if err != nil {
				b.Fatal(err)
			}
			benchmarkTrendsTermsResult = result
		}
	})

	b.Run("signals", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(bytesPerOperation)
		for range b.N {
			result, err := database.GetAnalyticsSignals(b.Context(), filter)
			if err != nil {
				b.Fatal(err)
			}
			benchmarkSignalsResult = result
		}
	})
}
