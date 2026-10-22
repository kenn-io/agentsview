package servicehttp_test

import (
	"encoding/json/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestFileReferencesRoundTrip(t *testing.T) {
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"version":{"id":"12345678-1234-1234-1234-123456789abc","node_id":7},"node":{"id":7,"name":"image.png"},"path":"/images/image.png","is_current":true}],"total":1}`))
	}))
	defer stub.Close()
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "images", "p1")
	dbtest.SeedSession(t, d, "empty", "p1")
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{{SessionID: "images", Ordinal: 0, Role: "user", Content: "![pasted](asset://ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad.png)"}}))
	daemon := httptest.NewUnstartedServer(nil)
	s := server.New(config.Config{Host: "127.0.0.1", Port: daemon.Listener.Addr().(*net.TCPAddr).Port, DataDir: t.TempDir(), Integrations: config.IntegrationsConfig{Docbank: config.DocbankIntegration{URL: stub.URL, WebURL: "https://example.com", APIKeyEnv: "DOCBANK_TEST_KEY"}}}, d, nil)
	daemon.Config.Handler = s.Handler()
	daemon.Start()
	defer daemon.Close()
	svc := servicehttp.NewHTTPBackend(daemon.URL, "", true, "")
	out, err := service.FileReferences(t.Context(), svc, "images")
	require.NoError(t, err)
	require.Len(t, out.Occurrences, 1)
	occurrence := out.Occurrences[0]
	assert.Nil(t, occurrence.ToolUseID)
	assert.Nil(t, occurrence.EventIndex)
	assert.Nil(t, occurrence.Size)
	assert.Nil(t, occurrence.CallIndex)
	assert.Nil(t, occurrence.ToolName)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, daemon.URL+"/api/v1/sessions/empty/file-references", nil)
	require.NoError(t, err)
	response, err := daemon.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var body map[string]any
	require.NoError(t, json.UnmarshalRead(response.Body, &body))
	assert.Equal(t, []any{}, body["occurrences"])
	_, err = service.FileReferences(t.Context(), svc, "missing")
	assert.ErrorIs(t, err, service.ErrSessionNotFound)
}

func TestFileReferencesDiscoveredCapability(t *testing.T) {
	t.Setenv("AGENTSVIEW_SIGNING_URL", "https://reader.example.com")
	t.Setenv("AGENTSVIEW_SIGNING_KEY_ID", "reader")
	t.Setenv("AGENTSVIEW_SIGNING_KEY_FILE", "unused-key")
	for _, tc := range []struct {
		name, metadata string
		enabled        bool
		signed         bool
	}{
		{"enabled", `{"api_version":12,"docbank_enabled":true}`, true, false},
		{"signed", `{"api_version":12,"docbank_enabled":true}`, false, true},
		{"disabled", `{"api_version":12,"docbank_enabled":false}`, false, false},
		{"older daemon", `{"api_version":12}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v1/version", r.URL.Path)
				_, _ = w.Write([]byte(tc.metadata))
			}))
			defer daemon.Close()
			caps, err := servicehttp.ProbeHTTPServerCapabilities(t.Context(), daemon.URL, "")
			require.NoError(t, err)
			url := daemon.URL
			if tc.signed {
				url = "https://reader.example.com"
			}
			svc := servicehttp.NewHTTPBackendForServer(url, "", caps)
			assert.Equal(t, tc.enabled, service.SupportsFileReferences(svc))
		})
	}
}
