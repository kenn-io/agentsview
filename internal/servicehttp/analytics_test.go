package servicehttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// Paging the activity report either yields every session or an error; a
// failure partway through must not return a report missing rows.
func TestActivityReportPagingFailures(t *testing.T) {
	const firstPage = `{"report_id":"r1","projects":{},"by_session":[{"session_id":"a"}],"sessions_next_cursor":"c1","sessions_total":2}`
	refresh := `{"report_id":"r1","sessions":[],"total":2,"refresh_required":true,` +
		`"report":{"report_id":"r1","projects":{},"by_session":[{"session_id":"a"}],"sessions_next_cursor":"c1","sessions_total":2}}`
	tests := []struct {
		name      string
		page      func(w http.ResponseWriter)
		wantErr   string
		wantPages int32
	}{
		{
			name: "page request fails",
			page: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"report expired"}`))
			},
			wantErr:   "HTTP 500",
			wantPages: 1,
		},
		{
			name: "empty page with a cursor",
			page: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(`{"report_id":"r1","sessions":[],"total":2,"next_cursor":"c1"}`))
			},
			wantErr:   "daemon returned an empty activity session page",
			wantPages: 1,
		},
		{
			name: "archive keeps changing",
			page: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(refresh))
			},
			wantErr:   "activity data kept changing while reading its sessions; try again",
			wantPages: maxActivityRefreshes + 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pages atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/api/v1/activity/report":
					_, _ = w.Write([]byte(firstPage))
				case strings.HasSuffix(r.URL.Path, "/r1/sessions"):
					pages.Add(1)
					tt.page(w)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)

			report, err := NewHTTPBackend(srv.URL, "", true, "").ActivityReport(t.Context(), service.ActivityReportRequest{
				Preset: "day", Date: "2024-06-01",
			})
			require.ErrorContains(t, err, tt.wantErr)
			assert.Nil(t, report)
			assert.Equal(t, tt.wantPages, pages.Load())
		})
	}
}
