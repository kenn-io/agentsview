//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// pgToolsBenchArchive sizes a synthetic archive spread evenly over
// 2023-01-01 through 2025-12-31. Every message carries one tool call and
// every fifth call names a skill.
type pgToolsBenchArchive struct {
	name            string
	sessions        int
	callsPerSession int
}

var pgToolsBenchArchives = []pgToolsBenchArchive{
	{name: "small", sessions: 365, callsPerSession: 20},
	{name: "large", sessions: 4_380, callsPerSession: 50},
}

// seedPGToolsBenchArchive writes the archive directly with set-based SQL
// so the large fixture seeds in seconds.
func seedPGToolsBenchArchive(
	b *testing.B, archive pgToolsBenchArchive,
) *Store {
	b.Helper()
	schema := "agentsview_tools_bench_" + archive.name
	pgURL := testPGURL(b)
	pg, err := Open(pgURL, schema, true)
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
			id, machine, project, agent, started_at, ended_at,
			message_count, user_message_count
		)
		SELECT 'bench-' || i, 'bench', 'project-' || (i % 50),
			(ARRAY['claude', 'codex', 'openhands'])[i % 3 + 1],
			start_at, start_at + make_interval(mins => $2::int),
			$2::int, 2
		FROM generate_series(0, $1::int - 1) AS i,
			LATERAL (SELECT '2023-01-01T12:00:00Z'::timestamptz
				+ make_interval(secs => i * (1095 * 86400.0 / $1::int)) AS start_at) AS t`,
		archive.sessions, archive.callsPerSession)
	require.NoError(b, err, "seed sessions")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, has_tool_use, model
		)
		SELECT s.id, n, 'assistant', 'call',
			s.started_at + make_interval(mins => n), 4, TRUE,
			(ARRAY['model-a', 'model-b'])[n % 2 + 1]
		FROM sessions s, generate_series(0, $1::int - 1) AS n`,
		archive.callsPerSession)
	require.NoError(b, err, "seed messages")
	_, err = pg.ExecContext(ctx, `
		INSERT INTO tool_calls (
			session_id, tool_name, category, call_index,
			message_ordinal, skill_name
		)
		SELECT m.session_id, tool, tool, 0, m.ordinal,
			CASE WHEN m.ordinal % 5 = 0
				THEN 'skill-' || (m.ordinal % 7) END
		FROM messages m,
			LATERAL (SELECT (ARRAY['Read', 'Edit', 'Bash', 'Grep',
				'Glob', 'Write', 'Task', 'WebFetch'])[m.ordinal % 8 + 1]
				AS tool) AS t`)
	require.NoError(b, err, "seed tool_calls")
	_, err = pg.ExecContext(ctx, `ANALYZE sessions, messages, tool_calls`)
	require.NoError(b, err)

	store, err := NewStore(pgURL, schema, true)
	require.NoError(b, err)
	b.Cleanup(func() { _ = store.Close() })
	return store
}

// BenchmarkPGAnalyticsToolsSkills measures the tools and skills panels
// over a year-long New York window, the same window narrowed to one of
// fifty projects, and the full archive history.
func BenchmarkPGAnalyticsToolsSkills(b *testing.B) {
	year := db.AnalyticsFilter{
		From: "2025-01-01", To: "2025-12-31",
		Timezone:       "America/New_York",
		ExcludeOneShot: true, ExcludeAutomated: true,
	}
	sparse := year
	sparse.Project = "project-7"
	history := year
	history.From = "2023-01-01"
	filters := []struct {
		name   string
		filter db.AnalyticsFilter
	}{
		{"year", year},
		{"sparse-year", sparse},
		{"history", history},
	}
	for _, archive := range pgToolsBenchArchives {
		b.Run("archive="+archive.name, func(b *testing.B) {
			store := seedPGToolsBenchArchive(b, archive)
			for _, fc := range filters {
				b.Run("panel=tools/filter="+fc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						resp, err := store.GetAnalyticsTools(b.Context(), fc.filter)
						require.NoError(b, err)
						require.Positive(b, resp.TotalCalls)
					}
				})
				b.Run("panel=skills/filter="+fc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						resp, err := store.GetAnalyticsSkills(
							b.Context(), fc.filter, "week")
						require.NoError(b, err)
						require.Positive(b, resp.TotalSkillCalls)
					}
				})
			}
		})
	}
}
