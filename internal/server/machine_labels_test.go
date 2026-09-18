package server_test

import (
	"net/http"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/duckdb"
	"go.kenn.io/agentsview/internal/server"
)

func TestMachinesExposeLabelsWithoutChangingFilterKeys(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const id = "0123456789abcdef0123456789abcdef"
	te := setup(t, func(c *config.Config) { c.InstallationID = id })
	_, err := te.db.EnsureInstallationIdentity(t.Context(), id)
	require.NoError(err)
	te.seedSession(t, "current", "project", 5, func(s *db.Session) {
		s.Machine = id
		s.UserMessageCount = 3
	})
	te.seedSession(t, "historical", "project", 5, func(s *db.Session) {
		s.Machine = "old-host"
		s.UserMessageCount = 3
	})
	require.NoError(te.db.SetSyncState("machine_label:"+id, "Laptop"))
	require.NoError(te.db.SetSyncState("machine_alias:old-owner", id))
	w := te.get(t, "/api/v1/machines")
	assertStatus(t, w, http.StatusOK)
	resp := decode[struct {
		Machines []string          `json:"machines"`
		Labels   map[string]string `json:"machine_labels"`
		Aliases  map[string]string `json:"machine_aliases"`
	}](t, w)
	assert.ElementsMatch([]string{id, "old-host"}, resp.Machines)
	assert.Equal(map[string]string{id: "Laptop"}, resp.Labels)
	assert.Equal(map[string]string{"old-owner": id, "local": id}, resp.Aliases)
	for _, machine := range []string{id, "old-owner", "local", "old-owner," + id} {
		for _, path := range []string{"/api/v1/sessions", "/api/v1/sessions/sidebar-index"} {
			w = te.get(t, path+"?machine="+machine)
			assertStatus(t, w, http.StatusOK)
			page := decode[db.SessionPage](t, w)
			require.Len(page.Sessions, 1)
			assert.Equal("current", page.Sessions[0].ID)
		}
		w = te.get(t, "/api/v1/analytics/summary?from="+tsSeed[:10]+"&to="+tsSeed[:10]+"&machine="+machine)
		assertStatus(t, w, http.StatusOK)
		assert.Equal(1, decode[db.AnalyticsSummary](t, w).TotalSessions)
		w = te.get(t, "/api/v1/activity/report?preset=day&date="+tsSeed[:10]+"&timezone=UTC&machine="+machine)
		assertStatus(t, w, http.StatusOK)
		assert.Equal(1, decode[activity.Report](t, w).Totals.Sessions)
	}
	w = te.get(t, "/api/v1/sessions?machine=Laptop")
	assertStatus(t, w, http.StatusOK)
	assert.Empty(decode[db.SessionPage](t, w).Sessions)
	w = te.upload(t, "imported.jsonl", claudeTranscriptWithMessageCount(4), "project=project&machine=old-owner")
	assertStatus(t, w, http.StatusOK)
	assert.Equal("old-owner", decode[struct {
		Machine string `json:"machine"`
	}](t, w).Machine)
}

func TestMachineAliasesOnDuckDB(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	if runtime.GOOS == "windows" && runtime.GOARCH == "arm64" {
		t.Skip("duckdb-go-bindings does not ship a windows/arm64 library")
	}
	te := setup(t)
	te.seedSession(t, "current", "project", 5, func(s *db.Session) {
		s.Machine = "installation-a"
		s.UserMessageCount = 3
	})
	require.NoError(te.db.SetSyncState("machine_alias:old-owner", "installation-a"))
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err := duckdb.Push(t.Context(), path, te.db, "installation-a", duckdb.SyncOptions{}, true, nil)
	require.NoError(err)
	store, err := duckdb.NewStore(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(store.Close()) })
	cfg := config.Config{Host: "127.0.0.1", InstallationID: "server-installation"}
	te.handler = wrapTestHandler(cfg, server.New(cfg, store, nil).Handler())
	w := te.get(t, "/api/v1/sessions?machine=old-owner")
	assertStatus(t, w, http.StatusOK)
	page := decode[db.SessionPage](t, w)
	require.Len(page.Sessions, 1)
	assert.Equal("current", page.Sessions[0].ID)
	w = te.get(t, "/api/v1/machines")
	assertStatus(t, w, http.StatusOK)
	assert.Equal(map[string]string{"old-owner": "installation-a"}, decode[struct {
		Aliases map[string]string `json:"machine_aliases"`
	}](t, w).Aliases)
}
