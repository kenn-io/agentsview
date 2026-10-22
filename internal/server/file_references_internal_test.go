package server

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/integrations/docbank"
	"go.kenn.io/agentsview/internal/service"
)

func TestFileReferencesRoute(t *testing.T) {
	s := testServer(t, 30*time.Second)
	dbtest.SeedSession(t, s.db.(*db.DB), "image-session", "p1")
	s.sessions = service.NewReadOnlyBackend(s.db, service.WithDocbank(newTestDocbank(t, config.DocbankIntegration{URL: "http://127.0.0.1:1", APIKeyEnv: "DOCBANK_TEST_KEY"})))
	for _, tc := range []struct {
		id     string
		status int
	}{{"image-session", 200}, {"missing", 404}} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/"+tc.id+"/file-references", nil))
		require.Equal(t, tc.status, rec.Code, rec.Body.String())
		if tc.status == 200 {
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, "enabled", body["integration"])
			assert.Equal(t, []any{}, body["occurrences"])
		}
	}
}

func TestVersionReportsDocbankConfiguration(t *testing.T) {
	s := testServer(t, 30*time.Second)
	for _, enabled := range []bool{false, true} {
		s.cfg.Integrations.Docbank = config.DocbankIntegration{}
		if enabled {
			s.cfg.Integrations.Docbank = config.DocbankIntegration{URL: "https://example.com", APIKeyEnv: "DOCBANK_TEST_KEY"}
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/version", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var version VersionInfo
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &version))
		assert.Equal(t, enabled, version.DocbankEnabled)
	}
}

func newTestDocbank(t *testing.T, cfg config.DocbankIntegration) *docbank.Client {
	t.Helper()
	client, err := docbank.New(cfg)
	require.NoError(t, err)
	return client
}
