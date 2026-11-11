package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// BenchmarkSQLiteActivityReportUsage reads a month of Activity usage: 2,000
// sessions of 100 priced assistant messages, each request stored as two
// Claude snapshots, and the first requests of every session repeated in the
// session before it. "cache" is the report path on a filled usage cache;
// "raw" re-reads and re-parses the archive rows, as reports did before.
func BenchmarkSQLiteActivityReportUsage(b *testing.B) {
	d := testDB(b)
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 1999)
		INSERT INTO sessions(id, project, agent, started_at, ended_at, created_at, message_count)
		SELECT printf('use-%04d', i), 'project-' || (i % 20), 'claude',
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-07-01T00:00:00Z', printf('+%d minutes', i * 20)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-07-01T01:40:00Z', printf('+%d minutes', i * 20)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-07-01T00:00:00Z', printf('+%d minutes', i * 20)),
			100
		FROM n`,
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 99)
		INSERT INTO messages(session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id)
		SELECT s.id, n.i, 'assistant', 'x',
			strftime('%Y-%m-%dT%H:%M:%fZ', s.started_at, printf('+%d seconds', n.i * 60)),
			1, 'claude-sonnet-4-5',
			printf('{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d}',
				100 + n.i, 10 * (n.i % 2 + 1), 1000 * n.i),
			printf('msg-%s-%d', CASE WHEN n.i < 4 AND s.id != 'use-1999'
				THEN printf('use-%04d', CAST(substr(s.id, 5) AS INTEGER) + 1) ELSE s.id END, n.i / 2),
			printf('req-%d', n.i / 2)
		FROM sessions s CROSS JOIN n`,
		`ANALYZE`,
	} {
		_, err := d.getWriter().Exec(b.Context(), stmt)
		require.NoError(b, err)
	}
	ids := make([]string, 2_000)
	for i := range ids {
		ids[i] = fmt.Sprintf("use-%04d", i)
	}
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "month", Date: "2026-07-01", Timezone: "UTC",
	}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(b, err)
	lower := PaddedUTCBound(q.RangeStart.UTC().Format(time.RFC3339), -14)
	upper := PaddedUTCBound(q.RangeEnd.UTC().Format(time.RFC3339), 14)
	// Every session holds 50 requests, but the first session has no copy of
	// its own first two: it repeats the second session's.
	const survivors = 2_000*50 - 2
	b.Run("cache", func(b *testing.B) {
		_, err := d.prepareActivityUsageWindow(b.Context(), lower, upper)
		require.NoError(b, err)
		b.ReportAllocs()
		for b.Loop() {
			window, err := d.prepareActivityUsageWindow(b.Context(), lower, upper)
			require.NoError(b, err)
			rows, _, _, err := d.activityReportCachedUsage(b.Context(), window, ids, lower, upper, q, true)
			require.NoError(b, err)
			require.Len(b, rows, survivors)
		}
	})
	b.Run("raw", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			rows, _, _, err := d.activityReportUsage(b.Context(), ids, lower, upper, q, true)
			require.NoError(b, err)
			require.Len(b, rows, survivors)
		}
	})
}
