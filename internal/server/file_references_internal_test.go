package server

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestFileReferencesEmptyAndParameterizedFilesAPI(t *testing.T) {
	t.Setenv("DOCBANK_TEST_KEY", "test-key")
	var requests atomic.Int32
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.Contains(t, []string{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"}, r.URL.Query().Get("sha256"))
		_, _ = w.Write([]byte(`{"items":[],"total":0}`))
	}))
	defer stub.Close()
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "files", "p1")
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{{SessionID: "files", Ordinal: 0, Role: "assistant", ToolCalls: []db.ToolCall{{ToolName: "Read", ResultContent: `[{"type":"file","uri":"data:text/plain;base64,"},{"type":"file","uri":"data:text/plain;charset=utf-8;base64,YWJj"}]`}}}}))
	s := New(config.Config{Host: "127.0.0.1", DataDir: t.TempDir(), Integrations: config.IntegrationsConfig{Docbank: config.DocbankIntegration{URL: stub.URL, APIKeyEnv: "DOCBANK_TEST_KEY"}}}, d, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/files/file-references", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var out service.FileReferencesResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, int32(2), requests.Load())
	require.Len(t, out.Occurrences, 2)
	assert.Equal(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", out.Occurrences[0].SHA256)
	assert.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", out.Occurrences[1].SHA256)
	for i, occurrence := range out.Occurrences {
		assert.Equal(t, i, occurrence.ImageIndex)
		assert.Equal(t, "text/plain", occurrence.MediaType)
		assert.Equal(t, "not_found", occurrence.State)
	}
}

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
