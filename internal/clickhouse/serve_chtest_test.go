//go:build chtest

package clickhouse_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
)

func TestServeSessionsMessagesAndSearch(t *testing.T) {
	store, _, _ := clickhouse.TestingNewPushedStore(t)
	cfg := config.Config{Host: "127.0.0.1", DataDir: t.TempDir()}
	handler := server.New(cfg, store, nil, server.WithVersion(server.VersionInfo{ReadOnly: true})).Handler()

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0"+path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	w := get("/api/v1/sessions?include_one_shot=true")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var page db.SessionPage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	ids := make([]string, len(page.Sessions))
	for i, sess := range page.Sessions {
		ids[i] = sess.ID
	}
	assert.Contains(t, ids, clickhouse.TestingAlphaID)
	assert.Contains(t, ids, clickhouse.TestingBetaID)

	w = get("/api/v1/sessions/" + clickhouse.TestingAlphaID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var sess db.Session
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &sess))
	assert.Equal(t, clickhouse.TestingAlphaID, sess.ID)
	assert.Equal(t, "alpha", sess.Project)

	w = get("/api/v1/sessions/" + clickhouse.TestingAlphaID + "/messages")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var msgs struct {
		Messages []db.Message `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msgs))
	require.Len(t, msgs.Messages, 2)
	assert.Equal(t, "alpha first", msgs.Messages[0].Content)
	require.NotEmpty(t, msgs.Messages[1].ToolCalls)
	assert.Equal(t, "clickhouse result", msgs.Messages[1].ToolCalls[0].ResultContent)

	w = get("/api/v1/search?q=secret+token")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var search struct {
		Results []db.SearchResult `json:"results"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &search))
	require.NotEmpty(t, search.Results)
	assert.Equal(t, clickhouse.TestingAlphaID, search.Results[0].SessionID)

	w = get("/api/v1/settings")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var settings struct {
		ReadOnly bool `json:"read_only"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &settings))
	assert.True(t, settings.ReadOnly)
}
