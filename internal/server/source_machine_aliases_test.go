package server

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
)

func TestRemoteSyncUsesRecordedSourceMachineAliases(t *testing.T) {
	srv, handler, sessionPath := newRemoteSyncServer(t)
	localRoot := filepath.Dir(sessionPath)
	foreignRoot := filepath.Join(localRoot, "foreign")
	require.NoError(t, os.Mkdir(foreignRoot, 0o755))
	srv.cfg.InstallationID = "installation-a"
	srv.cfg.LocalMachineName = "foreign-host"
	srv.cfg.AgentDirs[parser.AgentClaude] = append(srv.cfg.AgentDirs[parser.AgentClaude], foreignRoot)
	srv.cfg.SourceMachines = map[parser.AgentType]map[string]string{
		parser.AgentClaude: {localRoot: "old-host", foreignRoot: "foreign-host"},
	}
	database := srv.db.(*db.DB)
	require.NoError(t, database.SetSyncState(db.MachineAliasKeyPrefix+"old-host", "installation-a"))
	readOnly, err := db.OpenReadOnly(database.Path())
	require.NoError(t, err)
	defer readOnly.Close()
	srv.db = readOnly

	get := httptest.NewRequest(http.MethodGet, "/api/v1/remote-sync/targets", nil)
	get.Header.Set("Authorization", "Bearer remote-token")
	getW := httptest.NewRecorder()
	handler.ServeHTTP(getW, get)
	require.Equal(t, http.StatusOK, getW.Code, getW.Body.String())
	var targets remotesync.TargetSet
	require.NoError(t, json.Unmarshal(getW.Body.Bytes(), &targets))
	assert.Equal(t, []string{localRoot}, targets.Dirs[parser.AgentClaude])
	assert.Contains(t, targets.ForbiddenRoots, foreignRoot,
		"a display label alone must not make a foreign source local")

	requested := remotesync.TargetSet{Dirs: map[parser.AgentType][]string{
		parser.AgentClaude: {localRoot},
	}}
	for route, body := range map[string]any{
		"manifest": requested,
		"archive":  remotesync.ArchiveRequest{TargetSet: requested},
	} {
		t.Run(route, func(t *testing.T) {
			payload, err := json.Marshal(body)
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/"+route, bytes.NewReader(payload))
			req.Header.Set("Authorization", "Bearer remote-token")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		})
	}
}
