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
	assert.NotContains(t, strings.Join(plan, "\n"), "SCAN sessions")
}

type unavailableExclusionChildren struct {
	calls int
}

func (q *unavailableExclusionChildren) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	q.calls++
	return nil, errors.New("child probe unavailable")
}

func TestSessionExclusionCountsKeepsFlatCountsOnChildFailure(t *testing.T) {
	f := SessionFilter{Project: "exclusions", ExcludeOneShot: true, ExcludeAutomated: true, ExclusionCounts: "all"}
	counts := &SessionExclusionCounts{OneShot: 1, Automated: 1}
	q := &unavailableExclusionChildren{}
	counts.LoadChildren(t.Context(), q, f, SQLiteQueryDialect(), 1)
	assert.Equal(t, 1, q.calls)
	assert.Equal(t, &SessionExclusionCounts{OneShot: 1, Automated: 1}, counts)
}
