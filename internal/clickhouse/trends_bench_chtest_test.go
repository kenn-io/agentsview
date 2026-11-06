//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
)

// BenchmarkClickHouseTrendsTerms uses the archive shape of
// BenchmarkSQLiteTrendsTerms: 4,380 sessions every six hours from 2023-01-01
// through 2025-12-31 over twenty projects, each with 40 messages of about
// 400 bytes. It measures a month inside the archive, the default one-year
// range narrowed to one project, and the whole archive.
func BenchmarkClickHouseTrendsTerms(b *testing.B) {
	ctx := b.Context()
	dsn, database := chtest.FreshDatabase(b)
	target := Target{URL: dsn, Database: database}
	require.NoError(b, EnsureSchema(ctx, target))
	store, err := NewStore(ctx, target)
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO sessions (
			id, project, machine, agent, started_at, ended_at, created_at,
			message_count, user_message_count, push_version, data_version
		)
		SELECT concat('bench-', toString(i)), concat('project-', toString(i % 20)),
			'bench', 'claude', st, st + toIntervalMinute(20), st, 40, 20, 1, 1
		FROM (
			SELECT number AS i,
				toDateTime64('2023-01-01 12:00:00', 6, 'UTC') + toIntervalHour(number * 6) AS st
			FROM numbers(4380)
		)`)
	require.NoError(b, err, "seed sessions")
	_, err = store.DB().ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, ordinal, role, content, timestamp,
			content_length, push_version
		)
		SELECT toInt64(number), concat('bench-', toString(s)), toInt64(n),
			if(n % 2 = 0, 'user', 'assistant'), content,
			toDateTime64('2023-01-01 12:00:00', 6, 'UTC')
				+ toIntervalHour(s * 6) + toIntervalSecond(n * 30),
			length(content), 1
		FROM (
			SELECT number, intDiv(number, 40) AS s, number % 40 AS n,
				concat(if(n % 4 = 0, 'seam ', ''), repeat('lorem ipsum ', 32),
					'bench-', toString(s), '-', toString(n)) AS content
			FROM numbers(4380 * 40)
		)`)
	require.NoError(b, err, "seed messages")

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
