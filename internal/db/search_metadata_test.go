package db

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchContentSessionMetadata(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"custom", "provider", "unnamed"} {
		seedSearchSession(t, d, id, "app", [][2]string{{"user", "find fleetneedle"}, {"assistant", "fleetneedle answer"}})
	}
	_, err := d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET machine = 'node-a', display_name = CASE WHEN id = 'custom' THEN 'Custom title' END, session_name = CASE WHEN id IN ('custom','provider') THEN 'Provider title' END`)
	require.NoError(t, err)
	d.SetVectorSearcher(&fakeVectorSearcher{hits: []VectorHit{
		{SessionID: "custom", Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 0, Score: 0.9},
		{SessionID: "provider", Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 0, Score: 0.8},
		{SessionID: "unnamed", Ordinal: 0, OrdinalStart: 0, OrdinalEnd: 0, Score: 0.7},
	}})
	for _, mode := range []string{"substring", "regex", "fts", "terms", "semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			page, err := d.SearchContent(t.Context(), ContentSearchFilter{Pattern: "fleetneedle", Mode: mode, Sources: []string{"messages"}, Limit: 50})
			require.NoError(t, err)
			require.NotEmpty(t, page.Matches)
			seen := map[string]bool{}
			for _, m := range page.Matches {
				seen[m.SessionID] = true
				assert.Equal(t, "node-a", m.Machine)
				switch m.SessionID {
				case "custom":
					require.NotNil(t, m.DisplayName)
					assert.Equal(t, "Custom title", *m.DisplayName)
				case "provider":
					require.NotNil(t, m.DisplayName)
					assert.Equal(t, "Provider title", *m.DisplayName)
				case "unnamed":
					assert.Nil(t, m.DisplayName)
				}
				raw, err := json.Marshal(m)
				require.NoError(t, err)
				assert.Contains(t, string(raw), `"display_name":`)
			}
			assert.Len(t, seen, 3)
		})
	}
}

func TestSearchSessionMachineMetadata(t *testing.T) {
	d := testDB(t)
	seedSearchSession(t, d, "message", "app", [][2]string{{"user", "fleetneedle text"}})
	seedSearchSession(t, d, "title", "app", [][2]string{{"user", "unmatched content"}})
	_, err := d.getWriter().ExecContext(t.Context(), `UPDATE sessions SET machine='node-a', display_name=CASE WHEN id='title' THEN 'fleetneedle title' END`)
	require.NoError(t, err)
	page, err := d.Search(t.Context(), SearchFilter{Query: "fleetneedle", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Results, 2)
	for _, r := range page.Results {
		assert.Equal(t, "node-a", r.Machine)
	}
}
