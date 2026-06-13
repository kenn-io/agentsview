package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

type trashHandlerResponse struct {
	Sessions []db.Session `json:"sessions"`
}

type emptyTrashHandlerResponse struct {
	Deleted int `json:"deleted"`
}

func TestSessionManagementRenameHandler(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "alpha", 2)

	w := te.patch(t, "/api/v1/sessions/s1/rename", `{"display_name":"Pinned investigation"}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	renamed := decode[db.Session](t, w)
	require.NotNil(t, renamed.DisplayName)
	assert.Equal(t, "Pinned investigation", *renamed.DisplayName)

	w = te.patch(t, "/api/v1/sessions/s1/rename", `{"display_name":""}`)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	cleared := decode[db.Session](t, w)
	assert.Nil(t, cleared.DisplayName)

	w = te.patch(t, "/api/v1/sessions/missing/rename", `{"display_name":"Nope"}`)
	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	assertErrorResponse(t, w, "session not found")
}

func TestSessionManagementTrashRestoreAndPermanentDeleteHandlers(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "alpha", 2)

	w := te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.get(t, "/api/v1/trash")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	trash := decode[trashHandlerResponse](t, w)
	require.Len(t, trash.Sessions, 1)
	assert.Equal(t, "s1", trash.Sessions[0].ID)

	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(t, http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	assertErrorResponse(t, w, "session not found or not in trash")

	w = te.del(t, "/api/v1/sessions/s1/permanent")
	require.Equal(t, http.StatusConflict, w.Code, "body: %s", w.Body.String())
	assertErrorResponse(t, w, "session not found or not in trash")

	w = te.del(t, "/api/v1/sessions/s1")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.del(t, "/api/v1/sessions/s1/permanent")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	got, err := te.db.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestSessionManagementEmptyTrashHandler(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "s1", "alpha", 2)
	te.seedSession(t, "s2", "beta", 2)

	for _, id := range []string{"s1", "s2"} {
		w := te.del(t, "/api/v1/sessions/"+id)
		require.Equal(t, http.StatusNoContent, w.Code, "delete %s body: %s", id, w.Body.String())
	}

	w := te.del(t, "/api/v1/trash")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decode[emptyTrashHandlerResponse](t, w)
	assert.Equal(t, 2, resp.Deleted)

	w = te.get(t, "/api/v1/trash")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	trash := decode[trashHandlerResponse](t, w)
	assert.Empty(t, trash.Sessions)
}
