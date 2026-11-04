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
	w := te.post(t, "/api/v1/commits/sessions", `{"shas":["abc1234"]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	commits := decode[db.CommitSessionList](t, w).Commits
	require.Len(t, commits, 1)
	assert.Equal(t, "abc1234", commits[0].SHA)
	w = te.get(t, "/api/v1/sessions/s1/commits")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	many := make([]string, 200)
	for i := range many {
		many[i] = "abc1234"
	}
	body, err := json.Marshal(map[string]any{"shas": many})
	require.NoError(t, err)
	w = te.post(t, "/api/v1/commits/sessions", string(body))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Len(t, decode[db.CommitSessionList](t, w).Commits, 200)

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
