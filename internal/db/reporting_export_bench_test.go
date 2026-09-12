package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func BenchmarkExportReportingDayLongSessions(b *testing.B) {
	d := testDB(b)
	date := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	start := date.Add(-12 * time.Hour)
	for i := range 32 {
		id := fmt.Sprintf("long-session-%d", i)
		require.NoError(b, d.UpsertSession(Session{
			ID: id, Project: "project-a", Agent: "claude",
			StartedAt:    new(start.Format(time.RFC3339)),
			EndedAt:      new(start.Add(719 * 2 * time.Minute).Format(time.RFC3339)),
			MessageCount: 720, UserMessageCount: 1,
		}))
		messages := make([]Message, 720)
		for j := range messages {
			messages[j] = Message{
				SessionID: id, Ordinal: j, Role: "assistant", Model: "model-a",
				Content:   "benchmark activity",
				Timestamp: start.Add(time.Duration(j) * 2 * time.Minute).Format(time.RFC3339),
			}
		}
		messages[0].Role = "user"
		require.NoError(b, d.InsertMessages(messages))
	}
	options := ReportingExportOptions{Date: date, Now: date.Add(36 * time.Hour)}
	b.ReportAllocs()
	for b.Loop() {
		day, err := d.ExportReportingDay(context.Background(), options)
		require.NoError(b, err)
		require.Len(b, day.Hours, 24)
		require.InDelta(b, 1920, day.Hours[0].Activity.Totals.AgentMinutes, 0.0001)
	}
}
