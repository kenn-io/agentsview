package server_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestMessagesExpectedRevisionReturnsSourceChanged(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "revisioned", "proj", 2)
	te.seedMessages(t, "revisioned", 2)

	first := te.get(t, "/api/v1/sessions/revisioned/messages?limit=20")
	assertStatus(t, first, http.StatusOK)
	list := decode[service.MessageList](t, first)
	require.NotEmpty(t, list.TranscriptRevision)
	require.NotEmpty(t, list.EvidenceSource)

	same := te.get(t, "/api/v1/sessions/revisioned/messages?limit=20&expected_revision="+
		url.QueryEscape(list.TranscriptRevision)+"&evidence_source="+url.QueryEscape(list.EvidenceSource))
	assertStatus(t, same, http.StatusOK)

	require.NoError(t, te.db.ReplaceSessionMessages(t.Context(), "revisioned", []db.Message{{
		SessionID: "revisioned", Ordinal: 0, Role: "user", Content: "changed",
	}}))
	changed := te.get(t, "/api/v1/sessions/revisioned/messages?expected_revision="+
		url.QueryEscape(list.TranscriptRevision))
	assertStatus(t, changed, http.StatusConflict)
	assert.Contains(t, changed.Body.String(), "source_changed")
}
