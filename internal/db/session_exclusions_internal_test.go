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
	for _, tc := range []struct {
		name   string
		filter SessionFilter
		counts int
	}{
		{"defaults", SessionFilter{}, 3},
		{"label", SessionFilter{Labels: []string{"selected"}}, 1},
		{"pull request", SessionFilter{PR: PRFilter{Repository: "example/project", Number: 1}}, 1},
		{"explicit IDs", SessionFilter{IDs: []string{"selected"}, IDsExact: true}, 1},
		{"label explicit one-shot", SessionFilter{Labels: []string{"selected"}, OneShotFilterSet: true}, 2},
		{"explicit IDs automated", SessionFilter{IDs: []string{"selected"}, IDsExact: true, AutomatedFilterSet: true}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.filter
			f.Project, f.ExcludeOneShot, f.ExcludeAutomated = "selected", true, true
			query, args := BuildSessionExclusionCountsSQL(f, SQLiteQueryDialect())
			rows, err := d.getReader().QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
			require.NoError(t, err)
			defer rows.Close()
			var plan []string
			counts := 0
			for rows.Next() {
				var id, parent, unused int
				var detail string
				require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
				plan = append(plan, detail)
				if strings.HasPrefix(detail, "SEARCH sessions ") {
					counts++
				}
			}
			require.NoError(t, rows.Err())
			assert.Equal(t, tc.counts, counts, strings.Join(plan, "\n"))
			assert.NotContains(t, strings.Join(plan, "\n"), "SCAN sessions")
		})
	}
}

type unavailableExclusionChildren struct {
	calls int
}

func (q *unavailableExclusionChildren) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	q.calls++
	return nil, errors.New("child probe unavailable")
}

func TestSessionExclusionCountsKeepsFlatCountsOnChildFailure(t *testing.T) {
	f := SessionFilter{Project: "exclusions", ExcludeOneShot: true, ExcludeAutomated: true, ExclusionCounts: new(true)}
	counts := &SessionExclusionCounts{OneShot: 1, Automated: 1}
	q := &unavailableExclusionChildren{}
	counts.LoadChildren(t.Context(), q, f, SQLiteQueryDialect(), 1)
	assert.Equal(t, 1, q.calls)
	assert.Equal(t, &SessionExclusionCounts{OneShot: 1, Automated: 1}, counts)
}
