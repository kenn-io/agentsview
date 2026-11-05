//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// BenchmarkPGTrendsTerms uses the archive shape of BenchmarkSQLiteTrendsTerms:
// 4,380 sessions every six hours from 2023-01-01 through 2025-12-31 over
// twenty projects, each with 40 messages of about 400 bytes. It measures a
// month inside the archive, the default one-year range narrowed to one
// project, and the whole archive.
func BenchmarkPGTrendsTerms(b *testing.B) {
	const schema = "agentsview_trends_bench"
	pgURL := testPGURL(b)
	pg, err := Open(b.Context(), pgURL, schema, true)
	require.NoError(b, err)
	b.Cleanup(func() { _ = pg.Close() })
	ctx := b.Context()
	_, err = pg.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	require.NoError(b, err)
	require.NoError(b, EnsureSchema(ctx, pg, schema))
	b.Cleanup(func() {
		_, _ = pg.ExecContext(context.WithoutCancel(ctx),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})
	_, err = pg.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent, started_at, ended_at, created_at,
			message_count, user_message_count
		)
		SELECT 'bench-' || i, 'bench', 'project-' || (i % 20), 'claude',
			t.st, t.st + interval '20 minutes', t.st, 40, 20
		FROM generate_series(0, 4379) AS i,
			LATERAL (SELECT '2023-01-01T12:00:00Z'::timestamptz
				+ make_interval(hours => i * 6) AS st) AS t`)
	require.NoError(b, err, "seed sessions")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp, content_length
		)
		SELECT s.id, n,
			CASE WHEN n % 2 = 0 THEN 'user' ELSE 'assistant' END,
			c.content, s.started_at + make_interval(secs => n * 30),
			length(c.content)
		FROM sessions s, generate_series(0, 39) AS n,
			LATERAL (SELECT CASE WHEN n % 4 = 0 THEN 'seam ' ELSE '' END
				|| repeat('lorem ipsum ', 32) || s.id || '-' || n AS content) AS c`)
	require.NoError(b, err, "seed messages")
	_, err = pg.ExecContext(ctx, `ANALYZE sessions, messages`)
	require.NoError(b, err)

	store, err := NewStore(b.Context(), pgURL, schema, true)
	require.NoError(b, err)
	b.Cleanup(func() { _ = store.Close() })
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
