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

// seedToolOutputSearchBenchmark writes 2,000 sessions of 100 messages, each
// assistant turn running a tool whose 0.5-1 KB result has no index. The 25
// newest sessions each hold "deploy window" once in a tool result, so a page
// of 20 matches comes from them alone.
func seedToolOutputSearchBenchmark(b *testing.B) *DB {
	b.Helper()
	d := testDB(b)
	ctx := b.Context()
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 1999)
		INSERT INTO sessions(id, project, agent, started_at, ended_at, created_at,
			message_count, user_message_count)
		SELECT printf('tools-%04d', i), 'project-' || (i % 20), 'claude',
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-03-01T00:00:00Z', printf('+%d minutes', i * 60)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-03-01T00:50:00Z', printf('+%d minutes', i * 60)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-03-01T00:00:00Z', printf('+%d minutes', i * 60)),
			100, 50
		FROM n`,
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 99)
		INSERT INTO messages(session_id, ordinal, role, content, timestamp, content_length)
		SELECT s.id, n.i,
			CASE WHEN n.i % 2 = 0 THEN 'user' ELSE 'assistant' END,
			CASE WHEN n.i % 2 = 0 THEN 'please run the next step' ELSE 'running it' END,
			strftime('%Y-%m-%dT%H:%M:%SZ', s.started_at, printf('+%d minutes', n.i)),
			20
		FROM sessions s CROSS JOIN n`,
		`INSERT INTO tool_calls(message_id, session_id, tool_name, category, tool_use_id,
			input_json, result_content, result_content_length)
		SELECT m.id, m.session_id, 'Bash', 'Bash', 'tu-' || m.id,
			'{"command":"make step-' || m.ordinal || '"}',
			CASE WHEN m.ordinal = 7 AND CAST(substr(m.session_id, 7) AS INTEGER) >= 1975
				THEN 'the deploy window opens ' ELSE '' END
				|| hex(randomblob(250 + (m.id * 397) % 250)),
			750
		FROM messages m WHERE m.role = 'assistant'`,
		`ANALYZE`,
	} {
		_, err := d.getWriter().Exec(ctx, stmt)
		require.NoError(b, err)
	}
	return d
}

// BenchmarkSearchContentToolOutputRecentMatches searches every source for
// text the newest sessions' tool output holds. The page fills from those
// sessions, so the search need not read the rest of the archive's tool output.
func BenchmarkSearchContentToolOutputRecentMatches(b *testing.B) {
	d := seedToolOutputSearchBenchmark(b)
	for _, bc := range []struct{ name, pattern, mode string }{
		{"words", "deploy window", "substring"},
		{"quoted", `"deploy window"`, "substring"},
		{"regex", `deploy\s+window`, "regex"},
	} {
		b.Run(bc.name, func(b *testing.B) {
			f := ContentSearchFilter{Pattern: bc.pattern, Mode: bc.mode, Limit: 20}
			b.ReportAllocs()
			for b.Loop() {
				page, err := d.SearchContent(b.Context(), f)
				require.NoError(b, err)
				require.Len(b, page.Matches, 20)
				require.Equal(b, 20, page.NextCursor)
			}
		})
	}
}
