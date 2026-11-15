package db_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestSearchContentWordParity(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedWordSearch(t, d)
	dbtest.AssertWordSearch(t, d)
}

func TestSearchContentExplicitFTS(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedWordSearch(t, d)
	for _, tc := range []struct {
		pattern, project string
		want             []string
	}{
		{"invoice*", "word-literal-star", []string{"literal-star", "plain-invoice"}},
		{"invoice* rounding", "word-star", []string{"star-both"}},
		{`NOT "invoice"`, "word-literal-not", []string{"literal-not"}},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			page, err := d.SearchContent(t.Context(), db.ContentSearchFilter{Pattern: tc.pattern, Project: tc.project, Mode: "fts"})
			require.NoError(t, err)
			var ids []string
			for _, match := range page.Matches {
				ids = append(ids, match.SessionID)
			}
			assert.ElementsMatch(t, tc.want, ids)
		})
	}
}
