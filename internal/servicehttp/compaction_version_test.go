package servicehttp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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

func TestCompactionFiltersCacheVersionProbe(t *testing.T) {
	for _, version := range []int{18, 19} {
		t.Run(fmt.Sprintf("version=%d", version), func(t *testing.T) {
			var probes atomic.Int32
			var reads atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/version" {
					if probes.Add(1) == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					fmt.Fprintf(w, `{"api_version":%d}`, version)
					return
				}
				reads.Add(1)
				if r.URL.Path == "/api/v1/sessions" {
					fmt.Fprint(w, `{"sessions":[],"total":0}`)
				} else {
					fmt.Fprint(w, `{"messages":[],"count":0}`)
				}
			}))
			defer srv.Close()
			b := NewHTTPBackend(srv.URL, "", false, "")
			_, err := b.List(t.Context(), service.ListFilter{HasCompactionSummary: true})
			require.Error(t, err)

			var wg sync.WaitGroup
			for range 4 {
				wg.Go(func() {
					_, err := b.Messages(t.Context(), "s", service.MessageFilter{CompactBoundaryOnly: true})
					if version == 18 {
						assert.ErrorContains(t, err, "restart or upgrade")
					} else {
						assert.NoError(t, err)
					}
				})
			}
			wg.Wait()
			_, err = b.List(t.Context(), service.ListFilter{HasCompactionSummary: true})
			assert.EqualValues(t, 2, probes.Load())
			if version == 18 {
				assert.ErrorContains(t, err, "restart or upgrade")
				assert.Zero(t, reads.Load())
			} else {
				assert.NoError(t, err)
				assert.EqualValues(t, 5, reads.Load())
			}
		})
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

func TestCompactionSummariesProbeOnceForMultipleSessions(t *testing.T) {
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
			fmt.Fprint(w, `{"messages":[{"ordinal":0,"content":"summary"}],"count":1}`)
		}
	}))
	defer srv.Close()
	b := NewHTTPBackend(srv.URL, "", false, "")
	out, err := service.CompactionSummaries(t.Context(), b, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z"})
	require.NoError(t, err)
	require.Len(t, out.Summaries, 2)
	assert.EqualValues(t, 1, probes.Load())
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
