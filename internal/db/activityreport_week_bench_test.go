package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// BenchmarkSQLiteActivityReportBusyWeekFirstOpen builds a week holding 6,000
// sessions of 50 alternating user and Claude assistant messages, with an empty
// pairing memo, as the first Activity open after a start does. Each response
// is stored as two rows that share its message and request IDs, as Claude
// transcripts store multi-part responses. The week exceeds the pairing memo's
// budgets, so it measures a build the memo cannot hold.
func BenchmarkSQLiteActivityReportBusyWeekFirstOpen(b *testing.B) {
	d := testDB(b)
	const sessionCount = 6_000
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 5999)
		INSERT INTO sessions(id, project, agent, started_at, ended_at, created_at, message_count)
		SELECT printf('week-%04d', i), 'project-' || (i % 20), 'claude',
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-07-06T00:00:00Z', printf('+%d seconds', i * 100)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-07-06T00:49:00Z', printf('+%d seconds', i * 100)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-07-06T00:00:00Z', printf('+%d seconds', i * 100)),
			50
		FROM n`,
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 49)
		INSERT INTO messages(session_id, ordinal, role, content, timestamp, content_length,
			model, token_usage, claude_message_id, claude_request_id)
		SELECT s.id, n.i,
			CASE WHEN n.i % 2 = 0 THEN 'user' ELSE 'assistant' END,
			'message',
			strftime('%Y-%m-%dT%H:%M:%fZ', s.started_at, printf('+%d.%03d seconds', n.i * 60, n.i * 37 % 1000)),
			7,
			CASE WHEN n.i % 2 = 1 THEN 'claude-sonnet-4-5' ELSE '' END,
			CASE WHEN n.i % 2 = 1 THEN printf(
				'{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":2000,"cache_creation_input_tokens":300}',
				100 + n.i, 40 + n.i) ELSE '' END,
			CASE WHEN n.i % 2 = 1 THEN printf('msg_%s_%d', s.id, n.i / 4) ELSE '' END,
			CASE WHEN n.i % 2 = 1 THEN printf('req_%s_%d', s.id, n.i / 4) ELSE '' END
		FROM sessions s CROSS JOIN n`,
		`ANALYZE`,
	} {
		_, err := d.getWriter().Exec(b.Context(), stmt)
		require.NoError(b, err)
	}
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "week", Date: "2026-07-08", Timezone: "UTC",
	}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(b, err)
	b.ReportAllocs()
	for b.Loop() {
		d.activityMemo.swap(func() {})
		artifacts, err := d.BuildActivityReportArtifacts(b.Context(), AnalyticsFilter{Timezone: "UTC"}, q, nil)
		require.NoError(b, err)
		require.Len(b, artifacts.Sessions, sessionCount)
	}
}
