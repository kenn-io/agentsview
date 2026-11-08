package dbtest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// SeedSessionExclusions covers each default category and continuation membership.
func SeedSessionExclusions(t *testing.T, d *db.DB) {
	t.Helper()
	for _, id := range []string{"visible", "one-shot", "automated", "child", "continuation"} {
		SeedSession(t, d, id, "exclusions", func(s *db.Session) {
			s.MessageCount, s.UserMessageCount = 3, 3
			s.Machine = "exclusion-machine"
			s.StartedAt, s.EndedAt = new("2024-01-01T00:00:00Z"), new("2024-01-01T00:00:00Z")
			s.PRLinks = []db.PRLink{AnnotatedPRLink("example/project", 1)}
			if id == "one-shot" || id == "automated" {
				s.UserMessageCount = 1
			}
			if id == "automated" {
				s.IsAutomated, s.SessionKind = true, "roborev"
			}
			if id == "child" || id == "continuation" {
				s.ParentSessionID = new("visible")
				s.RelationshipType = "subagent"
				if id == "continuation" {
					s.RelationshipType = id
				}
			}
		})
		_, err := d.SetSessionLabels(t.Context(), id, []string{"exclusion-label"})
		require.NoError(t, err)
	}
}

// AssertSessionExclusionCounts checks literal counts through each store's list path.
func AssertSessionExclusionCounts(t *testing.T, store db.Store) {
	t.Helper()
	for _, tc := range []struct {
		name                                string
		change                              func(*db.SessionFilter)
		total, oneShot, automated, children int
	}{
		{"defaults", func(*db.SessionFilter) {}, 2, 1, 1, 1},
		{"include one-shot", func(f *db.SessionFilter) { f.ExcludeOneShot = false }, 3, 0, 1, 1},
		{"include automated", func(f *db.SessionFilter) { f.ExcludeAutomated = false }, 3, 1, 0, 1},
		{"tree", func(f *db.SessionFilter) { f.IncludeChildren = true }, 3, 1, 1, 0},
		{"label", func(f *db.SessionFilter) { f.ExclusionCounts = "flat"; f.Labels = []string{"exclusion-label"} }, 5, 0, 0, 0},
		{"label with child summary", func(f *db.SessionFilter) { f.Labels = []string{"exclusion-label"} }, 5, 0, 0, 0},
		{"label explicit one-shot", func(f *db.SessionFilter) {
			f.ExclusionCounts = "flat"
			f.Labels = []string{"exclusion-label"}
			f.OneShotFilterSet = true
		}, 3, 2, 0, 0},
		{"label explicit automated", func(f *db.SessionFilter) {
			f.ExclusionCounts = "flat"
			f.Labels = []string{"exclusion-label"}
			f.AutomatedFilterSet = true
		}, 4, 0, 1, 0},
		{"pull request", func(f *db.SessionFilter) {
			f.ExclusionCounts = "flat"
			f.PR = db.PRFilter{Repository: "example/project", Number: 1}
		}, 5, 0, 0, 0},
		{"restricted", func(f *db.SessionFilter) {
			f.Machine = "exclusion-machine"
			f.Date = "2024-01-01"
			f.OrderBy = "id"
			f.Descending = new(false)
		}, 2, 1, 1, 1},
		{"nothing hidden", func(f *db.SessionFilter) { f.SessionID = "continuation" }, 1, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := db.SessionFilter{Project: "exclusions", ExcludeOneShot: true, ExcludeAutomated: true, ExclusionCounts: "all", Limit: 1}
			tc.change(&f)
			page, err := store.ListSessions(t.Context(), f)
			require.NoError(t, err)
			assert.Equal(t, tc.total, page.Total)
			require.NotNil(t, page.ExclusionCounts)
			assert.Equal(t, &db.SessionExclusionCounts{OneShot: tc.oneShot, Automated: tc.automated, Children: tc.children}, page.ExclusionCounts)
			if page.NextCursor != "" {
				f.Cursor = page.NextCursor
				next, err := store.ListSessions(t.Context(), f)
				require.NoError(t, err)
				assert.Nil(t, next.ExclusionCounts)
			}
		})
	}

	t.Run("fully included", func(t *testing.T) {
		page, err := store.ListSessions(t.Context(), db.SessionFilter{Project: "exclusions", IncludeChildren: true, ExclusionCounts: "all"})
		require.NoError(t, err)
		assert.Equal(t, 5, page.Total)
		assert.Nil(t, page.ExclusionCounts)
	})
}
