package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIRecoveryHTTP(t *testing.T) {
	isolateDirectCLISources(t)
	newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "")
	for _, tc := range []struct {
		name, body, kind, message, hint string
		status, code                    int
		closed, noDaemonRestartAdvice   bool
		private                         string
	}{
		{name: "authentication required", status: http.StatusUnauthorized, code: 1, body: `{"error":"unauthorized"}`, kind: "authentication", message: "server rejected authentication", hint: "--server-token-file"},
		{name: "plain forbidden authentication response", status: http.StatusForbidden, code: 1, body: "Forbidden", kind: "authentication", message: "server rejected authentication", hint: "AGENTSVIEW_SERVER_TOKEN"},
		{name: "application policy rejection", status: http.StatusForbidden, code: 1, body: `{"error":"archive-only policy: use agentsview archive import or agentsview archive reparse","debug":"private response body"}`, kind: "forbidden", message: "archive-only policy: use agentsview archive import or agentsview archive reparse", hint: "agentsview session get --help", private: "private response body"},
		{name: "bad request", status: http.StatusBadRequest, code: 65, body: `{"error":"bad request value"}`, kind: "validation", message: "bad request value", hint: "agentsview session get --help"},
		{name: "unprocessable request", status: http.StatusUnprocessableEntity, code: 65, body: `{"error":"bad request value"}`, kind: "validation", message: "bad request value", hint: "agentsview session get --help"},
		{name: "stale archive conflict", status: http.StatusConflict, code: 1, body: `{"error":"archive is stale; wait for the rebuild or run 'agentsview sync --full' before syncing this session","debug":"private response body"}`, kind: "conflict", message: "archive is stale; wait for the rebuild or run 'agentsview sync --full' before syncing this session", hint: "agentsview session get --help", private: "private response body"},
		{name: "missing session", status: http.StatusNotFound, code: 1, body: `{"error":"session not found"}`, kind: "not_found", message: "session codex:fixture not found", hint: "session list --json"},
		{name: "server error", status: http.StatusInternalServerError, code: 1, body: `{"error":"bad request value"}`, kind: "error", message: "server error (HTTP 500)", hint: "server logs"},
		{name: "server unavailable", code: 69, closed: true, noDaemonRestartAdvice: true, kind: "unavailable", message: "cannot reach server", hint: "--server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(tc.body, "{") {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			if tc.closed {
				ts.Close()
			} else {
				t.Cleanup(ts.Close)
			}
			for _, output := range []struct {
				name string
				json bool
			}{{"human", false}, {"json", true}} {
				t.Run(output.name, func(t *testing.T) {
					args := []string{"session", "get", "codex:fixture", "--server", ts.URL}
					if output.json {
						args = append(args, "--json")
					}
					var stdout, stderr bytes.Buffer
					err := executeCLIWithLegacyFlagCompat(args, &stdout, &stderr)
					require.Error(t, err)
					assert.Equal(t, tc.code, reportCLIError(err, &stderr))
					assert.Empty(t, stdout.String())
					if output.json {
						var diagnostic struct {
							Error   string `json:"error"`
							Message string `json:"message"`
							Hint    string `json:"hint"`
						}
						require.NoError(t, json.Unmarshal(stderr.Bytes(), &diagnostic), stderr.String())
						assert.Equal(t, tc.kind, diagnostic.Error)
						assert.Equal(t, tc.message, diagnostic.Message)
						assert.Contains(t, diagnostic.Hint, tc.hint)
					} else {
						assert.Contains(t, stderr.String(), tc.message)
						assert.Contains(t, stderr.String(), tc.hint)
					}
					assert.NotContains(t, stderr.String(), "/api/v1")
					if tc.private != "" {
						assert.NotContains(t, stderr.String(), tc.private)
					}
					if tc.noDaemonRestartAdvice {
						assert.NotContains(t, stderr.String(), "daemon restart")
					}
				})
			}
		})
	}
}

func TestCLIRecoveryRawSyncEnvironmentTarget(t *testing.T) {
	legacyRecoveryEnv(t)
	ts := httptest.NewServer(http.NotFoundHandler())
	ts.Close()
	t.Setenv("AGENTSVIEW_RAW_SYNC_URL", ts.URL)
	t.Setenv("AGENTSVIEW_RAW_SYNC_DEVICE_ID", "fixture-device")
	t.Setenv("AGENTSVIEW_RAW_SYNC_CREDENTIAL", "fixture-credential")
	var stdout, stderr bytes.Buffer
	err := executeCLIWithLegacyFlagCompat([]string{"raw-sync", "server-status", "--allow-insecure-http"}, &stdout, &stderr)
	require.Error(t, err)
	assert.Equal(t, 69, reportCLIError(err, &stderr))
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "AGENTSVIEW_RAW_SYNC_URL")
	assert.NotContains(t, stderr.String(), "daemon status")
}

func TestCLIRecoveryUpdateNetworkAdvice(t *testing.T) {
	cmd, _, err := newRootCommand().Find([]string{"update"})
	require.NoError(t, err)
	cause := &url.Error{Op: "Get", URL: "https://example.invalid/private-path?token=fixture-secret", Err: errors.New("fixture unavailable")}
	var stderr bytes.Buffer
	err = contextualizeCLIError(cmd, cause, false)
	assert.Equal(t, 69, reportCLIError(err, &stderr))
	require.ErrorIs(t, err, cause)
	assert.Contains(t, stderr.String(), "GitHub")
	assert.Contains(t, stderr.String(), "agentsview update --help")
	assert.NotContains(t, stderr.String(), "daemon")
	assert.NotContains(t, stderr.String(), "fixture-secret")
}
