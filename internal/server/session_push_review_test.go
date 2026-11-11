package server

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
)

func TestSessionPushReportsFutureParserVersion(t *testing.T) {
	s := testServer(t, 30*time.Second)
	s.cfg.AcceptSessionPush, s.cfg.RequireAuth, s.cfg.AuthToken = true, true, "fixture"
	d := s.db.(*db.DB)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "selected", Agent: "claude", Machine: "source"}))
	packet, _, err := artifact.EncodeSessionPush(t.Context(), d, "selected", "")
	require.NoError(t, err)
	var envelope artifact.SessionPushEnvelope
	require.NoError(t, json.Unmarshal(packet, &envelope))
	var manifest map[string]any
	require.NoError(t, json.Unmarshal(envelope.Manifest, &manifest))
	manifest["data_version"] = db.CurrentDataVersion() + 1
	envelope.Manifest, err = json.Marshal(manifest)
	require.NoError(t, err)
	packet, err = json.Marshal(envelope)
	require.NoError(t, err)
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/sessions/ingest", bytes.NewReader(packet))
	r.Header.Set("Authorization", "Bearer fixture")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "future-data-version", w.Header().Get("X-AgentsView-Push-Error"))
}
