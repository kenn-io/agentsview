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

// BenchmarkExportReportingDayLargeArchive exports one day from an archive
// that holds two years of usage (146,000 usage rows). Finding the day's usage sessions should touch
// that day's rows, not every usage row in the archive.
func BenchmarkExportReportingDayLargeArchive(b *testing.B) {
	d := testDB(b)
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 7299)
		INSERT INTO sessions(id, project, agent, started_at, ended_at,
			message_count, user_message_count)
		SELECT printf('usage-%04d', i), 'project-' || (i % 10), 'claude',
			strftime('%Y-%m-%dT%H:%M:%SZ', '2024-07-29T00:00:00Z', printf('+%d minutes', i * 144)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2024-07-29T00:39:00Z', printf('+%d minutes', i * 144)),
			40, 20
		FROM n`,
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 39)
		INSERT INTO messages(session_id, ordinal, role, content, timestamp, model, token_usage)
		SELECT s.id, n.i,
			CASE WHEN n.i % 2 = 0 THEN 'user' ELSE 'assistant' END,
			'benchmark message',
			strftime('%Y-%m-%dT%H:%M:%SZ', s.started_at, printf('+%d minutes', n.i)),
			CASE WHEN n.i % 2 = 1 THEN 'claude-sonnet-4-5' ELSE '' END,
			CASE WHEN n.i % 2 = 1
				THEN '{"input_tokens":100,"output_tokens":40,"cache_read_input_tokens":2000}'
				ELSE '' END
		FROM sessions s CROSS JOIN n`,
		`ANALYZE`,
	} {
		_, err := d.getWriter().Exec(b.Context(), stmt)
		require.NoError(b, err)
	}
	date := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	options := ReportingExportOptions{Date: date, Now: date.Add(36 * time.Hour)}
	b.ReportAllocs()
	for b.Loop() {
		day, err := d.ExportReportingDay(b.Context(), options)
		require.NoError(b, err)
		require.Len(b, day.Hours, 24)
		require.True(b, day.Hours[12].HasData)
	}
}
