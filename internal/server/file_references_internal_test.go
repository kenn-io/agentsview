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
)

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
