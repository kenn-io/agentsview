package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

func TestActivityDaemonReportVersions(t *testing.T) {
	for _, format := range []string{"json", "sse", "page"} {
		for _, version := range []int{export.ActivityReportSchemaVersion, export.ActivityReportSchemaVersion - 1} {
			t.Run(fmt.Sprintf("%s/%d", format, version), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					report := fmt.Sprintf(`{"schema_version":%d,"report_id":"report"}`, version)
					switch format {
					case "sse":
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "event: done\ndata: %s\n\n", report)
					case "page":
						_, _ = fmt.Fprintf(w, `{"report_id":"report","sessions":[],"total":0,"report":%s}`, report)
					default:
						_, _ = fmt.Fprint(w, report)
					}
				}))
				t.Cleanup(server.Close)
				cfg := ActivityReportConfig{}
				if format == "page" {
					cfg.SessionsReportID = "report"
				}
				_, err := fetchHTTPActivityReport(t.Context(), transport{URL: server.URL}, "", cfg)
				if version == export.ActivityReportSchemaVersion {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
					assert.Contains(t, err.Error(), "use matching CLI and daemon versions")
				}
			})
		}
	}
}
