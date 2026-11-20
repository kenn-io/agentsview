package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// seedSearchRankBenchmark writes 2,000 sessions of 50 messages with 1-3 KB
// bodies of varying word counts, so their ranks differ as transcript ranks
// do. Every assistant message mentions "handler", and every user message
// and session title mentions "refactor", so a search matches 50,000 messages
// across all sessions and has to rank them to fill one page of sessions.
func seedSearchRankBenchmark(b *testing.B) *DB {
	b.Helper()
	d := testDB(b)
	ctx := b.Context()
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 1999)
		INSERT INTO sessions(id, project, agent, first_message, started_at, ended_at,
			created_at, message_count, user_message_count)
		SELECT printf('rank-%04d', i), 'project-' || (i % 20), 'claude',
			'please refactor module ' || i,
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-03-01T00:00:00Z', printf('+%d minutes', i * 60)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-03-01T00:50:00Z', printf('+%d minutes', i * 60)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-03-01T00:00:00Z', printf('+%d minutes', i * 60)),
			50, 25
		FROM n`,
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 49)
		INSERT INTO messages(session_id, ordinal, role, content, timestamp, content_length, model)
		SELECT s.id, n.i,
			CASE WHEN n.i % 2 = 0 THEN 'user' ELSE 'assistant' END,
			CASE WHEN n.i % 2 = 1 THEN 'the handler checks ' ELSE 'please refactor ' END
				|| replace(hex(randomblob(500 + (n.i * 397 + CAST(substr(s.id, 6) AS INTEGER) * 7919) % 1000)), '0', ' '),
			strftime('%Y-%m-%dT%H:%M:%SZ', s.started_at, printf('+%d minutes', n.i)),
			1000 + (n.i * 397) % 2000,
			CASE WHEN n.i % 2 = 1 THEN 'claude-sonnet-4-5' ELSE '' END
		FROM sessions s CROSS JOIN n`,
		`ANALYZE`,
	} {
		_, err := d.getWriter().Exec(ctx, stmt)
		require.NoError(b, err)
	}
	return d
}

func BenchmarkSearchCommonContentWord(b *testing.B) {
	d := seedSearchRankBenchmark(b)
	f := SearchFilter{Query: "handler", Limit: 25}
	b.ReportAllocs()
	for b.Loop() {
		page, err := d.Search(b.Context(), f)
		require.NoError(b, err)
		require.Len(b, page.Results, 25)
	}
}

func BenchmarkSearchCommonContentAndTitleWord(b *testing.B) {
	d := seedSearchRankBenchmark(b)
	f := SearchFilter{Query: "refactor", Limit: 25}
	b.ReportAllocs()
	for b.Loop() {
		page, err := d.Search(b.Context(), f)
		require.NoError(b, err)
		require.Len(b, page.Results, 25)
	}
}
