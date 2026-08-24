package server_test

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestHumaIncludeDeletedQueryMappingForListAndContentSearch(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "huma-active", "proj", 2)
	te.seedSession(t, "huma-deleted", "proj", 2)
	te.seedMessages(t, "huma-active", 1, func(_ int, m *db.Message) {
		m.Content = "needle active"
	})
	te.seedMessages(t, "huma-deleted", 1, func(_ int, m *db.Message) {
		m.Content = "needle deleted"
	})
	conn, err := sql.Open("sqlite3", filepath.Join(te.dataDir, "test.db"))
	require.NoError(t, err)
	_, err = conn.Exec("UPDATE sessions SET deleted_at = ? WHERE id = ?",
		"2026-05-21T00:00:00Z", "huma-deleted")
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	defaultList := te.get(t, "/api/v1/sessions?limit=50")
	assert.Equal(t, http.StatusOK, defaultList.Code)
	assert.Contains(t, defaultList.Body.String(), "huma-active")
	assert.NotContains(t, defaultList.Body.String(), "huma-deleted")
	withDeletedList := te.get(t,
		"/api/v1/sessions?limit=50&include_deleted=true")
	assert.Equal(t, http.StatusOK, withDeletedList.Code)
	assert.Contains(t, withDeletedList.Body.String(), "huma-deleted")

	defaultSearch := te.get(t,
		"/api/v1/search/content?pattern=needle&in=messages&limit=50")
	assert.Equal(t, http.StatusOK, defaultSearch.Code)
	assert.Contains(t, defaultSearch.Body.String(), "huma-active")
	assert.NotContains(t, defaultSearch.Body.String(), "huma-deleted")
	withDeletedSearch := te.get(t,
		"/api/v1/search/content?pattern=needle&in=messages&limit=50&include_deleted=true")
	assert.Equal(t, http.StatusOK, withDeletedSearch.Code)
	assert.Contains(t, withDeletedSearch.Body.String(), "huma-deleted")
}
