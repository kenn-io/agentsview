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

func TestCompactionSummariesEvenerPlaceholdersOverHTTP(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	for _, row := range []struct{ id, ended string }{{"a", "2024-06-18T00:00:00Z"}, {"b", "2024-06-17T00:00:00Z"}, {"c", "2024-06-16T00:00:00Z"}} {
		dbtest.SeedSession(t, d, row.id, "project", func(s *db.Session) { s.Agent = "evener"; s.EndedAt = &row.ended })
	}
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "a", Ordinal: 0, Role: "system", IsCompactBoundary: true, SourceType: "evener_empty_compaction", SourceSubtype: "CHECKPOINT", Content: "[CHECKPOINT]"},
		{SessionID: "a", Ordinal: 1, Role: "system", IsCompactBoundary: true, SourceType: "evener_empty_compaction", SourceSubtype: "SUMMARY", Content: "[SUMMARY]"},
		{SessionID: "b", Ordinal: 0, Role: "system", IsCompactBoundary: true, SourceSubtype: "CHECKPOINT", Content: "Retained summary"},
		{SessionID: "c", Ordinal: 0, Role: "system", IsCompactBoundary: true, SourceSubtype: "SUMMARY", Content: "[SUMMARY]"},
	}))
	daemon := httptest.NewUnstartedServer(nil)
	s := server.New(config.Config{Host: "127.0.0.1", Port: daemon.Listener.Addr().(*net.TCPAddr).Port, DataDir: t.TempDir()}, d, nil)
	daemon.Config.Handler = s.Handler()
	daemon.Start()
	defer daemon.Close()
	svc := servicehttp.NewHTTPBackend(daemon.URL, "", true, "")
	for id, want := range map[string][]string{"a": nil, "b": {"Retained summary"}, "c": {"[SUMMARY]"}, "missing": nil} {
		out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{SessionID: id})
		require.NoError(t, err)
		var contents []string
		for _, summary := range out.Summaries {
			contents = append(contents, summary.Content)
		}
		assert.Equal(t, want, contents, id)
	}
	first, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z", Limit: 1})
	require.NoError(t, err)
	require.Len(t, first.Summaries, 1)
	assert.Equal(t, "b", first.Summaries[0].SessionID)
	second, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z", Limit: 1, Cursor: first.NextCursor})
	require.NoError(t, err)
	require.Len(t, second.Summaries, 1)
	assert.Equal(t, "c", second.Summaries[0].SessionID)
	assert.Empty(t, second.NextCursor)
	response, err := daemon.Client().Get(daemon.URL + "/api/v1/sessions/b/messages?compact_boundary=true&around=0")
	require.NoError(t, err)
	defer response.Body.Close()
	assert.Equal(t, http.StatusBadRequest, response.StatusCode)
}
