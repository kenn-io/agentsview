package server_test

import (
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
	assert := assert.New(t)
	require := require.New(t)

	te := setup(t)
	te.seedSession(t, "s1", "alpha", 2)

	w := te.patch(t, "/api/v1/sessions/s1/rename", `{"display_name":"Pinned investigation"}`)
	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	renamed := decode[db.Session](t, w)
	require.NotNil(renamed.DisplayName)
	assert.Equal("Pinned investigation", *renamed.DisplayName)

	w = te.patch(t, "/api/v1/sessions/s1/rename", `{"display_name":""}`)
	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	cleared := decode[db.Session](t, w)
	assert.Nil(cleared.DisplayName)

	w = te.patch(t, "/api/v1/sessions/missing/rename", `{"display_name":"Nope"}`)
	require.Equal(http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	assertErrorResponse(t, w, "session not found")
}

func TestSessionManagementTrashRestoreAndPermanentDeleteHandlers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	te := setup(t)
	te.seedSession(t, "s1", "alpha", 2)

	w := te.del(t, "/api/v1/sessions/s1")
	require.Equal(http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.get(t, "/api/v1/trash")
	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	trash := decode[trashHandlerResponse](t, w)
	require.Len(trash.Sessions, 1)
	assert.Equal("s1", trash.Sessions[0].ID)

	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.post(t, "/api/v1/sessions/s1/restore", `{}`)
	require.Equal(http.StatusNotFound, w.Code, "body: %s", w.Body.String())
	assertErrorResponse(t, w, "session not found or not in trash")

	w = te.del(t, "/api/v1/sessions/s1/permanent")
	require.Equal(http.StatusConflict, w.Code, "body: %s", w.Body.String())
	assertErrorResponse(t, w, "session not found or not in trash")

	w = te.del(t, "/api/v1/sessions/s1")
	require.Equal(http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	w = te.del(t, "/api/v1/sessions/s1/permanent")
	require.Equal(http.StatusNoContent, w.Code, "body: %s", w.Body.String())

	got, err := te.db.GetSessionFull(t.Context(), "s1")
	require.NoError(err)
	assert.Nil(got)
}

func TestSessionManagementEmptyTrashHandler(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	te := setup(t)
	te.seedSession(t, "s1", "alpha", 2)
	te.seedSession(t, "s2", "beta", 2)

	for _, id := range []string{"s1", "s2"} {
		w := te.del(t, "/api/v1/sessions/"+id)
		require.Equal(http.StatusNoContent, w.Code, "delete %s body: %s", id, w.Body.String())
	}

	w := te.del(t, "/api/v1/trash")
	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decode[emptyTrashHandlerResponse](t, w)
	assert.Equal(2, resp.Deleted)

	w = te.get(t, "/api/v1/trash")
	require.Equal(http.StatusOK, w.Code, "body: %s", w.Body.String())
	trash := decode[trashHandlerResponse](t, w)
	assert.Empty(trash.Sessions)
}
