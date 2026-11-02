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
	gitlog "go.kenn.io/agentsview/internal/db/git"
	"go.kenn.io/agentsview/internal/server"
)

func TestCommitLinksRoutes(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "project-a", 0)
	w := te.post(t, "/api/v1/commits/sessions", `{"shas":["abc1234"]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
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

func TestCommitLinksRoutesTrailer(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "trailer-session", "project-a", 0)
	require.NoError(t, te.db.InsertMessages(t.Context(), []db.Message{{SessionID: "trailer-session", Ordinal: 0, Role: "user", Content: "Tidy docs", Timestamp: "2026-10-30T10:00:00Z"}}))
	sha := "abcdef10000000000000000000000000000000000"
	require.NoError(t, te.db.StoreTrailerCommitLinks(t.Context(), "repo", []gitlog.TrailerCommit{{SHA: sha, Branch: "main", Subject: "Editor", CommittedAt: "2026-10-30T10:10:00Z", Sessions: []string{"claude:trailer-session"}}}))
	w := te.get(t, "/api/v1/sessions/trailer-session/commits")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	list := decode[db.SessionCommitList](t, w)
	require.Len(t, list.Commits, 1)
	assert.Equal(t, "trailer", list.Commits[0].Source)
	assert.Equal(t, sha, list.Commits[0].SHA)
	require.NotNil(t, list.Commits[0].Turn)
	assert.True(t, list.Commits[0].Turn.Heuristic)
	assert.Equal(t, "Tidy docs", list.Commits[0].Turn.Text)
	w = te.post(t, "/api/v1/commits/sessions", `{"shas":["abcdef1"]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	matches := decode[db.CommitSessionList](t, w)
	require.Len(t, matches.Commits, 1)
	assert.Equal(t, list.Commits, matches.Commits[0].Links)
}
