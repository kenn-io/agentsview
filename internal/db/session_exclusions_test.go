package db_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestSessionExclusionCounts(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSessionExclusions(t, d)
	dbtest.AssertSessionExclusionCounts(t, d)
}

func TestSessionExclusionChildrenBounds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		roots    int
		childID  string
		children int
		lower    bool
	}{
		{"500 rows", 499, "000-child", 1, false},
		{"501 rows", 500, "000-child", 1, true},
		{"child after prefix", 500, "zzz-child", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			for i := range tc.roots {
				dbtest.SeedSession(t, d, fmt.Sprintf("root-%03d", i), "bounds", func(s *db.Session) { s.UserMessageCount = 3 })
			}
			dbtest.SeedSession(t, d, tc.childID, "bounds", func(s *db.Session) { s.ParentSessionID = new("root-000"); s.RelationshipType = "fork" })
			page, err := d.ListSessions(t.Context(), db.SessionFilter{Project: "bounds", ExclusionCounts: "all", OrderBy: "id", Descending: new(false)})
			require.NoError(t, err)
			require.NotNil(t, page.ExclusionCounts)
			assert.Equal(t, tc.children, page.ExclusionCounts.Children)
			assert.Equal(t, tc.lower, page.ExclusionCounts.ChildrenLowerBound)
		})
	}
}
