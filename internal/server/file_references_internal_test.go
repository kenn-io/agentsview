package server

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestVersionReportsDocbankConfiguration(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		cfg := config.Config{Host: "127.0.0.1", DataDir: t.TempDir()}
		if enabled {
			cfg.Integrations.Docbank = config.DocbankIntegration{URL: "https://example.com", APIKeyEnv: "DOCBANK_TEST_KEY"}
		}
		s := New(cfg, dbtest.OpenTestDB(t), nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:0/api/v1/version", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var version VersionInfo
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &version))
		assert.Equal(t, enabled, version.DocbankEnabled)
	}
}

type unavailableFileReferences struct{ service.SessionService }

func (unavailableFileReferences) FileReferences(context.Context, string) (service.FileReferencesResult, error) {
	return service.FileReferencesResult{}, service.ErrRevisionBoundReadUnavailable
}

func TestFileReferencesRevisionUnavailable(t *testing.T) {
	s := testServer(t, 30*time.Second)
	s.sessions = unavailableFileReferences{s.sessions}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/example/file-references", nil))
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}
