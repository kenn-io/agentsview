//go:build pgtest

package postgres_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/server"
)

func TestMachineAliasesOnPostgresHTTP(t *testing.T) {
	pgURL := os.Getenv("TEST_PG_URL")
	if pgURL == "" {
		t.Skip("TEST_PG_URL must point to a dedicated test database")
	}
	_, dataDir := newPGE2ETestDatabase(t)
	local := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, local, "current", "project", func(s *db.Session) {
		s.Machine = "old-owner"
		s.UserMessageCount = 3
		s.MessageCount = 5
	})
	require.NoError(t, local.SetSyncState("artifact_local_machine_name", "old-owner"))
	const schema = pgE2ESchema
	store, err := postgres.NewStore(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	syncer, err := postgres.New(pgURL, schema, local, "old-owner", true, postgres.SyncOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	require.NoError(t, syncer.EnsureSchema(t.Context()))
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	require.NoError(t, local.EnsureInstallationIdentity(t.Context(), "installation-a"))
	upgraded, err := postgres.New(pgURL, schema, local, "installation-a", true, postgres.SyncOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, upgraded.Close()) })
	result, err := upgraded.Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.SessionsPushed, "existing history is republished by an incremental push")
	cfg := config.Config{Host: "127.0.0.1", DataDir: dataDir, InstallationID: "server-installation"}
	handler := server.New(cfg, store, nil).Handler()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions?machine=old-owner", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var page db.SessionPage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "current", page.Sessions[0].ID)
	assert.Equal(t, "installation-a", page.Sessions[0].Machine)
	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/machines", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response struct {
		Aliases map[string]string `json:"machine_aliases"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.Equal(t, map[string]string{"old-owner": "installation-a"}, response.Aliases)
}
