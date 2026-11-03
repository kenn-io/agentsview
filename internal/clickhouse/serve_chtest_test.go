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

func TestServeSessions(t *testing.T) {
	t.Parallel()
	store, _, _ := clickhouse.TestingNewPushedStore(t)
	cfg := config.Config{Host: "127.0.0.1", DataDir: t.TempDir()}
	handler := server.New(cfg, store, nil, server.WithVersion(server.VersionInfo{ReadOnly: true})).Handler()

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions?include_one_shot=true", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var page db.SessionPage
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
	ids := make([]string, len(page.Sessions))
	for i, sess := range page.Sessions {
		ids[i] = sess.ID
	}
	assert.Contains(t, ids, clickhouse.TestingAlphaID)
	assert.Contains(t, ids, clickhouse.TestingBetaID)
}
