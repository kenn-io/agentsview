package servicehttp_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestCompactionSummariesDaemonReplacement(t *testing.T) {
	var version, probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/version":
			probes.Add(1)
			fmt.Fprintf(w, `{"api_version":%d}`, version.Load())
		case "/api/v1/sessions":
			assert.Equal(t, "true", r.URL.Query().Get("flat"))
			fmt.Fprint(w, `{"sessions":[{"id":"one"},{"id":"two"}],"total":2}`)
		default:
			fmt.Fprint(w, `{"messages":[{"ordinal":1,"content":"summary","is_compact_boundary":true}],"count":1}`)
		}
	}))
	defer srv.Close()
	backend := servicehttp.NewHTTPBackend(srv.URL, "", false, "")
	req := service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z"}
	for i, v := range []int32{19, 18, 19, 20} {
		version.Store(v)
		out, err := service.CompactionSummaries(t.Context(), backend, req)
		if v >= 19 {
			require.NoError(t, err)
			require.Len(t, out.Summaries, 2)
			assert.Equal(t, "summary", out.Summaries[0].Content)
		} else {
			require.ErrorContains(t, err, fmt.Sprintf("API version %d", v))
			assert.Empty(t, out.Summaries)
		}
		assert.EqualValues(t, i+1, probes.Load())
	}
}
