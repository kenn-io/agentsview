package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkSearchContentTermsCommonAndListedWord searches terms mode for two
// words in 2,000 sessions of 30 exchanges with 1-3 KB messages. "handler"
// appears in every assistant message, so the substring index skips it;
// "deploy-window" appears in one user message of 800 sessions, which the
// index lists. Every matching session must still be grouped into exchanges.
func BenchmarkSearchContentTermsCommonAndListedWord(b *testing.B) {
	d := testDB(b)
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 1999)
		INSERT INTO sessions(id, project, agent, started_at, ended_at, created_at,
			message_count, user_message_count)
		SELECT printf('anchor-%04d', i), 'project-' || (i % 20), 'claude',
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-01-01T00:00:00Z', printf('+%d minutes', i * 240)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-01-01T01:00:00Z', printf('+%d minutes', i * 240)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-01-01T00:00:00Z', printf('+%d minutes', i * 240)),
			60, 30
		FROM n`,
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 59)
		INSERT INTO messages(session_id, ordinal, role, content, content_length, timestamp)
		SELECT s.id, n.i,
			CASE WHEN n.i % 2 = 0 THEN 'user' ELSE 'assistant' END,
			CASE
				WHEN n.i % 2 = 1 THEN 'the handler replied '
				WHEN n.i = 10 AND CAST(substr(s.id, 8) AS INTEGER) % 5 < 2 THEN 'open the deploy-window now '
				ELSE 'turn text '
			END || hex(randomblob(500 + (n.i * 397) % 1000)),
			1000 + (n.i * 397) % 2000,
			strftime('%Y-%m-%dT%H:%M:%SZ', s.started_at, printf('+%d seconds', n.i * 30))
		FROM sessions s CROSS JOIN n`,
		`ANALYZE`,
	} {
		_, err := d.getWriter().Exec(b.Context(), stmt)
		require.NoError(b, err)
	}
	f := ContentSearchFilter{Pattern: "handler deploy-window", Mode: "terms", Limit: 50}
	b.ReportAllocs()
	for b.Loop() {
		page, err := d.SearchContent(b.Context(), f)
		require.NoError(b, err)
		require.Len(b, page.Matches, 50)
	}
}
