package servicehttp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/service"
)

func TestCompactionFiltersRequireAPIVersion(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, version := range []int{18, 19, 20} {
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
							assert.Equal(t, "true", r.URL.Query().Get("has_compaction_summary"))
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
						_, err = b.List(t.Context(), service.ListFilter{HasCompactionSummary: true})
					} else {
						_, err = b.Messages(t.Context(), "s", service.MessageFilter{CompactBoundaryOnly: true})
					}
					if version < 19 {
						require.ErrorContains(t, err, "restart or upgrade")
						assert.Equal(t, []string{"/api/v1/version"}, requests)
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

func TestCompactionSummariesProbeForMultipleSessions(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/version":
			probes.Add(1)
			fmt.Fprint(w, `{"api_version":19}`)
		case "/api/v1/sessions":
			fmt.Fprint(w, `{"sessions":[{"id":"one"},{"id":"two"}],"total":2}`)
		default:
			fmt.Fprint(w, `{"messages":[{"ordinal":0,"content":"summary","is_compact_boundary":true}],"count":1}`)
		}
	}))
	defer srv.Close()
	b := NewHTTPBackend(srv.URL, "", false, "")
	out, err := service.CompactionSummaries(t.Context(), b, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z"})
	require.NoError(t, err)
	require.Len(t, out.Summaries, 2)
	assert.EqualValues(t, 3, probes.Load())
}

func TestCompactionAndLabelsRequireVersion19(t *testing.T) {
	for _, version := range []int{18, 19} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			var probes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/version" {
					probes.Add(1)
					fmt.Fprintf(w, `{"api_version":%d}`, version)
					return
				}
				fmt.Fprint(w, `{"sessions":[],"total":0}`)
			}))
			defer srv.Close()
			b := NewHTTPBackend(srv.URL, "", false, "")
			_, err := b.List(t.Context(), service.ListFilter{HasCompactionSummary: true, Labels: []string{"label"}})
			if version < 19 {
				require.ErrorContains(t, err, "compaction summaries")
			} else {
				require.NoError(t, err)
			}
			assert.EqualValues(t, 1, probes.Load())
		})
	}
}

func TestCompactionFiltersFollowDaemonVersion(t *testing.T) {
	var version atomic.Int32
	version.Store(19)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/version" {
			fmt.Fprintf(w, `{"api_version":%d}`, version.Load())
			return
		}
		if r.URL.Path == "/api/v1/sessions" {
			fmt.Fprint(w, `{"sessions":[],"total":0}`)
			return
		}
		fmt.Fprint(w, `{"messages":[{"ordinal":1,"content":"injected prompt","is_system":true}],"count":1}`)
	}))
	defer srv.Close()
	b := NewHTTPBackend(srv.URL, "", false, "")
	_, err := b.List(t.Context(), service.ListFilter{HasCompactionSummary: true})
	require.NoError(t, err)
	version.Store(18)
	_, err = b.Messages(t.Context(), "session", service.MessageFilter{CompactBoundaryOnly: true})
	require.ErrorContains(t, err, "restart or upgrade")
	_, err = b.List(t.Context(), service.ListFilter{HasCompactionSummary: true})
	require.ErrorContains(t, err, "restart or upgrade")
	version.Store(19)
	_, err = b.List(t.Context(), service.ListFilter{HasCompactionSummary: true})
	require.NoError(t, err)
}

func TestCompactionSummariesFilterRowsAfterDaemonReplacement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/version":
			fmt.Fprint(w, `{"api_version":19}`)
		case "/api/v1/sessions/session":
			fmt.Fprint(w, `{"id":"session","agent":"claude"}`)
		case "/api/v1/sessions/session/messages":
			fmt.Fprint(w, `{"messages":[{"ordinal":0,"content":"ordinary"},{"ordinal":1,"content":"injected prompt","is_system":true},{"ordinal":2,"content":"summary","is_compact_boundary":true}],"count":3}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	out, err := service.CompactionSummaries(t.Context(), NewHTTPBackend(srv.URL, "", false, ""), service.CompactionSummariesRequest{SessionID: "session"})
	require.NoError(t, err)
	require.Len(t, out.Summaries, 1)
	assert.Equal(t, "summary", out.Summaries[0].Content)
}
