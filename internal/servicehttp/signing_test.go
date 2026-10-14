package servicehttp

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/apiclient"
	"go.kenn.io/agentsview/internal/requestsign"
	"go.kenn.io/agentsview/internal/service"
)

func TestSignedReaderDiscoversNonRecordingRecall(t *testing.T) {
	var handler http.Handler
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	defer ts.Close()
	secret := make([]byte, 64)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	require.NoError(t, os.WriteFile(keyFile, []byte(base64.StdEncoding.EncodeToString(secret)), 0o600))
	state := filepath.Join(dir, "replay.db")
	require.NoError(t, requestsign.InitReplay(state))
	replay, err := requestsign.OpenReplay(state)
	require.NoError(t, err)
	defer replay.Close()
	verifier, err := requestsign.NewVerifier(requestsign.VerifyConfig{ExternalURL: ts.URL + "/native", Replay: replay, Keys: func() (map[string]requestsign.Key, error) {
		return map[string]requestsign.Key{"reader": {ID: "reader", Secret: secret}}, nil
	}})
	require.NoError(t, err)
	handler = verifier.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/native/api/v1/version":
			w.Header().Set("X-Agentsview-Recall-Queries", "non-recording")
			_, _ = w.Write([]byte(`{"api_version":4,"read_only":true}`))
		case "/native/api/v1/recall/query":
			_, _ = w.Write([]byte(`{"mode":"lexical","recall_entries":[],"session_matches":[]}`))
		default:
			assert.Failf(t, "unexpected request", "path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}), nil, nil)
	t.Setenv("AGENTSVIEW_SIGNING_URL", ts.URL+"/native")
	t.Setenv("AGENTSVIEW_SIGNING_KEY_ID", "reader")
	t.Setenv("AGENTSVIEW_SIGNING_KEY_FILE", keyFile)
	api, err := apiclient.NewHTTPClient(ts.URL+"/native", "test", ts.Client())
	require.NoError(t, err)
	caps, err := probeHTTPServerCapabilities(t.Context(), api)
	require.NoError(t, err)
	require.True(t, caps.ReadOnly)
	backend := NewHTTPBackendForServer(ts.URL+"/native", "test", caps).(*httpBackend)
	backend.client = ts.Client()
	backend.longRunningClient = ts.Client()
	assert.True(t, service.SupportsRecallQueries(backend))
	_, err = backend.QueryRecallEntries(t.Context(), service.RecallQuery{Query: "sample", Mode: "lexical"})
	require.NoError(t, err)
	_, err = backend.Sync(t.Context(), service.SyncInput{})
	require.Error(t, err, "read-only permission must remain independent of recall capability")
}
