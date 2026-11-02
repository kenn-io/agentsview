package db

import (
	"bytes"
	"fmt"
	"log"
	"path/filepath"
	"strings"
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

func TestSidebarTreeSeeksChildrenAfterOptimize(t *testing.T) {
	d := testDB(t)
	for i := range 20 {
		insertSession(t, d, fmt.Sprintf("root-%02d", i), "project")
	}
	path := d.Path()
	require.NoError(t, d.Close())
	d, err := OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })

	filter := SessionFilter{Project: "project", ExcludeAutomated: true, IncludeOrphans: true, Limit: 20}
	predicates := buildSidebarPredicates(filter)
	for _, tt := range []struct {
		name string
		plan func() string
	}{
		{"root page", func() string { return sidebarChildLookupPlan(t, d) }},
		{"starred count", func() string {
			return queryPlanOf(t, d, sidebarStarredRootCountQuery(predicates), predicates.rootArgs...)
		}},
		{"tree page", func() string {
			return queryPlanOf(t, d, sidebarTreePageQuery(predicates, []string{"SELECT ? AS id, ? AS ord"}), "root-00", 0)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plan := tt.plan()
			_, step, ok := strings.Cut(plan, "RECURSIVE STEP\n")
			require.True(t, ok, "expected recursive child lookup\n%s", plan)
			assert.True(t, strings.HasPrefix(step, "SCAN t\nSEARCH s USING INDEX idx_sessions_parent (parent_session_id=? AND is_automated=?)"),
				"each queued tree row must seek its children\n%s", plan)
		})
	}
}

func TestSidebarParentAutomationIndexUpgrade(t *testing.T) {
	d := testDB(t)
	// A few parents with many mostly automated children exercise both
	// the parent-only lookup and the upgraded automation filter.
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
	require.Contains(t, sidebarChildLookupPlan(t, d),
		"USING INDEX idx_sessions_parent (parent_session_id=?)",
		"the recursive child lookup must seek the parent-only index before the upgrade")

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
