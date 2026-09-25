package server_test

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestInitializeConversationExportBuildsProjectionOnce(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "chat", "alpha", 1)
	require.NoError(t, te.db.InsertMessages(t.Context(), []db.Message{{
		SessionID: "chat", Role: "assistant", Content: "Saved reply", SourceUUID: "reply-one",
	}}))
	reader, err := db.OpenReadOnly(t.Context(), te.db.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reader.Close()) })
	_, err = reader.ExportConversationChanges(t.Context(), db.ConversationExportOptions{})
	require.ErrorIs(t, err, db.ErrConversationInitializationRequired, "sync leaves a fresh archive cold")

	var response struct {
		DatabaseID  string `json:"database_id"`
		Initialized bool   `json:"initialized"`
	}
	w := te.requestJSON(t, http.MethodPost, "/api/v1/export/conversations/initialize", "")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.True(t, response.Initialized)
	page, err := reader.ExportConversationChanges(t.Context(), db.ConversationExportOptions{})
	require.NoError(t, err)
	assert.Equal(t, response.DatabaseID, page.DatabaseID)
	require.Len(t, page.Changes, 1)

	w = te.requestJSON(t, http.MethodPost, "/api/v1/export/conversations/initialize", "")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.False(t, response.Initialized, "an active archive is not rebuilt")
}

func TestInitializeConversationExportReturns503WhileWriterClosed(t *testing.T) {
	te := setup(t)
	require.NoError(t, te.db.CloseWriter())
	defer func() { assert.NoError(t, te.db.ReopenWriter()) }()

	w := te.requestJSON(t, http.MethodPost, "/api/v1/export/conversations/initialize", "")
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, "5", w.Header().Get("Retry-After"))
}
