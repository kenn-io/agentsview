package servicehttp_test

import (
	"net"
	"net/http"
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

func TestCompactionSummariesRoundTrip(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "session", "project", func(s *db.Session) { s.EndedAt = new("2024-01-01T00:00:00Z") })
	dbtest.SeedSession(t, d, "old-parent", "project", func(s *db.Session) { s.EndedAt = new("2024-01-01T00:00:00Z") })
	dbtest.SeedSession(t, d, "recent-child", "project", func(s *db.Session) {
		s.EndedAt = new("2024-06-15T00:00:00Z")
		s.ParentSessionID = new("old-parent")
		s.RelationshipType = "subagent"
	})
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "session", Ordinal: 0, Role: "user", Content: "hello"},
		{SessionID: "session", Ordinal: 1, Role: "assistant", IsSystem: true, IsCompactBoundary: true, Content: "summary"},
		{SessionID: "recent-child", Ordinal: 0, Role: "assistant", IsSystem: true, IsCompactBoundary: true, Content: "child summary"},
	}))
	daemon := httptest.NewUnstartedServer(nil)
	s := server.New(config.Config{Host: "127.0.0.1", Port: daemon.Listener.Addr().(*net.TCPAddr).Port, DataDir: t.TempDir()}, d, nil)
	daemon.Config.Handler = s.Handler()
	daemon.Start()
	defer daemon.Close()
	svc := servicehttp.NewHTTPBackend(daemon.URL, "", true, "")
	out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{SessionID: "session"})
	require.NoError(t, err)
	require.Len(t, out.Summaries, 1)
	assert.Equal(t, "summary", out.Summaries[0].Content)
	active, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z"})
	require.NoError(t, err)
	require.Len(t, active.Summaries, 1)
	assert.Equal(t, "recent-child", active.Summaries[0].SessionID)
	anchor, after := 0, 1
	_, err = svc.Messages(t.Context(), "session", service.MessageFilter{Around: &anchor, After: &after, CompactBoundaryOnly: true})
	require.Error(t, err)
	response, err := daemon.Client().Get(daemon.URL + "/api/v1/sessions/session/messages?compact_boundary=true&around=0")
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusBadRequest, response.StatusCode)
}
