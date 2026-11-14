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

func TestCompactionFiltersRequireAPIVersion(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, version := range []int{18, 19} {
			for _, list := range []bool{false, true} {
				t.Run(fmt.Sprintf("known=%t/version=%d/list=%t", known, version, list), func(t *testing.T) {
					var requests []string
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests = append(requests, r.URL.Path)
						w.Header().Set("Content-Type", "application/json")
						if r.URL.Path == "/api/v1/version" {
							fmt.Fprintf(w, `{"api_version":%d}`, version)
							return
						}
						if list {
							assert.Equal(t, "true", r.URL.Query().Get("match_each_session"))
							fmt.Fprint(w, `{"sessions":[],"total":0}`)
							return
						}
						assert.Equal(t, "true", r.URL.Query().Get("compact_boundary"))
						fmt.Fprint(w, `{"messages":[],"count":0}`)
					}))
					defer srv.Close()
					b := NewHTTPBackend(srv.URL, "", false, "").(*httpBackend)
					if known {
						b.apiVersion = version
					}
					var err error
					if list {
						_, err = b.List(t.Context(), service.ListFilter{MatchEachSession: true})
					} else {
						_, err = b.Messages(t.Context(), "s", service.MessageFilter{CompactBoundaryOnly: true})
					}
					if version < 19 {
						require.ErrorContains(t, err, "restart or upgrade")
						if known {
							assert.Empty(t, requests)
						} else {
							assert.Equal(t, []string{"/api/v1/version"}, requests)
						}
					} else {
						require.NoError(t, err)
					}
				})
			}
		}
	}
}

func TestCompactionFiltersRejectUnknownAPIVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer srv.Close()
	b := NewHTTPBackend(srv.URL, "", false, "")
	_, err := b.Messages(t.Context(), "s", service.MessageFilter{CompactBoundaryOnly: true})
	require.Error(t, err)
}

func TestCompactionFiltersInvalidActiveSince(t *testing.T) {
	_, err := filterToQuery(service.ListFilter{ActiveSince: "yesterday"})
	require.ErrorContains(t, err, "invalid active_since")
	require.ErrorContains(t, err, "RFC3339")
}
