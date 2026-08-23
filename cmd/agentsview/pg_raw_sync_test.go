package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/server"
)

func TestPreparePGRawSyncServicesRegistersHostedRoutes(t *testing.T) {
	t.Parallel()

	option, cleanup, err := preparePGRawSyncServices(
		t.Context(), t.TempDir(), new(sql.DB),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanup()) })

	srv := server.New(config.Config{
		Host:         "127.0.0.1",
		Port:         8080,
		WriteTimeout: time.Second,
	}, nil, nil, option)
	req := httptest.NewRequest(http.MethodGet, "/api/openapi.json", nil)
	req.Host = "127.0.0.1:8080"
	recorder := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var spec struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &spec))
	for _, path := range []string{
		"/api/v1/raw-sync/tokens",
		"/api/v1/raw-sync/objects/missing",
		"/api/v1/raw-sync/manifests",
	} {
		assert.Contains(t, spec.Paths, path)
	}
}

func TestPreparePGRawSyncServicesSkipsReadOnlySchema(t *testing.T) {
	t.Parallel()

	option, cleanup, err := preparePGRawSyncServicesIfWritable(
		t.Context(), "", nil, false,
	)

	require.NoError(t, err)
	assert.Nil(t, option)
	assert.Nil(t, cleanup)
}

func TestPreparePGRawSyncServicesDoesNotOpenArtifactSyncRepository(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	repositoryPath := filepath.Join(dataDir, "artifacts")
	require.NoError(t, os.WriteFile(repositoryPath, []byte("occupied"), 0o600))

	_, cleanup, err := preparePGRawSyncServices(
		t.Context(), dataDir, new(sql.DB),
	)
	require.NoError(t, err)
	require.NoError(t, cleanup())
}

func TestPreparePGRawSyncServicesDefersRawRepositoryOpen(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	repositoryParent := filepath.Join(dataDir, pgRawSyncDataDirectory)
	require.NoError(t, os.WriteFile(repositoryParent, []byte("occupied"), 0o600))

	_, cleanup, err := preparePGRawSyncServices(
		t.Context(), dataDir, new(sql.DB),
	)
	require.NoError(t, err)
	require.NoError(t, cleanup())
}
