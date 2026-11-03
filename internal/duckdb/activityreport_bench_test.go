//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

// Activity report benchmark archive: sessions start every
// activityBenchSpacingSeconds from 2026-01-01 UTC, so 20,000 sessions span
// about 300 days and a one-day window selects roughly 66 of them. Each
// session alternates user and assistant messages 45 seconds apart.
const (
	activityBenchSessions        = 20_000
	activityBenchMessages        = 100
	activityBenchSpacingSeconds  = 1_300
	activityBenchMessageInterval = 45
)

// seedActivityReportBenchStore writes the archive with set-based SQL into
// an on-disk DuckDB file so seeding takes seconds and the fixture stays
// a few hundred megabytes.
func seedActivityReportBenchStore(b *testing.B) *Store {
	b.Helper()
	ctx := b.Context()
	conn, err := openDuckDB(filepath.Join(b.TempDir(), "activity-bench.duckdb"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, conn.Close()) })
	require.NoError(b, configureDuckDBThreads(ctx, conn))
	require.NoError(b, EnsureSchema(ctx, conn))

	_, err = conn.ExecContext(ctx, `
		INSERT INTO sessions (
			id, project, machine, agent, started_at, ended_at,
			message_count, user_message_count, data_version
		)
		SELECT 'bench-' || lpad(CAST(i AS VARCHAR), 6, '0'),
			'project-' || CAST(i % 50 AS VARCHAR), 'bench',
			CASE i % 2 WHEN 0 THEN 'claude' ELSE 'codex' END,
			TIMESTAMP '2026-01-01 00:00:00' + to_seconds(i * $3),
			TIMESTAMP '2026-01-01 00:00:00' + to_seconds(i * $3 + ($2 - 1) * $4),
			$2, $2 // 2, 1
		FROM range($1) t(i)`,
		activityBenchSessions, activityBenchMessages,
		activityBenchSpacingSeconds, activityBenchMessageInterval)
	require.NoError(b, err, "seed sessions")
	_, err = conn.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, ordinal, role, content, timestamp, model
		)
		SELECT s.i * $2 + o.j + 1,
			'bench-' || lpad(CAST(s.i AS VARCHAR), 6, '0'), o.j,
			CASE WHEN o.j % 2 = 0 THEN 'user' ELSE 'assistant' END,
			'message',
			TIMESTAMP '2026-01-01 00:00:00' + to_seconds(s.i * $3 + o.j * $4),
			CASE WHEN o.j % 2 = 1 THEN 'model-' || CAST(s.i % 3 AS VARCHAR) ELSE '' END
		FROM range($1) s(i), range($2) o(j)`,
		activityBenchSessions, activityBenchMessages,
		activityBenchSpacingSeconds, activityBenchMessageInterval)
	require.NoError(b, err, "seed messages")
	_, err = conn.ExecContext(ctx, `CHECKPOINT`)
	require.NoError(b, err)
	return NewStoreFromDB(conn)
}

// BenchmarkDuckDBActivityReportDay measures GetActivityReport for one UTC
// day in the middle of a 20,000-session, 2,000,000-message archive. The
// day touches a few dozen sessions, so the cost should track those
// sessions rather than the whole archive.
func BenchmarkDuckDBActivityReportDay(b *testing.B) {
	store := seedActivityReportBenchStore(b)
	now, err := time.Parse(time.RFC3339, "2030-01-01T00:00:00Z")
	require.NoError(b, err)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2026-06-01", Timezone: "UTC",
	}, now)
	require.NoError(b, err)
	filter := db.AnalyticsFilter{Timezone: "UTC"}

	b.ReportAllocs()
	for b.Loop() {
		report, err := store.GetActivityReport(b.Context(), filter, q)
		require.NoError(b, err)
		if report.Totals.Sessions == 0 || len(report.BySession) == 0 {
			b.Fatal("activity report for the benchmark day is empty")
		}
	}
}
