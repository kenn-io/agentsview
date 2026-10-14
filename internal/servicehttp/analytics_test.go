package servicehttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/service"
)

// A daemon that reports the archive changed mid-read hands back a refreshed
// first page; ActivityReport restarts from it instead of failing.
func TestActivityReportRestartsAfterRefresh(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/activity/report":
			_, _ = w.Write([]byte(`{"report_id":"old","projects":{},"by_session":[{"session_id":"a"}],"sessions_next_cursor":"c1","sessions_total":2}`))
		case strings.HasSuffix(r.URL.Path, "/old/sessions"):
			_, _ = w.Write([]byte(`{"report_id":"new","sessions":[{"session_id":"x"}],"total":3,"refresh_required":true,` +
				`"report":{"report_id":"new","projects":{},"by_session":[{"session_id":"x"}],"sessions_next_cursor":"c2","sessions_total":3}}`))
		case strings.HasSuffix(r.URL.Path, "/new/sessions"):
			assert.Equal(t, "c2", r.URL.Query().Get("cursor"))
			_, _ = w.Write([]byte(`{"report_id":"new","sessions":[{"session_id":"y"},{"session_id":"z"}],"total":3}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	report, err := NewHTTPBackend(srv.URL, "", true, "").ActivityReport(t.Context(), service.ActivityReportRequest{
		Preset: "day", Date: "2024-06-01",
	})
	require.NoError(t, err)
	ids := make([]string, 0, len(report.BySession))
	for _, row := range report.BySession {
		ids = append(ids, row.SessionID)
	}
	assert.Equal(t, []string{"x", "y", "z"}, ids)
	assert.Empty(t, report.SessionsNextCursor)
}
