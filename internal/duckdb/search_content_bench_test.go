//go:build !(windows && arm64)

package duckdb

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// Archive shape for BenchmarkDuckDBSearchContentRunExtents: many sessions,
// with the rare term in one assistant row of every runExtentsBenchHitEvery-th
// session.
const (
	runExtentsBenchSessions    = 20_000
	runExtentsBenchMessages    = 100
	runExtentsBenchHitEvery    = 1_000
	runExtentsBenchHitSessions = runExtentsBenchSessions / runExtentsBenchHitEvery
)

// seedRunExtentsBenchArchive writes the archive straight into a DuckDB file
// with set-based SQL. Each session repeats a ten-message block: a real user
// prompt at ordinal%10 == 0, user rows with system prefixes (not
// conversation-unit stops) at 3 and 6, and assistant rows everywhere else.
// Ordinal 5 of every hit session holds the rare term, so its run spans
// ordinals 1 through 9.
func seedRunExtentsBenchArchive(b *testing.B) *Store {
	b.Helper()
	ctx := b.Context()
	path := filepath.Join(b.TempDir(), "mirror.duckdb")
	conn, err := Open(ctx, path)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, conn.Close()) })
	require.NoError(b, createSchema(ctx, conn))

	_, err = conn.ExecContext(ctx, `
		INSERT INTO sessions (
			id, project, machine, agent, started_at, ended_at,
			message_count, user_message_count
		)
		SELECT 'bench-' || i, 'project-' || (i % 50), 'bench', 'claude',
			TIMESTAMP '2026-01-01 00:00:00' + to_minutes(i),
			TIMESTAMP '2026-01-01 00:00:00' + to_minutes(i + 60),
			?, ? / 10
		FROM range(?) t(i)`,
		runExtentsBenchMessages, runExtentsBenchMessages,
		runExtentsBenchSessions)
	require.NoError(b, err, "seed sessions")
	_, err = conn.ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length
		)
		SELECT s.id, n,
			CASE WHEN n % 10 IN (0, 3, 6) THEN 'user' ELSE 'assistant' END,
			c.content,
			s.started_at + to_seconds(n),
			length(c.content)
		FROM sessions s, range(?) r(n),
			LATERAL (SELECT CASE
				WHEN n % 10 = 0 THEN 'please continue with task ' || n
				WHEN n % 10 = 3 THEN '<command-name>/status</command-name>'
				WHEN n % 10 = 6 THEN '[Request interrupted by user]'
				WHEN n = 5 AND CAST(substr(s.id, 7) AS INTEGER) % ? = 0
					THEN 'assistant step with runextentshit marker'
				ELSE 'assistant step ' || n || ' of the ongoing work'
			END AS content) c`,
		runExtentsBenchMessages, runExtentsBenchHitEvery)
	require.NoError(b, err, "seed messages")
	_, err = conn.ExecContext(ctx, `CHECKPOINT`)
	require.NoError(b, err)
	info, err := os.Stat(path)
	require.NoError(b, err)
	b.Logf("fixture: %d sessions, %d messages, %d bytes on disk",
		runExtentsBenchSessions,
		runExtentsBenchSessions*runExtentsBenchMessages, info.Size())
	return NewStoreFromDB(conn)
}

// BenchmarkDuckDBSearchContentRunExtents measures a substring search whose
// rare term hits a few sessions in a large archive. The cost is dominated
// by deriving each hit's conversation-unit extent (RunExtents), whose stop
// lookups test the system-prefix predicate on user rows.
func BenchmarkDuckDBSearchContentRunExtents(b *testing.B) {
	store := seedRunExtentsBenchArchive(b)
	filter := db.ContentSearchFilter{
		Pattern: "runextentshit", Mode: "substring",
		Sources: []string{"messages"}, IncludeOneShot: true, Limit: 50,
	}
	b.ReportAllocs()
	for b.Loop() {
		page, err := store.SearchContent(b.Context(), filter)
		require.NoError(b, err)
		require.Len(b, page.Matches, runExtentsBenchHitSessions)
		for _, m := range page.Matches {
			require.Equal(b, [2]int{1, 9}, m.OrdinalRange,
				"session %s ordinal %d", m.SessionID, m.Ordinal)
		}
	}
}
