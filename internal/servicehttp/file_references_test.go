package servicehttp_test

import (
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
		assert.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", r.URL.Query().Get("sha256"))
		_, _ = w.Write([]byte(`{"items":[{"version":{"id":"12345678-1234-1234-1234-123456789abc","node_id":7},"node":{"id":7,"name":"image.png"},"path":"/images/image.png","is_current":true}],"total":1}`))
	}))
	defer stub.Close()
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "images", "p1")
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
	assert.Equal(t, "found", occurrence.State)
	require.Len(t, occurrence.Matches, 1)
	assert.Equal(t, "https://example.com/#node=7&version=12345678-1234-1234-1234-123456789abc", occurrence.Matches[0].OpenURL)
	_, err = service.FileReferences(t.Context(), svc, "missing")
	assert.ErrorIs(t, err, service.ErrSessionNotFound)
}
