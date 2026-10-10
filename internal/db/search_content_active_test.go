package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchContentExcludeActiveSince(t *testing.T) {
	t.Run("fills page before pagination", func(t *testing.T) {
		d := testDB(t)
		for i := range 7 {
			id := fmt.Sprintf("session-%d", i)
			seedSearchSession(t, d, id, "project-a", [][2]string{{"user", "docker-compose.test.yml"}})
			ended := "2024-06-15T10:00:00Z"
			if i < 3 {
				ended = "2024-06-15T11:59:00Z"
			}
			_, err := d.getWriter().Exec(t.Context(), "UPDATE sessions SET ended_at = ? WHERE id = ?", ended, id)
			require.NoError(t, err)
		}
		filter := ContentSearchFilter{
			Pattern: "docker-compose.test.yml", Limit: 3, ExcludeActiveSince: "2024-06-15T11:50:00Z",
		}
		page, err := d.SearchContent(t.Context(), filter)
		require.NoError(t, err)
		require.Len(t, page.Matches, 3)
		require.NotZero(t, page.NextCursor)
		ids := make([]string, 0, 4)
		for _, match := range page.Matches {
			ids = append(ids, match.SessionID)
		}
		filter.Cursor = page.NextCursor
		page, err = d.SearchContent(t.Context(), filter)
		require.NoError(t, err)
		require.Len(t, page.Matches, 1)
		assert.Zero(t, page.NextCursor)
		ids = append(ids, page.Matches[0].SessionID)
		assert.ElementsMatch(t, []string{"session-3", "session-4", "session-5", "session-6"}, ids)
	})
	d := testDB(t)
	fixtures := []struct {
		id, ended, started string
	}{
		{"active", "2024-06-15T11:59:00Z", "2024-06-15T10:00:00Z"},
		{"offset-active", "2024-06-15T07:59:00-04:00", ""},
		{"started-active", "", "2024-06-15T11:59:00Z"},
		{"boundary", "2024-06-15T07:50:00-04:00", ""},
		{"started-boundary", "", "2024-06-15T11:50:00Z"},
		{"created-active", "", ""},
		{"idle", "2024-06-15T10:00:00Z", ""},
		{"malformed", "invalid", "2024-06-15T11:59:00Z"},
		{"unknown", "", ""},
	}
	hits := make([]VectorHit, 0, len(fixtures))
	for _, f := range fixtures {
		seedSearchSession(t, d, f.id, "project-a", [][2]string{{"user", "needle"}})
		_, err := d.getWriter().Exec(t.Context(),
			"UPDATE sessions SET ended_at = ?, started_at = ?, created_at = ? WHERE id = ?",
			f.ended, f.started, "2024-06-15T11:59:00Z", f.id)
		require.NoError(t, err)
		if f.id == "unknown" {
			_, err = d.getWriter().Exec(t.Context(), "UPDATE sessions SET created_at = '' WHERE id = ?", f.id)
			require.NoError(t, err)
		}
		hits = append(hits, VectorHit{SessionID: f.id, Ordinal: 0, Score: 0.9, Snippet: "needle"})
	}
	d.SetVectorSearcher(&fakeVectorSearcher{hits: hits})
	for _, mode := range []string{"substring", "regex", "fts", "terms", "semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			cutoffs := []string{"2024-06-15T11:50:00Z"}
			if mode == "substring" {
				cutoffs = append(cutoffs, "2024-06-15T13:50:00+02:00", "2024-06-15T06:50:00-05:00")
			}
			for _, cutoff := range cutoffs {
				t.Run(cutoff, func(t *testing.T) {
					page, err := d.SearchContent(t.Context(), ContentSearchFilter{
						Pattern: "needle", Mode: mode, Limit: 6,
						ExcludeActiveSince: cutoff,
					})
					require.NoError(t, err)
					ids := make([]string, 0, len(page.Matches))
					for _, match := range page.Matches {
						ids = append(ids, match.SessionID)
					}
					assert.ElementsMatch(t, []string{"boundary", "started-boundary", "idle", "malformed", "unknown"}, ids)
				})
			}
		})
	}
}

func TestSearchContentExcludeActiveChildBeforeLimit(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "parent", "project-a", [][2]string{{"user", "parent text"}})
	seedSearchSession(t, d, "child", "child-project", [][2]string{{"user", "needle"}})
	seedSearchSession(t, d, "independent", "project-a", [][2]string{{"user", "needle"}})
	_, err := d.getWriter().Exec(t.Context(), "UPDATE sessions SET parent_session_id = 'parent', relationship_type = 'subagent' WHERE id = 'child'")
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), "UPDATE messages SET timestamp = '2024-06-15T11:59:00Z' WHERE session_id = 'child'")
	require.NoError(t, err)
	_, err = d.getWriter().Exec(t.Context(), "UPDATE messages SET timestamp = '2024-06-15T10:00:00Z' WHERE session_id = 'independent'")
	require.NoError(t, err)
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "child", Ordinal: 0, Score: 0.9, Snippet: "needle"},
		{SessionID: "independent", Ordinal: 0, Score: 0.8, Snippet: "needle"},
	}})
	for _, tc := range []struct {
		name, parentEnd, childEnd, project string
	}{
		{"idle parent active child", "2024-06-15T10:00:00Z", "2024-06-15T11:59:00Z", "project-a"},
		{"idle parent active child without project", "2024-06-15T10:00:00Z", "2024-06-15T11:59:00Z", ""},
		{"active parent idle child", "2024-06-15T11:59:00Z", "2024-06-15T10:00:00Z", "project-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.getWriter().Exec(t.Context(), "UPDATE sessions SET ended_at = CASE id WHEN 'parent' THEN ? WHEN 'child' THEN ? ELSE '2024-06-15T10:00:00Z' END", tc.parentEnd, tc.childEnd)
			require.NoError(t, err)
			modes := []string{"substring", "regex", "fts"}
			if tc.name != "active parent idle child" {
				modes = append(modes, "semantic", "hybrid")
			}
			for _, mode := range modes {
				t.Run(mode, func(t *testing.T) {
					page, err := d.SearchContent(t.Context(), ContentSearchFilter{
						Pattern: "needle", Mode: mode, Limit: 1, IncludeChildren: true,
						Project: tc.project, ExcludeActiveSince: "2024-06-15T11:50:00Z",
					})
					require.NoError(t, err)
					require.Len(t, page.Matches, 1)
					if tc.name == "active parent idle child" {
						assert.Equal(t, "child", page.Matches[0].SessionID)
					} else {
						assert.Equal(t, "independent", page.Matches[0].SessionID)
					}
				})
			}
		})
	}
}
