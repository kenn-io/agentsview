package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
)

func TestSessionPushReceiverGates(t *testing.T) {
	for _, tc := range []struct {
		name          string
		enabled, auth bool
		token         string
		status        int
	}{
		{"disabled", false, false, "", http.StatusNotFound},
		{"auth-off", true, false, "secret", http.StatusForbidden},
		{"missing-token", true, true, "", http.StatusUnauthorized},
		{"wrong-token", true, true, "bad", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer(t, 30*time.Second)
			s.cfg.AcceptSessionPush, s.cfg.RequireAuth, s.cfg.AuthToken = tc.enabled, tc.auth, "secret"
			r := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/ingest", bytes.NewBufferString("not JSON"))
			r.Host = "127.0.0.1:0"
			r.Header.Set("Origin", "http://127.0.0.1:0")
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			assert.Equal(t, tc.status, w.Code, w.Body.String())
		})
	}
}

func TestSessionPushReceiverInsertAndRetry(t *testing.T) {
	s := testServer(t, 30*time.Second)
	s.cfg.AcceptSessionPush, s.cfg.RequireAuth, s.cfg.AuthToken = true, true, "secret"
	d := s.db.(*db.DB)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "source", Agent: "claude", Machine: "source"}))
	payload, err := artifact.EncodeSessionPush(t.Context(), d, "source", "")
	require.NoError(t, err)
	// Move this selected synthetic session to a distinct receiving archive.
	receiver := testServer(t, 30*time.Second)
	receiver.cfg.AcceptSessionPush, receiver.cfg.RequireAuth, receiver.cfg.AuthToken = true, true, "secret"
	for _, status := range []string{"created", "unchanged"} {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/ingest", bytes.NewReader(payload))
		r.Header.Set("Authorization", "Bearer secret")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		receiver.Handler().ServeHTTP(w, r)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.JSONEq(t, `{"session_id":"source","agent":"claude","status":"`+status+`"}`, w.Body.String())
	}
	stored, err := receiver.db.(*db.DB).GetSession(t.Context(), "source")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, "claude", stored.Agent)
}
