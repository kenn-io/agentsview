package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// seedSubstringSearchBenchmark writes 2,000 sessions of 50 messages with
// 1-3 KB bodies of varied text and thinking text on some assistant turns.
// Every assistant body mentions "handler"; only message 7 of every 100th
// session mentions "deploy-window". A substring search has to find those 20
// rows without reading every body in the archive.
func seedSubstringSearchBenchmark(b *testing.B) *DB {
	b.Helper()
	d := testDB(b)
	ctx := b.Context()
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 1999)
		INSERT INTO sessions(id, project, agent, started_at, ended_at, created_at,
			message_count, user_message_count)
		SELECT printf('substr-%04d', i), 'project-' || (i % 20), 'claude',
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-03-01T00:00:00Z', printf('+%d minutes', i * 60)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-03-01T00:50:00Z', printf('+%d minutes', i * 60)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-03-01T00:00:00Z', printf('+%d minutes', i * 60)),
			50, 25
		FROM n`,
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 49)
		INSERT INTO messages(session_id, ordinal, role, content, thinking_text,
			timestamp, has_thinking, content_length, model)
		SELECT s.id, n.i,
			CASE WHEN n.i % 2 = 0 THEN 'user' ELSE 'assistant' END,
			CASE WHEN n.i % 2 = 1 THEN 'the handler checks ' ELSE 'please look at ' END
				|| CASE WHEN n.i = 7 AND CAST(substr(s.id, 8) AS INTEGER) % 100 = 0
					THEN 'the deploy-window settings ' ELSE '' END
				|| hex(randomblob(500 + (n.i * 397) % 1000)),
			CASE WHEN n.i % 4 = 1 THEN 'consider ' || hex(randomblob(400)) ELSE '' END,
			strftime('%Y-%m-%dT%H:%M:%SZ', s.started_at, printf('+%d minutes', n.i)),
			CASE WHEN n.i % 4 = 1 THEN 1 ELSE 0 END,
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

func BenchmarkSearchContentSubstringRareText(b *testing.B) {
	d := seedSubstringSearchBenchmark(b)
	f := ContentSearchFilter{Pattern: "deploy-window", Limit: 50}
	b.ReportAllocs()
	for b.Loop() {
		page, err := d.SearchContent(b.Context(), f)
		require.NoError(b, err)
		require.Len(b, page.Matches, 20)
	}
}

func BenchmarkSearchContentRegexRareText(b *testing.B) {
	d := seedSubstringSearchBenchmark(b)
	f := ContentSearchFilter{Pattern: `deploy-window settings [0-9A-F]+`, Mode: "regex", Limit: 50}
	b.ReportAllocs()
	for b.Loop() {
		page, err := d.SearchContent(b.Context(), f)
		require.NoError(b, err)
		require.Len(b, page.Matches, 20)
	}
}

func BenchmarkSearchContentSubstringCommonText(b *testing.B) {
	d := seedSubstringSearchBenchmark(b)
	f := ContentSearchFilter{Pattern: "handler", Limit: 50}
	b.ReportAllocs()
	for b.Loop() {
		page, err := d.SearchContent(b.Context(), f)
		require.NoError(b, err)
		require.Len(b, page.Matches, 50)
	}
}

func BenchmarkSearchContentTermsRareText(b *testing.B) {
	d := seedSubstringSearchBenchmark(b)
	f := ContentSearchFilter{Pattern: "deploy-window", Mode: "terms", Limit: 50}
	b.ReportAllocs()
	for b.Loop() {
		page, err := d.SearchContent(b.Context(), f)
		require.NoError(b, err)
		require.Len(b, page.Matches, 20)
	}
}
