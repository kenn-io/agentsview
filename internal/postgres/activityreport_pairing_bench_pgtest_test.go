//go:build pgtest

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

func BenchmarkPGActivityReportPairing(b *testing.B) {
	fixture := openPGUsageBenchmarkFixture(b)
	store := fixture.remote
	_, err := store.DB().ExecContext(b.Context(), `INSERT INTO sessions
  (id, machine, project, agent, started_at, ended_at)
  SELECT 'pairing-' || s, 'machine', 'project', 'codex',
   '2026-01-01'::timestamptz + (s % 365) * interval '1 day',
   '2026-01-01'::timestamptz + (s % 365) * interval '1 day' + interval '1000 seconds'
  FROM generate_series(0, 999) s;
  INSERT INTO messages (session_id, ordinal, role, content, timestamp, model)
  SELECT s.id, n, CASE WHEN n % 3 = 0 THEN 'user' ELSE 'assistant' END,
   repeat('A representative transcript body. ', 32),
   s.started_at + n * interval '1 second', 'model-priced'
  FROM sessions s CROSS JOIN generate_series(0, 999) n;
  INSERT INTO sessions (id, machine, project, agent, started_at, ended_at)
  VALUES ('long-session', 'machine', 'project', 'codex', '2026-06-15', '2026-06-17');
  INSERT INTO messages (session_id, ordinal, role, content, timestamp, model)
  SELECT 'long-session', n, 'assistant', repeat('A representative transcript body. ', 32),
   '2026-06-15'::timestamptz + n * interval '1 second', 'model-priced'
  FROM generate_series(0, 99999) n;
  ANALYZE sessions, messages`)
	require.NoError(b, err)
	for _, tc := range []struct{ name, preset string }{
		{"day", "day"}, {"week", "week"}, {"month", "month"}, {"year", "custom"}, {"long-session-narrow", "day"},
	} {
		input := activity.QueryInput{Preset: tc.preset, Date: "2026-06-16", Timezone: "UTC"}
		if tc.name == "year" {
			input.From, input.To = "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z"
		}
		q, err := activity.ResolveQuery(input, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
		require.NoError(b, err)
		rows, err := store.DB().QueryContext(b.Context(), `SELECT id FROM sessions
 WHERE started_at < $2 AND ended_at >= $1 ORDER BY id`, q.RangeStart, q.EffectiveEnd)
		require.NoError(b, err)
		var ids []string
		for rows.Next() {
			var id string
			require.NoError(b, rows.Scan(&id))
			ids = append(ids, id)
		}
		require.NoError(b, rows.Err())
		require.NoError(b, rows.Close())
		if tc.name == "long-session-narrow" {
			ids = []string{"long-session"}
			q.EffectiveEnd = q.RangeStart.Add(time.Minute)
		}
		b.Run(tc.name, func(b *testing.B) {
			for _, legacy := range []bool{true, false} {
				name := "go"
				if legacy {
					name = "sql"
				}
				b.Run(name, func(b *testing.B) {
					source := store.ActivityReportCandidateSource(ids, q)
					if legacy {
						source = func(ctx context.Context, yield func(activity.IntervalCandidate) error) error {
							rows, err := store.DB().QueryContext(ctx, legacyPGActivityReportCandidatesSQL,
								ids, q.RangeStart.Add(-time.Duration(q.GapCapSeconds)*time.Second), q.EffectiveEnd)
							if err != nil {
								return err
							}
							defer rows.Close()
							for rows.Next() {
								var c activity.IntervalCandidate
								if err := rows.Scan(&c.SessionID, &c.StartOrdinal, &c.EndOrdinal, &c.Start, &c.End,
									&c.ClosingRole, &c.ClosingModel, &c.PriorModel); err != nil {
									return err
								}
								if err := yield(c); err != nil {
									return err
								}
							}
							return rows.Err()
						}
					}
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						require.NoError(b, source(b.Context(), func(activity.IntervalCandidate) error { return nil }))
					}
				})
			}
		})
	}
}
