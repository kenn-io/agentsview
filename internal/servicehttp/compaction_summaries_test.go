package servicehttp_test

import (
	"net"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func TestCompactionFiltersOverHTTP(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "b", "project")
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "b", Ordinal: 0, Role: "assistant", IsSystem: true, IsCompactBoundary: true, Content: "Retained summary"},
		{SessionID: "b", Ordinal: 1, Role: "user", Content: "ordinary"},
	}))
	for _, row := range []struct{ id, parent, ended string }{
		{"active-parent", "", "2024-06-15T00:00:00Z"},
		{"stale-child", "active-parent", "2024-01-01T00:00:00Z"},
		{"stale-parent", "", "2024-01-01T00:00:00Z"},
		{"active-child", "stale-parent", "2024-06-15T00:00:00Z"},
	} {
		dbtest.SeedSession(t, d, row.id, "project", func(s *db.Session) {
			s.EndedAt = &row.ended
			if row.parent != "" {
				s.ParentSessionID = &row.parent
				s.RelationshipType = "subagent"
			}
		})
	}
	daemon := httptest.NewUnstartedServer(nil)
	s := server.New(config.Config{Host: "127.0.0.1", Port: daemon.Listener.Addr().(*net.TCPAddr).Port, DataDir: t.TempDir()}, d, nil)
	daemon.Config.Handler = s.Handler()
	daemon.Start()
	defer daemon.Close()
	svc := servicehttp.NewHTTPBackend(daemon.URL, "", true, "")
	t.Run("compact boundary", func(t *testing.T) {
		out, err := svc.Messages(t.Context(), "b", service.MessageFilter{CompactBoundaryOnly: true})
		require.NoError(t, err)
		require.Len(t, out.Messages, 1)
		assert.Equal(t, "Retained summary", out.Messages[0].Content)
	})
	t.Run("flat", func(t *testing.T) {
		filter := service.ListFilter{ActiveSince: "2024-06-01T00:00:00Z", Limit: 20, IncludeOneShot: true, OneShotFilterSet: true, IncludeAutomated: true, AutomatedFilterSet: true, IncludeChildren: true, Flat: true}
		page, err := svc.List(t.Context(), filter)
		require.NoError(t, err)
		var ids []string
		for _, session := range page.Sessions {
			ids = append(ids, session.ID)
		}
		assert.Contains(t, ids, "active-child")
		assert.NotContains(t, ids, "stale-child")
	})
}
