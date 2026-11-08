//go:build pgtest

package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// Keep the removed SQL independent of the shared Go pairing pass.
const legacyPGActivityReportCandidatesSQL = `SELECT
				m.session_id, m.ordinal, successor.ordinal,
				m.timestamp, successor.timestamp,
				successor.role, successor.model,
				COALESCE((
					SELECT prior.model
					FROM messages prior
					WHERE prior.session_id = m.session_id
						AND prior.ordinal <= m.ordinal
						AND prior.role = 'assistant'
						AND prior.model != ''
						AND prior.timestamp IS NOT NULL
						AND prior.timestamp > (
							SELECT prior_previous.timestamp
							FROM messages prior_previous
							WHERE prior_previous.session_id = prior.session_id
								AND prior_previous.ordinal < prior.ordinal
								AND prior_previous.timestamp IS NOT NULL
							ORDER BY prior_previous.ordinal DESC
							LIMIT 1
						)
					ORDER BY prior.ordinal DESC
					LIMIT 1
				), 'unknown')
			FROM messages m
			JOIN messages successor
				ON successor.session_id = m.session_id
				AND successor.ordinal = (
					SELECT next.ordinal
					FROM messages next
					WHERE next.session_id = m.session_id
						AND next.ordinal > m.ordinal
						AND next.timestamp IS NOT NULL
					ORDER BY next.ordinal
					LIMIT 1
				)
			WHERE m.session_id = ANY($1)
				AND m.timestamp IS NOT NULL
				AND m.timestamp >= $2
				AND m.timestamp < $3
			ORDER BY m.timestamp, m.session_id, m.ordinal`

func TestPGActivityReportPairingMatchesSQLPairing(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_activity_pairing_test")
	sessions := map[string][]struct{ role, ts, model string }{
		"a-mixed": {
			{"assistant", "2026-06-15T23:40:00Z", "initial"},
			{"assistant", "2026-06-15T23:50:00Z", "old"},
			{"user", "", ""},
			{"user", "2026-06-16T00:00:00Z", ""},
			{"user", "2026-06-16T01:00:00+02:00", ""},
			{"assistant", "2026-06-16T09:30:00.123456Z", ""},
			{"user", "2026-06-16T23:59:59.999999Z", ""},
			{"assistant", "2026-06-17T00:00:00Z", "after"},
		},
		"e-boundary": {
			{"assistant", "2026-06-15T23:54:59.999999Z", "before"},
			{"user", "2026-06-15T23:55:00Z", ""},
			{"assistant", "2026-06-16T00:00:00Z", "boundary"},
		},
	}
	var ids []string
	for id, messages := range sessions {
		ids = append(ids, id)
		_, err := store.DB().ExecContext(t.Context(), `INSERT INTO sessions
   (id, machine, project, agent) VALUES ($1, 'machine', 'project', 'codex')`, id)
		require.NoError(t, err)
		for ordinal, m := range messages {
			var ts any
			if m.ts != "" {
				ts = m.ts
			}
			_, err = store.DB().ExecContext(t.Context(), `INSERT INTO messages
    (session_id, ordinal, role, content, timestamp, model)
    VALUES ($1, $2, $3, '', $4, $5)`, id, ordinal*3, m.role, ts, m.model)
			require.NoError(t, err)
		}
	}
	day := pgDayQuery(t, "2026-06-16", "UTC")
	partial := day
	partial.EffectiveEnd = time.Date(2026, 6, 16, 9, 30, 0, 123456500, time.UTC)
	for _, tc := range []struct {
		name string
		q    activity.Query
	}{
		{"day", day}, {"partial", partial},
		{"offset", pgDayQuery(t, "2026-06-16", "Asia/Tokyo")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := store.DB().QueryContext(t.Context(), legacyPGActivityReportCandidatesSQL,
				ids, tc.q.RangeStart.Add(-time.Duration(tc.q.GapCapSeconds)*time.Second), tc.q.EffectiveEnd)
			require.NoError(t, err)
			defer rows.Close()
			var want []activity.IntervalCandidate
			for rows.Next() {
				var c activity.IntervalCandidate
				require.NoError(t, rows.Scan(&c.SessionID, &c.StartOrdinal, &c.EndOrdinal,
					&c.Start, &c.End, &c.ClosingRole, &c.ClosingModel, &c.PriorModel))
				c.Start, c.End = c.Start.UTC(), c.End.UTC()
				want = append(want, c)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			var got []activity.IntervalCandidate
			require.NoError(t, store.ActivityReportCandidateSource(ids, tc.q)(t.Context(), func(c activity.IntervalCandidate) error {
				got = append(got, c)
				return nil
			}))
			require.NotEmpty(t, want)
			assert.Equal(t, want, got)
		})
	}
}

func TestPGActivityReportCandidateSourceStopsOnConsumerError(t *testing.T) {
	_, store := prepareUsageSchema(t, "agentsview_activity_pairing_stop_test")
	seedPGDailyFixture(t, store)
	q := pgDayQuery(t, "2026-06-16", "UTC")
	sentinel := errors.New("consumer stopped")
	emitted := 0
	assert.ErrorIs(t, store.ActivityReportCandidateSource([]string{"a", "b"}, q)(t.Context(), func(activity.IntervalCandidate) error {
		emitted++
		return sentinel
	}), sentinel)
	assert.Equal(t, 1, emitted, "stop emitting when the consumer rejects a candidate")
}
