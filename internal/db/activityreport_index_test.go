package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActivityReportTerminalLookupIndex(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		name := "fresh"
		if upgrade {
			name = "existing archive"
		}
		t.Run(name, func(t *testing.T) {
			d := testDB(t)
			for _, status := range []string{"completed", "errored", "started"} {
				insertSession(t, d, status, "project", func(s *Session) {
					s.StartedAt = Ptr("2026-06-15T23:59:00Z")
					s.EndedAt = Ptr("2026-06-15T23:59:30Z")
				})
				seedMessage(t, d, status, 0, "assistant", "2026-06-15T23:59:15Z", "")
				timingInsertToolResultEvent(t, d, status, 0, 0,
					"call", status, "2026-06-16T00:01:00Z", 0)
			}
			if upgrade {
				_, err := d.getWriter().Exec("DROP INDEX IF EXISTS idx_tool_result_events_terminal")
				require.NoError(t, err)
				path := d.Path()
				require.NoError(t, d.Close())
				d, err = OpenIsolated(path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, d.Close()) })
			}

			// Verify the installed index supports a date seek. Pin it because
			// this tiny fixture can favor a session-only index after ANALYZE.
			rows, err := d.getReader().QueryContext(t.Context(), `EXPLAIN QUERY PLAN
				SELECT 1 FROM tool_result_events tre INDEXED BY idx_tool_result_events_terminal
				WHERE tre.session_id = ? AND tre.source = 'tool_execution'
					AND tre.status IN ('completed', 'errored')
					AND tre.timestamp IS NOT NULL AND tre.timestamp != ''
					AND agentsview_timestamp_unix_micro(tre.timestamp) IS NOT NULL
					AND tre.timestamp >= ?`, "completed", "2026-06-16T00:00:00Z")
			require.NoError(t, err)
			var details []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				details = append(details, detail)
			}
			require.NoError(t, rows.Err())
			require.NoError(t, rows.Close())
			assert.Contains(t, strings.Join(details, "\n"), "(session_id=? AND timestamp>?)")

			report, err := d.GetActivityReport(t.Context(), AnalyticsFilter{},
				dayQuery(t, "2026-06-16", "UTC"))
			require.NoError(t, err)
			var ids []string
			for _, session := range report.BySession {
				ids = append(ids, session.SessionID)
			}
			assert.ElementsMatch(t, []string{"completed", "errored"}, ids)
		})
	}
}
