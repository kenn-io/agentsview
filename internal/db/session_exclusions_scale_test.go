package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionExclusionCountsSelectiveProject(t *testing.T) {
	d := testDB(t)
	f := SessionFilter{Project: "selected", ExcludeOneShot: true, ExcludeAutomated: true, Limit: 50}
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

type unavailableExclusionChildren struct {
	calls int
}

func (q *unavailableExclusionChildren) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	q.calls++
	return nil, errors.New("child probe unavailable")
}

func TestSessionExclusionCountsKeepsFlatCountsOnChildFailure(t *testing.T) {
	d := testDB(t)
	_, err := d.getWriter().ExecContext(t.Context(), `
		INSERT INTO sessions (id, project, machine, agent, message_count, user_message_count, is_automated)
		VALUES ('visible', 'exclusions', 'local', 'claude', 3, 3, 0),
		       ('one-shot', 'exclusions', 'local', 'claude', 3, 1, 0),
		       ('automated', 'exclusions', 'local', 'claude', 3, 1, 1)`)
	require.NoError(t, err)
	f := SessionFilter{Project: "exclusions", ExcludeOneShot: true, ExcludeAutomated: true, ExclusionCounts: "all"}
	total, counts, err := CountSessionExclusions(t.Context(), d.getReader(), f, SQLiteQueryDialect())
	require.NoError(t, err)
	require.NotNil(t, counts)
	q := &unavailableExclusionChildren{}
	counts.LoadChildren(t.Context(), q, f, SQLiteQueryDialect(), total)
	assert.Equal(t, 1, q.calls)
	assert.Equal(t, &SessionExclusionCounts{OneShot: 1, Automated: 1}, counts)
}
