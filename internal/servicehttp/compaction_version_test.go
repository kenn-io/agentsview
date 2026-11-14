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

func compactionDaemon(t *testing.T, version, probes *atomic.Int32) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/version":
			probes.Add(1)
			fmt.Fprintf(w, `{"api_version":%d}`, version.Load())
		case "/api/v1/sessions":
			assert.Equal(t, "true", r.URL.Query().Get("has_compaction_summary"))
			fmt.Fprint(w, `{"sessions":[{"id":"one"},{"id":"two"}],"total":2}`)
		default:
			assert.Equal(t, "true", r.URL.Query().Get("compact_boundary"))
			fmt.Fprint(w, `{"messages":[{"ordinal":0,"content":"summary","is_compact_boundary":true}],"count":1}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCompactionFiltersRequireAPIVersion(t *testing.T) {
	for _, v := range []int32{18, 19, 20} {
		t.Run(fmt.Sprint(v), func(t *testing.T) {
			var version, probes atomic.Int32
			version.Store(v)
			b := NewHTTPBackend(compactionDaemon(t, &version, &probes).URL, "", false, "")
			_, listErr := b.List(t.Context(), service.ListFilter{HasCompactionSummary: true})
			_, messagesErr := b.Messages(t.Context(), "one", service.MessageFilter{CompactBoundaryOnly: true})
			if v == 19 {
				require.NoError(t, listErr)
				require.NoError(t, messagesErr)
			} else {
				require.ErrorContains(t, listErr, fmt.Sprintf("API version %d", v))
				require.ErrorContains(t, messagesErr, fmt.Sprintf("API version %d", v))
			}
			assert.EqualValues(t, 1, probes.Load())
		})
	}
	var version, probes atomic.Int32
	b := NewHTTPBackend(compactionDaemon(t, &version, &probes).URL, "", false, "")
	req := service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z"}
	for i, v := range []int32{19, 18, 19, 20} {
		version.Store(v)
		out, err := service.CompactionSummaries(t.Context(), b, req)
		if v == 19 {
			require.NoError(t, err)
			assert.Len(t, out.Summaries, 2)
		} else {
			require.ErrorContains(t, err, fmt.Sprintf("API version %d", v))
		}
		assert.EqualValues(t, i+1, probes.Load())
	}
}
func TestCompactionFiltersInvalidActiveSince(t *testing.T) {
	_, err := filterToQuery(service.ListFilter{ActiveSince: "yesterday"})
	require.ErrorContains(t, err, "invalid active_since")
	require.ErrorContains(t, err, "RFC3339")
}
