package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// seedMessageShapeBenchmark writes 2,000 sessions of 50 messages each with
// transcript-sized bodies (1-3 KB, plus thinking text on some assistant
// turns). Analytics that only need a message's role and flags must not pay
// for reading those bodies.
func seedMessageShapeBenchmark(b *testing.B) *DB {
	b.Helper()
	d := testDB(b)
	ctx := b.Context()
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 1999)
		INSERT INTO sessions(id, project, agent, started_at, ended_at, created_at,
			message_count, user_message_count)
		SELECT printf('shape-%04d', i), 'project-' || (i % 20), 'claude',
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-03-01T00:00:00Z', printf('+%d minutes', i * 60)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-03-01T00:50:00Z', printf('+%d minutes', i * 60)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-03-01T00:00:00Z', printf('+%d minutes', i * 60)),
			50, 25
		FROM n`,
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 49)
		INSERT INTO messages(session_id, ordinal, role, content, thinking_text,
			timestamp, has_thinking, has_tool_use, content_length, is_system, model,
			source_subtype)
		SELECT s.id, n.i,
			CASE WHEN n.i % 2 = 0 THEN 'user' ELSE 'assistant' END,
			substr(hex(zeroblob(1500)), 1, 1000 + (n.i * 397) % 2000),
			CASE WHEN n.i % 4 = 1 THEN substr(hex(zeroblob(500)), 1, 800) ELSE '' END,
			strftime('%Y-%m-%dT%H:%M:%SZ', s.started_at, printf('+%d minutes', n.i)),
			CASE WHEN n.i % 4 = 1 THEN 1 ELSE 0 END,
			CASE WHEN n.i % 4 = 3 THEN 1 ELSE 0 END,
			1000 + (n.i * 397) % 2000,
			CASE WHEN n.i = 48 THEN 1 ELSE 0 END,
			CASE WHEN n.i % 2 = 1 THEN 'claude-sonnet-4-5' ELSE '' END,
			CASE WHEN n.i % 6 = 2 THEN 'tool_result' ELSE '' END
		FROM sessions s CROSS JOIN n`,
		`ANALYZE`,
	} {
		_, err := d.getWriter().Exec(ctx, stmt)
		require.NoError(b, err)
	}
	return d
}

var messageShapeBenchFilter = AnalyticsFilter{
	From: "2026-03-01", To: "2026-06-30", Timezone: "UTC",
}

func BenchmarkGetAnalyticsSessionShapeWideMessages(b *testing.B) {
	d := seedMessageShapeBenchmark(b)
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		shape, err := d.GetAnalyticsSessionShape(ctx, messageShapeBenchFilter)
		require.NoError(b, err)
		require.Equal(b, 2000, shape.Count)
	}
}

func BenchmarkGetAnalyticsActivityWideMessages(b *testing.B) {
	d := seedMessageShapeBenchmark(b)
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		activity, err := d.GetAnalyticsActivity(ctx, messageShapeBenchFilter, "day")
		require.NoError(b, err)
		require.NotEmpty(b, activity.Series)
	}
}

func BenchmarkGetSessionStatsWideMessages(b *testing.B) {
	d := seedMessageShapeBenchmark(b)
	ctx := b.Context()
	f := StatsFilter{Since: "2026-03-01", Until: "2026-06-30", Timezone: "UTC"}
	b.ReportAllocs()
	for b.Loop() {
		stats, err := d.GetSessionStats(ctx, f)
		require.NoError(b, err)
		require.Equal(b, 2000, stats.Totals.SessionsAll)
	}
}
