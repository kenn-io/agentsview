package server_test

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
)

func TestCommitLinksRoutes(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "project-a", 0)
	require.NoError(t, te.db.InsertMessages(t.Context(), []db.Message{
		{SessionID: "s1", Ordinal: 0, Role: "user", Content: "Fix parser"},
		{SessionID: "s1", Ordinal: 1, Role: "assistant", ToolCalls: []db.ToolCall{{SessionID: "s1", ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"git commit -m fix"}`, ResultContent: "[main abc1234] Fix parser"}}},
	}))
	w := te.post(t, "/api/v1/commits/sessions", `{"shas":["deadbee","abc12340000000000000000000000000000000000","abc1234"]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decode[db.CommitSessionList](t, w)
	require.Len(t, got.Commits, 3)
	assert.Equal(t, "deadbee", got.Commits[0].SHA)
	assert.Empty(t, got.Commits[0].Links)
	assert.Equal(t, "abc12340000000000000000000000000000000000", got.Commits[1].SHA)
	require.Len(t, got.Commits[1].Links, 1)
	assert.Equal(t, "s1", got.Commits[1].Links[0].SessionID)
	require.NotNil(t, got.Commits[1].Links[0].Turn)
	assert.Equal(t, "Fix parser", got.Commits[1].Links[0].Turn.Text)
	assert.Equal(t, "abc1234", got.Commits[2].SHA)
	w = te.get(t, "/api/v1/sessions/s1/commits")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	session := decode[db.SessionCommitList](t, w)
	require.Len(t, session.Commits, 1)
	assert.Equal(t, "abc1234", session.Commits[0].SHA)
	w = te.post(t, "/api/v1/commits/sessions", `{"shas":["bad"]}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	for _, shas := range [][]string{{}, strings.Split(strings.TrimSuffix(strings.Repeat("abc1234,", 201), ","), ",")} {
		body, err := json.Marshal(map[string]any{"shas": shas})
		require.NoError(t, err)
		w = te.post(t, "/api/v1/commits/sessions", string(body))
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	}
}

func TestCommitLinksRoutesUnavailable(t *testing.T) {
	te := setup(t)
	cfg := config.Config{Host: "127.0.0.1"}
	te.handler = wrapTestHandler(cfg, server.New(cfg, readOnlyTestStore{Store: te.db}, nil).Handler())
	w := te.get(t, "/api/v1/sessions/s1/commits")
	assert.Equal(t, http.StatusNotImplemented, w.Code, w.Body.String())
	w = te.post(t, "/api/v1/commits/sessions", `{"shas":["abc1234"]}`)
	assert.Equal(t, http.StatusNotImplemented, w.Code, w.Body.String())
}
