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
		name                      string
		known, advertised         int
		cursor, included, missing bool
		wantError                 bool
	}{
		{"old", 15, 15, false, false, false, true},
		{"current", 16, 16, false, false, false, false},
		{"probe current", 0, 16, false, false, false, false},
		{"unknown", 0, 0, false, false, false, true},
		{"cursor on old", 15, 15, true, false, false, false},
		{"included on old", 15, 15, false, true, false, false},
		{"missing summary", 16, 16, false, false, true, true},
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
				if tc.cursor || tc.included {
					assert.Empty(t, r.URL.Query().Get("exclusion_counts"))
				} else {
					assert.Equal(t, "all", r.URL.Query().Get("exclusion_counts"))
				}
				if tc.missing {
					_, _ = w.Write([]byte(`{"sessions":[],"total":0}`))
					return
				}
				_, _ = w.Write([]byte(`{"sessions":[],"total":0,"exclusion_counts":{"one_shot":2,"automated":3,"children":4,"children_lower_bound":true}}`))
			}))
			t.Cleanup(srv.Close)
			b := NewHTTPBackendForServer(srv.URL, "", HTTPServerCapabilities{APIVersion: tc.known})
			f := service.ListFilter{ExclusionCounts: "all", IncludeOneShot: tc.included, IncludeAutomated: tc.included, IncludeChildren: tc.included}
			if tc.cursor {
				f.Cursor = "next"
			}
			page, err := b.List(t.Context(), f)
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 1, calls)
			require.NotNil(t, page.ExclusionCounts)
			assert.Equal(t, 4, page.ExclusionCounts.Children)
			assert.True(t, page.ExclusionCounts.ChildrenLowerBound)
		})
	}
}
