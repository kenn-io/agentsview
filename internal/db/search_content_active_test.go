package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchContentExcludeActiveSince(t *testing.T) {
	d := testDB(t)
	fixtures := []struct {
		id, ended, started string
	}{
		{"active", "2024-06-15T11:59:00Z", "2024-06-15T10:00:00Z"},
		{"offset-active", "2024-06-15T07:59:00-04:00", ""},
		{"started-active", "", "2024-06-15T11:59:00Z"},
		{"boundary", "2024-06-15T07:50:00-04:00", ""},
		{"started-boundary", "", "2024-06-15T11:50:00Z"},
		{"created-boundary", "", ""},
		{"idle", "2024-06-15T10:00:00Z", ""},
	}
	hits := make([]VectorHit, 0, len(fixtures))
	for _, f := range fixtures {
		seedSearchSession(t, d, f.id, "project-a", [][2]string{{"user", "needle"}})
		_, err := d.getWriter().Exec(t.Context(),
			"UPDATE sessions SET ended_at = ?, started_at = ?, created_at = ? WHERE id = ?",
			f.ended, f.started, "2024-06-15T11:50:00Z", f.id)
		require.NoError(t, err)
		hits = append(hits, VectorHit{SessionID: f.id, Ordinal: 0, Score: 0.9, Snippet: "needle"})
	}
	d.SetVectorSearcher(&fakeVectorSearcher{hits: hits})
	for _, mode := range []string{"substring", "regex", "fts", "terms", "semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			page, err := d.SearchContent(t.Context(), ContentSearchFilter{
				Pattern: "needle", Mode: mode, Limit: 4,
				ExcludeActiveSince: "2024-06-15T11:50:00Z",
			})
			require.NoError(t, err)
			ids := make([]string, 0, len(page.Matches))
			for _, match := range page.Matches {
				ids = append(ids, match.SessionID)
			}
			assert.ElementsMatch(t, []string{"boundary", "started-boundary", "created-boundary", "idle"}, ids)
		})
	}
}
