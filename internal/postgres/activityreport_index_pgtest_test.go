//go:build pgtest

package postgres

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestPGActivityReportTerminalLookupIndex(t *testing.T) {
	const schema = "agentsview_activity_index_test"
	_, store := prepareUsageSchema(t, schema)
	for _, status := range []string{"completed", "errored", "started"} {
		_, err := store.DB().ExecContext(t.Context(), `
			INSERT INTO sessions (id, machine, project, agent, started_at, ended_at,
				message_count, user_message_count)
			VALUES ($1, 'machine', 'project', 'grok',
				'2026-06-15T23:59:00Z', '2026-06-15T23:59:30Z', 1, 1)`, status)
		require.NoError(t, err)
		_, err = store.DB().ExecContext(t.Context(), `
			INSERT INTO messages (session_id, ordinal, role, content, timestamp)
			VALUES ($1, 0, 'assistant', '', '2026-06-15T23:59:15Z')`, status)
		require.NoError(t, err)
		_, err = store.DB().ExecContext(t.Context(), `
			INSERT INTO tool_result_events (session_id, tool_call_message_ordinal,
				source, status, content, timestamp)
			VALUES ($1, 0, 'tool_execution', $1, '', '2026-06-16T00:01:00Z')`, status)
		require.NoError(t, err)
	}
	for _, upgrade := range []bool{false, true} {
		name := "fresh"
		if upgrade {
			name = "existing archive"
		}
		t.Run(name, func(t *testing.T) {
			if upgrade {
				_, err := store.DB().ExecContext(t.Context(),
					"DROP INDEX IF EXISTS idx_tool_result_events_terminal")
				require.NoError(t, err)
				syncer := &Sync{pg: store.DB(), schema: schema}
				require.NoError(t, syncer.EnsureSchema(t.Context()))
			}
			tx, err := store.DB().BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer tx.Rollback()
			// On this tiny fixture a sequential scan is cheaper. Inspect the
			// available index path that a large archive needs instead.
			_, err = tx.ExecContext(t.Context(), "SET LOCAL enable_seqscan = off; SET LOCAL enable_bitmapscan = off")
			require.NoError(t, err)
			var raw []byte
			require.NoError(t, tx.QueryRowContext(t.Context(), `EXPLAIN (FORMAT JSON)
				SELECT 1 FROM tool_result_events tre
				WHERE tre.session_id = $1 AND tre.source = 'tool_execution'
					AND tre.status IN ('completed', 'errored')
					AND tre.timestamp >= $2::timestamptz`,
				"completed", "2026-06-16T00:00:00Z").Scan(&raw))
			var plan []struct {
				Plan struct {
					IndexCond string `json:"Index Cond"`
				}
			}
			require.NoError(t, json.Unmarshal(raw, &plan))
			require.Len(t, plan, 1)
			assert.Contains(t, plan[0].Plan.IndexCond, "session_id =")
			assert.Contains(t, plan[0].Plan.IndexCond, `"timestamp" >=`)
			require.NoError(t, tx.Rollback())

			report, err := store.GetActivityReport(t.Context(), db.AnalyticsFilter{},
				pgDayQuery(t, "2026-06-16", "UTC"))
			require.NoError(t, err)
			var ids []string
			for _, session := range report.BySession {
				ids = append(ids, session.SessionID)
			}
			assert.ElementsMatch(t, []string{"completed", "errored"}, ids)
		})
	}
}
