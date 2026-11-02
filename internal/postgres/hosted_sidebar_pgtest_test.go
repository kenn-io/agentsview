//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// Unstarred unlimited reads retain the independent parent predicate. Paging
// must keep the same public rows; starred cases also check explicit expectations.
func checkHostedSidebarPages(t *testing.T, h *HostedStore, filter db.SessionFilter, roots int, ids []string) {
	t.Helper()
	filter.Limit = 0
	full, err := h.GetSidebarSessionIndex(t.Context(), filter)
	require.NoError(t, err)
	require.Equal(t, roots, full.Total)
	want := make(map[string]db.SidebarSessionIndexRow)
	for _, row := range full.Sessions {
		want[row.ID] = row
	}
	if ids != nil {
		assert.Len(t, want, len(ids))
		for _, id := range ids {
			require.Contains(t, want, id)
		}
	}
	filter.Limit = 1
	seen := make(map[string]db.SidebarSessionIndexRow)
	cursors := make(map[string]bool)
	for page := 0; ; page++ {
		require.LessOrEqual(t, page, roots, "sidebar cursor did not terminate")
		index, err := h.GetSidebarSessionIndex(t.Context(), filter)
		require.NoError(t, err)
		require.Equal(t, roots, index.Total)
		for _, row := range index.Sessions {
			require.Contains(t, want, row.ID)
			assert.Equal(t, want[row.ID], row)
			seen[row.ID] = row
		}
		if index.NextCursor == "" {
			break
		}
		require.False(t, cursors[index.NextCursor], "sidebar cursor repeated")
		cursors[index.NextCursor] = true
		filter.Cursor = index.NextCursor
	}
	assert.Equal(t, want, seen)
}

func TestHostedSidebarPagesKeepMixedParentGraphs(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	parsed := projectionOutcome("raw parent")
	parsed.Outcome.Results = append(parsed.Outcome.Results, linkChildOutcome().Outcome.Results...)
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, parsed))
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions
		(id,project,machine,agent,parent_session_id,relationship_type,message_count,user_message_count,started_at,ended_at) VALUES
		('legacy-root','legacy','legacy','claude',NULL,'',2,1,'2026-01-02','2026-01-02'),
		('legacy-child','legacy','legacy','claude','legacy-root','subagent',2,1,'2026-01-04','2026-01-04'),
		('legacy-orphan','orphan','legacy','claude','missing','subagent',2,1,'2026-01-03','2026-01-03')`)
	require.NoError(t, err)
	h, err := newHostedAdapter(f.runtime, f.tenant)
	require.NoError(t, err)
	h.SetCursorSecret([]byte("synthetic mixed sidebar cursor"))
	for _, tc := range []struct {
		project string
		roots   int
		ids     []string
	}{
		{"", 3, []string{"codex:portable", "codex:child", "legacy-root", "legacy-child", "legacy-orphan"}},
		{"synthetic", 1, []string{"codex:portable", "codex:child"}},
		{"legacy", 1, []string{"legacy-root", "legacy-child"}},
		{"orphan", 1, []string{"legacy-orphan"}},
		{"missing", 0, []string{}},
	} {
		t.Run(tc.project, func(t *testing.T) {
			checkHostedSidebarPages(t, h, db.SessionFilter{Project: tc.project}, tc.roots, tc.ids)
		})
	}
	first, err := h.GetSidebarSessionIndex(t.Context(), db.SessionFilter{Limit: 1})
	require.NoError(t, err)
	require.Len(t, first.Sessions, 2)
	assert.ElementsMatch(t, []string{"legacy-root", "legacy-child"},
		[]string{first.Sessions[0].ID, first.Sessions[1].ID}, "descendant activity selects the first root tree")
	for _, id := range []string{"legacy-child", "codex:child"} {
		starred, err := h.StarSession(t.Context(), id)
		require.NoError(t, err)
		require.True(t, starred)
	}
	checkHostedSidebarPages(t, h, db.SessionFilter{Starred: true}, 2,
		[]string{"codex:portable", "codex:child", "legacy-root", "legacy-child"})
}
