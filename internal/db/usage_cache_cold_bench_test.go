package db

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkUsageColdAllHistory reads all-history daily usage from an empty
// usage cache, as the first Usage page after a cache rebuild does. The
// archive holds 3,000 sessions of 60 messages; every assistant message
// reports token usage, Claude splits each response across two rows that
// share message and request IDs, and every fifth session repeats the IDs of
// the session before it, as resumed sessions do.
func BenchmarkUsageColdAllHistory(b *testing.B) {
	database := testDB(b)
	ctx := b.Context()
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 2999)
		INSERT INTO sessions(id, project, agent, started_at, ended_at, created_at,
			message_count, user_message_count)
		SELECT 'cold-' || i, 'project-' || (i % 20), 'claude',
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-01-01T00:00:00Z', printf('+%d minutes', i * 120)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-01-01T01:00:00Z', printf('+%d minutes', i * 120)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-01-01T00:00:00Z', printf('+%d minutes', i * 120)),
			60, 30
		FROM n`,
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 59),
		sessions_n AS (
			SELECT id, started_at, CAST(substr(id, 6) AS INTEGER) AS k FROM sessions
		)
		INSERT INTO messages(session_id, ordinal, role, content, content_length, timestamp,
			model, token_usage, claude_message_id, claude_request_id)
		SELECT s.id, n.i,
			CASE WHEN n.i % 2 = 0 THEN 'user' ELSE 'assistant' END,
			'turn text', 9,
			strftime('%Y-%m-%dT%H:%M:%SZ', s.started_at, printf('+%d seconds', n.i * 30)),
			CASE WHEN n.i % 2 = 1 THEN 'claude-sonnet-4-5' ELSE '' END,
			CASE WHEN n.i % 2 = 1 THEN printf(
				'{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":2000,"cache_creation_input_tokens":300}',
				100 + n.i, 40 + n.i) ELSE '' END,
			CASE WHEN n.i % 2 = 1 THEN printf('msg_%d_%d',
				CASE WHEN s.k % 5 = 0 AND s.k > 0 THEN s.k - 1 ELSE s.k END, n.i / 4) ELSE '' END,
			CASE WHEN n.i % 2 = 1 THEN printf('req_%d_%d',
				CASE WHEN s.k % 5 = 0 AND s.k > 0 THEN s.k - 1 ELSE s.k END, n.i / 4) ELSE '' END
		FROM sessions_n s CROSS JOIN n`,
	} {
		_, err := database.getWriter().Exec(ctx, stmt)
		require.NoError(b, err)
	}
	filter := UsageFilter{Timezone: "UTC", Breakdowns: true}
	dir := b.TempDir()
	b.ReportAllocs()
	var run int
	for b.Loop() {
		b.StopTimer()
		run++
		runDir := filepath.Join(dir, strconv.Itoa(run))
		require.NoError(b, os.Mkdir(runDir, 0o700))
		require.NoError(b, database.usageCache.Close())
		database.usageCache = newUsageCacheManager(filepath.Join(runDir, "sessions.db"))
		database.usageCache.attachArchive(database)
		b.StartTimer()
		result, err := database.GetDailyUsage(ctx, filter)
		require.NoError(b, err)
		require.NotEmpty(b, result.Daily)
	}
}
