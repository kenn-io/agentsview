package servicehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestListExclusionCounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope *bool
		query string
	}{
		{"all", new(true), "true"},
		{"not requested", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				calls++
				assert.Equal(t, "/api/v1/sessions", r.URL.Path)
				assert.Equal(t, tc.query, r.URL.Query().Get("exclusion_counts"))
				assert.Equal(t, tc.scope != nil, r.URL.Query().Has("exclusion_counts"))
				if tc.scope == nil {
					_, _ = w.Write([]byte(`{"sessions":[],"total":0}`))
					return
				}
				_, _ = w.Write([]byte(`{"sessions":[],"total":0,"exclusion_counts":{"one_shot":2,"automated":3,"children":4,"children_lower_bound":true}}`))
			}))
			t.Cleanup(srv.Close)
			b := NewHTTPBackendForServer(srv.URL, "", HTTPServerCapabilities{})
			page, err := b.List(t.Context(), service.ListFilter{ExclusionCounts: tc.scope})
			require.NoError(t, err)
			assert.Equal(t, 1, calls)
			if tc.scope != nil {
				assert.Equal(t, &db.SessionExclusionCounts{OneShot: 2, Automated: 3, Children: 4, ChildrenLowerBound: true}, page.ExclusionCounts)
			}
		})
	}
}
