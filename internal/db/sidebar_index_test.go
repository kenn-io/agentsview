package db

import (
	"bytes"
	"fmt"
	"log"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sidebarParentIndexRebuildLog = "rebuilding stale SQLite index idx_sessions_parent"

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousWriter) })
	return &output
}

func sidebarChildLookupPlan(t *testing.T, d *DB) string {
	t.Helper()
	filter := SessionFilter{
		Project: "project", ExcludeAutomated: true, IncludeOrphans: true, Limit: 1,
	}
	query, args := sidebarRootPageQuery(filter, SessionCursor{}, buildSidebarPredicates(filter))
	return queryPlanOf(t, d, query, args...)
}

func TestSidebarParentAutomationIndexUpgrade(t *testing.T) {
	d := testDB(t)
	// A few parents with many mostly automated children reproduce the
	// reported plan: the parent-only index loses to a transient
	// is_automated index rebuilt on every recursive step.
	for i := range 40 {
		parent := fmt.Sprintf("root-%02d", i)
		insertSession(t, d, parent, "project")
		if i%4 != 0 {
			continue
		}
		for j := range 8 {
			insertSession(t, d, fmt.Sprintf("%s-child-%d", parent, j), "project", func(s *Session) {
				s.ParentSessionID = &parent
				s.RelationshipType = "subagent"
				if j > 0 {
					s.FirstMessage = new("You are a code reviewer. Review the code.")
				}
			})
		}
	}
	_, err := d.getWriter().Exec(t.Context(), `
		DROP INDEX idx_sessions_parent;
		CREATE INDEX idx_sessions_parent ON sessions(parent_session_id)
		WHERE parent_session_id IS NOT NULL;
		ANALYZE`)
	require.NoError(t, err)
	require.Contains(t, sidebarChildLookupPlan(t, d), "SEARCH s USING AUTOMATIC",
		"fixture must reproduce the transient-index plan before the upgrade")

	output := captureLog(t)
	path := d.Path()
	require.NoError(t, d.Close())
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	assert.Contains(t, output.String(), sidebarParentIndexRebuildLog)

	var stat string
	require.NoError(t, d.getReader().QueryRow(t.Context(),
		`SELECT stat FROM sqlite_stat1 WHERE idx = 'idx_sessions_parent'`).Scan(&stat))
	assert.NotEmpty(t, stat)

	plan := sidebarChildLookupPlan(t, d)
	assert.Contains(t, plan,
		"USING INDEX idx_sessions_parent (parent_session_id=? AND is_automated=?)")
	assert.NotContains(t, plan, "SEARCH s USING AUTOMATIC",
		"the recursive child lookup must not fall back to a transient index")
}

func TestSidebarParentIndexOnFreshArchiveIsQuiet(t *testing.T) {
	output := captureLog(t)
	d, err := OpenIsolated(t.Context(), filepath.Join(t.TempDir(), "fresh.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })

	requireIndexColumns(t, d, "idx_sessions_parent", []string{"parent_session_id", "is_automated"})
	assert.NotContains(t, output.String(), sidebarParentIndexRebuildLog)
}
