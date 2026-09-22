package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// BenchmarkExportReportingDayLongSessions measures a day export where 32
// sessions each emit a message every two minutes from noon on the prior day,
// so every hour has candidates drawn from the same long event lists.
func BenchmarkExportReportingDayLongSessions(b *testing.B) {
	d := testDB(b)
	date := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	start := date.Add(-12 * time.Hour)
	for i := range 32 {
		id := fmt.Sprintf("long-session-%d", i)
		require.NoError(b, d.UpsertSession(b.Context(), Session{
			ID: id, Project: "project-a", Agent: "claude",
			StartedAt:    Ptr(start.Format(time.RFC3339)),
			EndedAt:      Ptr(start.Add(719 * 2 * time.Minute).Format(time.RFC3339)),
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
		require.NoError(b, d.InsertMessages(b.Context(), messages))
	}
	options := ReportingExportOptions{Date: date, Now: date.Add(36 * time.Hour)}
	b.ReportAllocs()
	for b.Loop() {
		day, err := d.ExportReportingDay(b.Context(), options)
		require.NoError(b, err)
		require.Len(b, day.Hours, 24)
		require.InDelta(b, 1920, day.Hours[0].Activity.Totals.AgentMinutes, 0.0001)
	}
}
