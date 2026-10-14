package apiclient

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/requestsign"
)

func TestNativeHTTPAndRawClientsSignFromEnvironment(t *testing.T) {
	var verifier *requestsign.Verifier
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		verifier.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"api_version":4,"version":"test","read_only":true}`))
		}), nil, nil).ServeHTTP(w, r)
	}))
	defer ts.Close()
	secret := make([]byte, 64)
	_, err := rand.Read(secret)
	require.NoError(t, err)
	dir := t.TempDir()
	file := filepath.Join(dir, "key")
	require.NoError(t, os.WriteFile(file, []byte(base64.StdEncoding.EncodeToString(secret)), 0o600))
	state := filepath.Join(dir, "replay.db")
	require.NoError(t, requestsign.InitReplay(state))
	replay, err := requestsign.OpenReplay(state)
	require.NoError(t, err)
	defer replay.Close()
	verifier, err = requestsign.NewVerifier(requestsign.VerifyConfig{ExternalURL: ts.URL + "/native", Replay: replay, Keys: func() (map[string]requestsign.Key, error) {
		return map[string]requestsign.Key{"native-key": {ID: "native-key", Secret: secret}}, nil
	}})
	require.NoError(t, err)
	t.Setenv("AGENTSVIEW_SIGNING_URL", ts.URL+"/native")
	t.Setenv("AGENTSVIEW_SIGNING_KEY_ID", "native-key")
	t.Setenv("AGENTSVIEW_SIGNING_KEY_FILE", file)
	client, err := NewHTTPClient(ts.URL+"/native", "test-token", ts.Client())
	require.NoError(t, err)
	response, err := client.GetAPIV1VersionWithResponse(t.Context())
	require.NoError(t, err)
	require.NotNil(t, response)
	assert.Equal(t, 200, response.StatusCode)
	raw, err := RawRequest(ts.URL+"/native", ts.Client(), func(api *Client) error {
		_, err := api.GetAPIV1VersionWithResponse(t.Context())
		return err
	}, func(_ context.Context, r *http.Request) error {
		r.Header.Set("Authorization", "Bearer test-token")
		return nil
	})
	require.NoError(t, err)
	require.NotNil(t, raw)
	require.NoError(t, raw.Body.Close())
	assert.Equal(t, 200, raw.StatusCode)
}
