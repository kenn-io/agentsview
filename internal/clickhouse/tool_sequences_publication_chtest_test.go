//go:build chtest

package clickhouse

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/storage"
)

func TestToolSequencesRefuseEvidenceBeforeSessionRows(t *testing.T) {
	ctx := context.Background()
	local, target := seedFixture(t)
	s := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := s.Push(ctx, false, nil)
	require.NoError(t, err)

	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	handler := server.New(config.Config{Host: "127.0.0.1", InstallationID: "mirror"}, store, nil).Handler()
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:0/api/v1/sessions/"+fixtureAlphaID+"/tool-sequences", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	require.Equal(t, http.StatusOK, get().Code)

	// The push writes the new messages, then fails before the session row carrying their revision lands.
	appendMessage(t, local, fixtureAlphaID, "alpha third", "2026-01-10T00:30:00.000Z")
	s.hooks = &pushHooks{beforeSessionRows: func([]db.Session) error { return errors.New("simulated crash before session rows") }}
	_, err = s.Push(ctx, false, nil)
	require.NoError(t, err)

	res := get()
	assert.Equal(t, http.StatusConflict, res.Code, res.Body.String())
	assert.Contains(t, res.Body.String(), `"code":"source_changed"`)
	assert.NotContains(t, res.Body.String(), `"sequences"`)

	s.hooks = nil
	_, err = s.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, get().Code)
}
