package servicehttp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/service"
)

func TestListExclusionCountsVersion(t *testing.T) {
	for _, tc := range []struct {
		name              string
		known, advertised int
		scope             string
		missing           bool
		wantError         bool
	}{
		{"old", 15, 15, "all", false, true},
		{"current", 16, 16, "all", false, false},
		{"unknown", 0, 0, "all", false, true},
		{"not requested on old", 15, 15, "", false, false},
		{"missing summary", 16, 16, "all", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/version" {
					fmt.Fprintf(w, `{"api_version":%d}`, tc.advertised)
					return
				}
				calls++
				assert.Equal(t, tc.scope, r.URL.Query().Get("exclusion_counts"))
				if tc.missing || tc.scope == "" {
					_, _ = w.Write([]byte(`{"sessions":[],"total":0}`))
					return
				}
				_, _ = w.Write([]byte(`{"sessions":[],"total":0,"exclusion_counts":{"one_shot":2,"automated":3,"children":4,"children_lower_bound":true}}`))
			}))
			t.Cleanup(srv.Close)
			b := NewHTTPBackendForServer(srv.URL, "", HTTPServerCapabilities{APIVersion: tc.known})
			page, err := b.List(t.Context(), service.ListFilter{ExclusionCounts: tc.scope})
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 1, calls)
			if tc.scope == "" {
				assert.Nil(t, page.ExclusionCounts)
				return
			}
			require.NotNil(t, page.ExclusionCounts)
			assert.Equal(t, 4, page.ExclusionCounts.Children)
			assert.True(t, page.ExclusionCounts.ChildrenLowerBound)
		})
	}
}
