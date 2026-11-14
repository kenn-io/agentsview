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

func TestCompactionSummariesEvenerPlaceholders(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	for _, row := range []struct{ id, ended string }{{"a", "2024-06-18T00:00:00Z"}, {"b", "2024-06-17T00:00:00Z"}, {"c", "2024-06-16T00:00:00Z"}} {
		dbtest.SeedSession(t, d, row.id, "project", func(s *db.Session) { s.Agent = "evener"; s.EndedAt = &row.ended })
	}
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "a", Ordinal: 0, Role: "system", IsCompactBoundary: true, SourceSubtype: "CHECKPOINT", Content: "[CHECKPOINT]"},
		{SessionID: "a", Ordinal: 1, Role: "system", IsCompactBoundary: true, SourceSubtype: "SUMMARY", Content: "[SUMMARY]"},
		{SessionID: "b", Ordinal: 0, Role: "system", IsCompactBoundary: true, SourceSubtype: "CHECKPOINT", Content: "Retained summary"},
		{SessionID: "c", Ordinal: 0, Role: "system", IsCompactBoundary: true, SourceSubtype: "CHECKPOINT", Content: "[SUMMARY]"},
		{SessionID: "c", Ordinal: 1, Role: "system", IsCompactBoundary: true, SourceSubtype: "SUMMARY", Content: "[CHECKPOINT]"},
	}))
	daemon := httptest.NewUnstartedServer(nil)
	s := server.New(config.Config{Host: "127.0.0.1", Port: daemon.Listener.Addr().(*net.TCPAddr).Port, DataDir: t.TempDir()}, d, nil)
	daemon.Config.Handler = s.Handler()
	daemon.Start()
	defer daemon.Close()
	for _, svc := range []service.SessionService{service.NewDirectBackend(d, nil), servicehttp.NewHTTPBackend(daemon.URL, "", true, "")} {
		for _, tc := range []struct {
			id       string
			contents []string
		}{{"a", nil}, {"b", []string{"Retained summary"}}, {"c", []string{"[SUMMARY]", "[CHECKPOINT]"}}, {"missing", nil}} {
			out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{SessionID: tc.id})
			require.NoError(t, err)
			var contents []string
			for _, summary := range out.Summaries {
				contents = append(contents, summary.Content)
			}
			assert.Equal(t, tc.contents, contents)
		}
		first, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z", Limit: 1})
		require.NoError(t, err)
		require.Len(t, first.Summaries, 1)
		assert.Equal(t, "b", first.Summaries[0].SessionID)
		require.NotEmpty(t, first.NextCursor)
		second, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z", Limit: 1, Cursor: first.NextCursor})
		require.NoError(t, err)
		require.Len(t, second.Summaries, 2)
		assert.Equal(t, "c", second.Summaries[0].SessionID)
		assert.Empty(t, second.NextCursor)
		_, err = service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{SessionID: "b", Limit: -1})
		require.ErrorContains(t, err, "limit must be 0 or more")
	}
}
