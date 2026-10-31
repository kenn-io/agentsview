package main

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRawSyncServerHealth(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		invalid    bool
	}{
		{"complete", `{"observed_at":"2026-10-23T00:00:00Z","max_attempts":7,"stale_after_seconds":900,"dead_letters":{"count":0,"oldest_failed_at":null},"failed_job_count":3,"orphaned_manifests":[],"orphaned_manifest_count":0,"expired_leases":[],"expired_lease_count":0,"failed_jobs_by_error_class":[],"retrying_near_limit":[],"retrying_near_limit_count":0,"stale_source_heads":[],"stale_source_head_count":0}`, false},
		{"missing dead letters", `{"observed_at":"2026-10-23T00:00:00Z","max_attempts":7,"stale_after_seconds":900,"failed_job_count":3,"orphaned_manifests":[],"orphaned_manifest_count":0,"expired_leases":[],"expired_lease_count":0,"failed_jobs_by_error_class":[],"retrying_near_limit":[],"retrying_near_limit_count":0,"stale_source_heads":[],"stale_source_head_count":0}`, true},
		{"incomplete", `{"observed_at":"2026-10-23T00:00:00Z","max_attempts":7,"stale_after_seconds":900}`, true},
		{"null", `null`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "health-credential")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/raw-sync/tokens":
					var body struct {
						Scopes []string `json:"scopes"`
					}
					if !assert.NoError(t, json.UnmarshalRead(r.Body, &body)) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					assert.Equal(t, []string{"status"}, body.Scopes)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"token":"avdt_health","device_id":"device-a","scopes":["status"],"expires_at":"2099-01-01T00:00:00Z"}`))
				case "/api/v1/raw-sync/health":
					assert.Equal(t, "7", r.URL.Query().Get("max_attempts"))
					assert.Equal(t, "900", r.URL.Query().Get("stale_after_seconds"))
					assert.Equal(t, "Bearer avdt_health", r.Header.Get("Authorization"))
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tc.body))
				default:
					assert.Failf(t, "unexpected route", "%s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			output, err := executeCommand(newRootCommand(), "raw-sync", "server-health", "--server", server.URL, "--device-id", "device-a", "--allow-insecure-http", "--max-attempts", "7", "--stale-after-seconds", "900")
			if tc.invalid {
				require.ErrorContains(t, err, "health response")
				return
			}
			require.NoError(t, err)
			var got struct {
				FailedJobCount int64 `json:"failed_job_count"`
			}
			require.NoError(t, json.Unmarshal([]byte(output), &got))
			assert.Equal(t, int64(3), got.FailedJobCount)
		})
	}
}

func TestRawSyncServerHealthRequiresThresholds(t *testing.T) {
	_, err := executeCommand(newRootCommand(), "raw-sync", "server-health")
	require.ErrorContains(t, err, "required")
}

func TestRawSyncServerHealthRejectsInvalidThresholds(t *testing.T) {
	for _, args := range [][]string{{"--max-attempts", "0", "--stale-after-seconds", "900"}, {"--max-attempts", "7", "--stale-after-seconds", "-1"}} {
		_, err := executeCommand(newRootCommand(), append([]string{"raw-sync", "server-health"}, args...)...)
		require.ErrorContains(t, err, "must be positive")
	}
}
