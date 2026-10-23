package servicehttp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/remotecred"
	"go.kenn.io/agentsview/internal/service"
)

func TestPolicyClientCapabilities(t *testing.T) {
	for _, body := range []string{`{"api_version":4,"session_stats_available":true}`, `null`, `{}`, `{"api_version":0}`, `{"api_version":-1}`, `{"api_version":"wrong"}`, ``, `{broken`} {
		t.Run(body, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer remote-only", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer ts.Close()
			caps, err := ProbeHTTPServerCapabilitiesWithClient(t.Context(), ts.URL, "remote-only", ts.Client())
			if body == `{"api_version":4,"session_stats_available":true}` {
				require.NoError(t, err)
				require.True(t, caps.SessionStatsAvailable)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestRemotePolicyUsedByLazyProbe(t *testing.T) {
	hits := 0
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer sink.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/secret-path?key=secret", http.StatusFound)
	}))
	defer source.Close()
	client, err := remotecred.Client(source.URL+"/private?token=secret", false)
	require.NoError(t, err)
	backend := NewHTTPBackendWithClient(source.URL+"/private?token=secret", "remote-only", true, "", client)
	_, err = backend.List(t.Context(), service.ListFilter{Labels: []string{"urgent"}})
	require.Error(t, err)
	require.Zero(t, hits)
	require.NotContains(t, err.Error(), "secret")
	require.NotContains(t, err.Error(), "private")
	require.Contains(t, err.Error(), "--local")
}

func TestRemotePolicyRedactsFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusBadGateway} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte("secret /private?token=secret"))
		}))
		client, err := remotecred.Client(ts.URL, false)
		require.NoError(t, err)
		_, err = ProbeHTTPServerCapabilitiesWithClient(t.Context(), ts.URL+"/private?token=secret", "remote-only", client)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "secret")
		require.NotContains(t, err.Error(), "private")
		require.Contains(t, err.Error(), "--local")
		ts.Close()
	}
}
