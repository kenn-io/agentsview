package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkSQLiteTrendsTerms seeds 4,380 sessions every six hours from
// 2023-01-01 through 2025-12-31 over twenty projects, each with 40
// alternating user and assistant messages of about 400 bytes; every fourth
// message mentions the term. It measures a month inside the archive, the
// default one-year range narrowed to one project, and the whole archive.
// The PostgreSQL, DuckDB and ClickHouse trends benchmarks use the same shape.
func BenchmarkSQLiteTrendsTerms(b *testing.B) {
	d := testDB(b)
	ctx := b.Context()
	require.NoError(b, d.Update(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			WITH RECURSIVE seq(i) AS (
				SELECT 0 UNION ALL SELECT i + 1 FROM seq WHERE i < ? - 1
			)
			INSERT INTO sessions (
				id, project, machine, agent, started_at, ended_at, created_at,
				message_count, user_message_count
			)
			SELECT 'bench-' || i, 'project-' || (i % 20), 'bench', 'claude',
				strftime('%Y-%m-%dT%H:%M:%SZ', '2023-01-01T12:00:00Z', '+' || (i * 6) || ' hours'),
				strftime('%Y-%m-%dT%H:%M:%SZ', '2023-01-01T12:20:00Z', '+' || (i * 6) || ' hours'),
				strftime('%Y-%m-%dT%H:%M:%SZ', '2023-01-01T12:00:00Z', '+' || (i * 6) || ' hours'),
				?, ? / 2
			FROM seq`,
			4380, 40, 40,
		); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			WITH RECURSIVE seq(n) AS (
				SELECT 0 UNION ALL SELECT n + 1 FROM seq WHERE n < ? - 1
			),
			msg AS (
				SELECT s.id AS session_id, seq.n AS n, s.started_at AS started_at,
					CASE WHEN seq.n % 4 = 0 THEN 'seam ' ELSE '' END
						|| replace(hex(zeroblob(32)), '00', 'lorem ipsum ')
						|| s.id || '-' || seq.n AS content
				FROM sessions s, seq
			)
			INSERT INTO messages (
				session_id, ordinal, role, content, timestamp, content_length
			)
			SELECT session_id, n,
				CASE WHEN n % 2 = 0 THEN 'user' ELSE 'assistant' END,
				content,
				strftime('%Y-%m-%dT%H:%M:%SZ', started_at, '+' || (n * 30) || ' seconds'),
				length(content)
			FROM msg`,
			40,
		)
		return err
	}))
	require.NoError(b, d.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `ANALYZE`)
		return err
	}))
	terms, err := ParseTrendTerms([]string{"seam"})
	require.NoError(b, err)
	for _, fc := range []struct {
		name   string
		filter AnalyticsFilter
	}{
		{"range=30d", AnalyticsFilter{From: "2025-06-01", To: "2025-06-30", Timezone: "America/New_York"}},
		{"project=year", AnalyticsFilter{From: "2025-01-01", To: "2025-12-31", Timezone: "America/New_York", Project: "project-7"}},
		{"range=full", AnalyticsFilter{From: "2023-01-01", To: "2025-12-31", Timezone: "America/New_York"}},
	} {
		b.Run(fc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				resp, err := d.GetTrendsTerms(b.Context(), fc.filter, terms, "week")
				require.NoError(b, err)
				require.Positive(b, resp.MessageCount)
				require.Positive(b, resp.Series[0].Total)
			}
		})
	}
}
