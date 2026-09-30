//go:build chtest

package clickhouse_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
)

func TestToolSequencesHTTPParity(t *testing.T) {
	local := dbtest.OpenTestDB(t)
	sessionIDs := dbtest.SeedToolSequencesParity(t, local)
	localHandler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "local"}, local, nil).Handler()

	dsn, database := chtest.FreshDatabase(t)
	target := clickhouse.Target{URL: dsn, Database: database}
	syncer, err := clickhouse.New(t.Context(), target, local, "tool-sequences-parity-host", storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)

	store, err := clickhouse.NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	remoteHandler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "remote"}, store, nil).Handler()
	for _, sessionID := range sessionIDs {
		localJSON := getToolSequencesDocument(t, localHandler, sessionID)
		remoteJSON := getToolSequencesDocument(t, remoteHandler, sessionID)
		assert.Equal(t, localJSON, remoteJSON, sessionID)
	}
}

func getToolSequencesDocument(t *testing.T, handler http.Handler, sessionID string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/"+sessionID+"/tool-sequences", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	var document map[string]any
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &document))
	return document
}
