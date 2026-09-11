package server_test

import (
	"net/http"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/server"
)

func TestMachinesExposeLabelsWithoutChangingFilterKeys(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	te := setup(t, func(c *config.Config) { c.InstallationID = id })
	te.seedSession(t, "current", "project", 5, func(s *db.Session) {
		s.Machine = id
		s.UserMessageCount = 3
	})
	te.seedSession(t, "historical", "project", 5, func(s *db.Session) {
		s.Machine = "old-host"
		s.UserMessageCount = 3
	})
	require.NoError(t, te.db.SetSyncState("machine_label:"+id, "Laptop"))
	require.NoError(t, te.db.SetSyncState("machine_alias:old-owner", id))
	w := te.get(t, "/api/v1/machines")
	assertStatus(t, w, http.StatusOK)
	resp := decode[struct {
		Machines []string          `json:"machines"`
		Labels   map[string]string `json:"machine_labels"`
		Aliases  map[string]string `json:"machine_aliases"`
	}](t, w)
	assert.ElementsMatch(t, []string{id, "old-host"}, resp.Machines)
	assert.Equal(t, map[string]string{id: "Laptop"}, resp.Labels)
	assert.Equal(t, map[string]string{"old-owner": id, "local": id}, resp.Aliases)
	for _, machine := range []string{id, "old-owner", "local", "old-owner," + id} {
		w = te.get(t, "/api/v1/sessions?machine="+machine)
		assertStatus(t, w, http.StatusOK)
		page := decode[db.SessionPage](t, w)
		require.Len(t, page.Sessions, 1)
		assert.Equal(t, "current", page.Sessions[0].ID)
	}
	w = te.get(t, "/api/v1/sessions?machine=Laptop")
	assertStatus(t, w, http.StatusOK)
	assert.Empty(t, decode[db.SessionPage](t, w).Sessions)
	w = te.upload(t, "imported.jsonl", claudeTranscriptWithMessageCount(4), "project=project&machine=old-owner")
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, "old-owner", decode[struct {
		Machine string `json:"machine"`
	}](t, w).Machine)
}

func TestMachineAliasesOnDuckDB(t *testing.T) {
	if runtime.GOOS == "windows" && runtime.GOARCH == "arm64" {
		t.Skip("duckdb-go-bindings does not ship a windows/arm64 library")
	}
	te := setup(t)
	te.seedSession(t, "current", "project", 5, func(s *db.Session) {
		s.Machine = "installation-a"
		s.UserMessageCount = 3
	})
	require.NoError(t, te.db.SetSyncState("machine_alias:old-owner", "installation-a"))
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err := duckdb.Push(t.Context(), path, te.db, "installation-a", duckdb.SyncOptions{}, true, nil)
	require.NoError(t, err)
	store, err := duckdb.NewStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	cfg := config.Config{Host: "127.0.0.1", InstallationID: "server-installation"}
	te.handler = wrapTestHandler(cfg, server.New(cfg, store, nil).Handler())
	w := te.get(t, "/api/v1/sessions?machine=old-owner")
	assertStatus(t, w, http.StatusOK)
	page := decode[db.SessionPage](t, w)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "current", page.Sessions[0].ID)
	w = te.get(t, "/api/v1/machines")
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, map[string]string{"old-owner": "installation-a"}, decode[struct {
		Aliases map[string]string `json:"machine_aliases"`
	}](t, w).Aliases)
}
