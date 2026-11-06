//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// BenchmarkDuckDBTrendsTerms uses the archive shape of
// BenchmarkSQLiteTrendsTerms: 4,380 sessions every six hours from 2023-01-01
// through 2025-12-31 over twenty projects, each with 40 messages of about
// 400 bytes. It measures a month inside the archive, the default one-year
// range narrowed to one project, and the whole archive.
func BenchmarkDuckDBTrendsTerms(b *testing.B) {
	ctx := b.Context()
	conn, err := Open(ctx, filepath.Join(b.TempDir(), "trends.duckdb"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, conn.Close()) })
	require.NoError(b, createSchema(ctx, conn))
	_, err = conn.ExecContext(ctx, `
		INSERT INTO sessions (
			id, project, machine, agent, started_at, ended_at, created_at,
			message_count, user_message_count
		)
		SELECT 'bench-' || CAST(i AS VARCHAR), 'project-' || CAST(i % 20 AS VARCHAR),
			'bench', 'claude', st, st + INTERVAL 20 MINUTE, st, 40, 20
		FROM (
			SELECT i, TIMESTAMP '2023-01-01 12:00:00' + to_hours(CAST(i * 6 AS BIGINT)) AS st
			FROM range(4380) AS t(i)
		)`)
	require.NoError(b, err, "seed sessions")
	_, err = conn.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, ordinal, role, content, timestamp, content_length
		)
		SELECT row_number() OVER (), session_id, n,
			CASE WHEN n % 2 = 0 THEN 'user' ELSE 'assistant' END,
			content, ts, length(content)
		FROM (
			SELECT s.id AS session_id, CAST(r.n AS INTEGER) AS n,
				s.started_at + to_seconds(CAST(r.n * 30 AS BIGINT)) AS ts,
				CASE WHEN r.n % 4 = 0 THEN 'seam ' ELSE '' END
					|| repeat('lorem ipsum ', 32) || s.id || '-' || CAST(r.n AS VARCHAR) AS content
			FROM sessions s, range(40) AS r(n)
		)`)
	require.NoError(b, err, "seed messages")

	store := NewStoreFromDB(conn)
	terms, err := db.ParseTrendTerms([]string{"seam"})
	require.NoError(b, err)
	for _, fc := range []struct {
		name   string
		filter db.AnalyticsFilter
	}{
		{"range=30d", db.AnalyticsFilter{From: "2025-06-01", To: "2025-06-30", Timezone: "America/New_York"}},
		{"project=year", db.AnalyticsFilter{From: "2025-01-01", To: "2025-12-31", Timezone: "America/New_York", Project: "project-7"}},
		{"range=full", db.AnalyticsFilter{From: "2023-01-01", To: "2025-12-31", Timezone: "America/New_York"}},
	} {
		b.Run(fc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				resp, err := store.GetTrendsTerms(b.Context(), fc.filter, terms, "week")
				require.NoError(b, err)
				require.Positive(b, resp.MessageCount)
				require.Positive(b, resp.Series[0].Total)
			}
		})
	}
}
