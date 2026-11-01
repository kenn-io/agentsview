package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

type protectedPurgeStore struct {
	db.Store
	refused []string
}

func (s *protectedPurgeStore) DeleteSessionIfTrashed(ctx context.Context, id string) (int64, error) {
	if id == "protected" {
		s.refused = append(s.refused, id)
		return 0, fmt.Errorf("private identity details: %w", db.ErrProtectedSessionPurge)
	}
	return s.Store.DeleteSessionIfTrashed(ctx, id)
}

func (s *protectedPurgeStore) EmptyTrash(context.Context) (int, error) {
	s.refused = append(s.refused, "trash")
	return 0, fmt.Errorf("private identity details: %w", db.ErrProtectedSessionPurge)
}

func TestProtectedPurgeConflictEndpoints(t *testing.T) {
	local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })
	require.NoError(t, local.UpsertSession(t.Context(), db.Session{ID: "ordinary", Project: "p", Agent: "claude"}))
	require.NoError(t, local.SoftDeleteSession(t.Context(), "ordinary"))
	store := &protectedPurgeStore{Store: local}
	srv := newRoutedTestServerWithStore(t, store)
	notifications := 0
	srv.sessionMutationNotify = func() { notifications++ }
	for _, path := range []string{"/api/v1/sessions/protected/permanent", "/api/v1/trash"} {
		response := httptest.NewRecorder()
		srv.mux.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodDelete, path, nil))
		require.Equal(t, http.StatusConflict, response.Code, "body: %s", response.Body.String())
		assert.Contains(t, response.Body.String(), "purge conflicts with a protected session identity")
		assert.NotContains(t, response.Body.String(), "private identity details")
	}
	assert.Equal(t, []string{"protected", "trash"}, store.refused)
	assert.Zero(t, notifications, "refusal must not publish a completed mutation")
	response := httptest.NewRecorder()
	srv.mux.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodDelete, "/api/v1/sessions/ordinary/permanent", nil))
	require.Equal(t, http.StatusNoContent, response.Code, "body: %s", response.Body.String())
	assert.Equal(t, 1, notifications)
	session, err := local.GetSessionFull(t.Context(), "ordinary")
	require.NoError(t, err)
	assert.Nil(t, session)
}
