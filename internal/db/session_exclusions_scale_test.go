package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionExclusionCountsSelectiveProject(t *testing.T) {
	d := testDB(t)
	_, err := d.getWriter().ExecContext(t.Context(), `
		WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < 92001)
		INSERT INTO sessions (id, project, machine, agent, message_count, user_message_count)
		SELECT printf('scale-%06d', n), CASE WHEN n = 1 THEN 'selected' ELSE 'unrelated' END, 'local', 'claude', 4, 3 FROM seq`)
	require.NoError(t, err)
	f := SessionFilter{Project: "selected", ExcludeOneShot: true, ExcludeAutomated: true, Limit: 50}
	for _, summary := range []bool{false, true} {
		start := time.Now()
		for range 5 {
			request := f
			if summary {
				request.ExclusionCounts = "all"
			}
			page, err := d.ListSessions(t.Context(), request)
			require.NoError(t, err)
			assert.Equal(t, 1, page.Total)
			if summary {
				assert.Equal(t, &SessionExclusionCounts{}, page.ExclusionCounts)
				continue
			}
			for _, expansion := range []SessionFilter{
				{Project: "selected", ExcludeAutomated: true, Limit: 1},
				{Project: "selected", ExcludeOneShot: true, Limit: 1},
				{Project: "selected", IncludeChildren: true, Limit: MaxSessionLimit},
			} {
				_, err := d.ListSessions(t.Context(), expansion)
				require.NoError(t, err)
			}
		}
		t.Logf("summary=%t: five complete page-and-notice flows took %s", summary, time.Since(start))
	}
	query, args := BuildSessionExclusionCountsSQL(f, SQLiteQueryDialect())
	rows, err := d.getReader().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, strings.Join(plan, "\n"), "SEARCH sessions")
}
