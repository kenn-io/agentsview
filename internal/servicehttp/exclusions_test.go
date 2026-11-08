package servicehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/service"
)

func TestListExclusionCounts(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
		scope   string
		missing bool
	}{
		{"all", 15, "all", false},
		{"flat", 15, "flat", false},
		{"unknown", 0, "all", true},
		{"not requested", 15, "", false},
		{"omitted summary", 15, "all", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				calls++
				assert.Equal(t, "/api/v1/sessions", r.URL.Path)
				assert.Equal(t, tc.scope, r.URL.Query().Get("exclusion_counts"))
				if tc.missing || tc.scope == "" {
					_, _ = w.Write([]byte(`{"sessions":[],"total":0}`))
					return
				}
				_, _ = w.Write([]byte(`{"sessions":[],"total":0,"exclusion_counts":{"one_shot":2,"automated":3,"children":4,"children_lower_bound":true}}`))
			}))
			t.Cleanup(srv.Close)
			b := NewHTTPBackendForServer(srv.URL, "", HTTPServerCapabilities{APIVersion: tc.version})
			page, err := b.List(t.Context(), service.ListFilter{ExclusionCounts: tc.scope})
			require.NoError(t, err)
			assert.Equal(t, 1, calls)
			if tc.scope == "" || tc.missing {
				assert.Nil(t, page.ExclusionCounts)
				return
			}
			require.NotNil(t, page.ExclusionCounts)
			assert.Equal(t, 4, page.ExclusionCounts.Children)
			assert.True(t, page.ExclusionCounts.ChildrenLowerBound)
		})
	}
}
