package servicehttp_test

import (
	"encoding/json"
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

func TestCompactionSummariesLegacyEvenerOverHTTP(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "b", "project")
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "b", Ordinal: 0, Role: "system", IsCompactBoundary: true, Content: "Retained summary"},
		{SessionID: "b", Ordinal: 1, Role: "user", Content: "ordinary"},
	}))
	daemon := httptest.NewUnstartedServer(nil)
	s := server.New(config.Config{Host: "127.0.0.1", Port: daemon.Listener.Addr().(*net.TCPAddr).Port, DataDir: t.TempDir()}, d, nil)
	daemon.Config.Handler = s.Handler()
	daemon.Start()
	defer daemon.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, daemon.URL+"/api/v1/sessions/b/messages?compact_boundary=true", nil)
	require.NoError(t, err)
	response, err := daemon.Client().Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	var raw struct {
		Messages []db.Message `json:"messages"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&raw))
	require.Len(t, raw.Messages, 1)
	assert.Equal(t, "Retained summary", raw.Messages[0].Content)
}

func TestCompactionSummariesActiveChildren(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	for _, row := range []struct{ id, parent, ended string }{
		{"active-parent", "", "2024-06-15T00:00:00Z"},
		{"stale-child", "active-parent", "2024-01-01T00:00:00Z"},
		{"stale-parent", "", "2024-01-01T00:00:00Z"},
		{"active-child", "stale-parent", "2024-06-15T00:00:00Z"},
		{"orphan", "missing", "2024-06-15T00:00:00Z"},
	} {
		dbtest.SeedSession(t, d, row.id, "project", func(s *db.Session) {
			s.EndedAt = &row.ended
			if row.parent != "" {
				s.ParentSessionID = &row.parent
				s.RelationshipType = "subagent"
			}
		})
		require.NoError(t, d.InsertMessages(t.Context(), []db.Message{{SessionID: row.id, Role: "assistant", Content: row.id, IsCompactBoundary: true}}))
	}
	daemon := httptest.NewUnstartedServer(nil)
	s := server.New(config.Config{Host: "127.0.0.1", Port: daemon.Listener.Addr().(*net.TCPAddr).Port, DataDir: t.TempDir()}, d, nil)
	daemon.Config.Handler = s.Handler()
	daemon.Start()
	defer daemon.Close()
	for _, svc := range []service.SessionService{service.NewDirectBackend(d, nil), servicehttp.NewHTTPBackend(daemon.URL, "", true, "")} {
		var ids []string
		cursor := ""
		for range 4 {
			out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z", Limit: 1, Cursor: cursor})
			require.NoError(t, err)
			for _, summary := range out.Summaries {
				ids = append(ids, summary.SessionID)
			}
			cursor = out.NextCursor
			if cursor == "" {
				break
			}
		}
		assert.Empty(t, cursor)
		assert.ElementsMatch(t, []string{"active-parent", "active-child", "orphan"}, ids)
	}
}
