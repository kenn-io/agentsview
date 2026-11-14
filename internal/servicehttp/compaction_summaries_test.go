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

func TestCompactionSummariesRoundTrip(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "session", "project")
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "session", Ordinal: 0, Role: "user", Content: "hello"},
		{SessionID: "session", Ordinal: 1, Role: "assistant", IsSystem: true, IsCompactBoundary: true, Content: "summary"},
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
	anchor, after := 0, 1
	window, err := svc.Messages(t.Context(), "session", service.MessageFilter{Around: &anchor, After: &after, CompactBoundaryOnly: true})
	require.NoError(t, err)
	require.Len(t, window.Messages, 1)
	assert.Equal(t, 1, window.Messages[0].Ordinal)
}
