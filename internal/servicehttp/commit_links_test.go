package servicehttp

import (
	"net"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
)

func TestHTTPCommitLinks(t *testing.T) {
	d, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	const id = "codex:session-a"
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: id, Agent: "codex", Project: "project-a"}))
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "Fix parser"},
		{SessionID: id, Ordinal: 1, Role: "assistant", ToolCalls: []db.ToolCall{{SessionID: id, ToolName: "Bash", Category: "Bash", ToolUseID: "commit-call", InputJSON: `{"command":"git commit -m fix"}`, ResultContent: "[main abc1234] Fix parser", ResultEvents: []db.ToolResultEvent{{Source: "tool_result", Content: "[main abc1234] Fix parser"}}}}},
	}))
	srv := httptest.NewUnstartedServer(nil)
	t.Cleanup(srv.Close)
	srv.Config.Handler = server.New(config.Config{Host: "127.0.0.1", Port: srv.Listener.Addr().(*net.TCPAddr).Port}, d, nil).Handler()
	srv.Start()
	client := NewHTTPBackend(srv.URL, "", false, "")
	list, err := client.SessionCommits(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, list.Commits, 1)
	link := list.Commits[0]
	assert.Equal(t, id, link.SessionID)
	assert.Equal(t, "abc1234", link.SHA)
	assert.Equal(t, 1, link.MessageOrdinal)
	assert.Equal(t, "commit-call", link.ToolUseID)
	require.NotNil(t, link.Turn)
	assert.Equal(t, "Fix parser", link.Turn.Text)
	matches, err := client.CommitSessions(t.Context(), []string{"deadbee", "abc1234000000000000000000000000000000000"})
	require.NoError(t, err)
	require.Len(t, matches.Commits, 2)
	assert.Equal(t, "deadbee", matches.Commits[0].SHA)
	assert.Empty(t, matches.Commits[0].Links)
	assert.Equal(t, "abc1234000000000000000000000000000000000", matches.Commits[1].SHA)
	assert.Equal(t, []db.CommitLink{link}, matches.Commits[1].Links)
}
