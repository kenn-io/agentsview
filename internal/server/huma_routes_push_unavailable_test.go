//go:build !duckdb || (windows && arm64)

package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
)

func TestMirrorPushWithoutDriverRejectsBeforeSync(t *testing.T) {
	f := newSyncRouteFixture(t)
	f.writeClaudeSession(t, "project/session.jsonl", "do not sync an unavailable mirror")
	f.srv.cfg.DuckDB = config.DuckDBConfig{
		Path:        filepath.Join(t.TempDir(), "mirror.duckdb"),
		MachineName: "test-machine",
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"/api/v1/push/duckdb", strings.NewReader(`{"full":false}`))
	req.Host = "127.0.0.1:0"
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", "http://127.0.0.1:0")
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "DuckDB is not available in this agentsview build")
	count, err := f.db.FileBackedSessionCount(t.Context())
	require.NoError(t, err)
	assert.Zero(t, count, "an unavailable mirror must be rejected before local sync")
}
