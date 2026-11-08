package server

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
)

func TestSessionPushServeExtendsBoundedUploadReadDeadline(t *testing.T) {
	s := testServer(t, 30*time.Second)
	s.cfg.AcceptSessionPush, s.cfg.RequireAuth, s.cfg.AuthToken = true, true, "secret"
	s.httpReadTimeout = 100 * time.Millisecond
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadlines := make(chan time.Time, 16)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.Serve(readDeadlineListener{Listener: listener, deadlines: deadlines})
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
		defer cancel()
		require.NoError(t, s.Shutdown(ctx))
		require.ErrorIs(t, <-serveErr, http.ErrServerClosed)
	})

	source, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "source.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, source.Close()) })
	require.NoError(t, source.UpsertSession(t.Context(), db.Session{
		ID: "selected", Agent: "claude", Machine: "source",
	}))
	payload, err := artifact.EncodeSessionPush(t.Context(), source, "selected", "")
	require.NoError(t, err)

	reader, writer := io.Pipe()
	request, err := http.NewRequestWithContext(
		t.Context(), http.MethodPost,
		"http://"+listener.Addr().String()+"/api/v1/sessions/ingest", reader,
	)
	require.NoError(t, err)
	request.ContentLength = int64(len(payload))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	response := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		result, err := http.DefaultClient.Do(request)
		if err != nil {
			requestErr <- err
			return
		}
		response <- result
	}()
	defer writer.Close()
	defer reader.Close()
	_, err = writer.Write(payload[:1])
	require.NoError(t, err)

	deadlineWait := time.NewTimer(3 * time.Second)
	defer deadlineWait.Stop()
waitForUploadDeadline:
	for {
		select {
		case deadline := <-deadlines:
			if time.Until(deadline) > time.Minute {
				break waitForUploadDeadline
			}
		case <-deadlineWait.C:
			require.FailNow(t, "session push did not extend the real connection read deadline")
		}
	}
	time.Sleep(200 * time.Millisecond)
	_, err = writer.Write(payload[1:])
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	select {
	case err := <-requestErr:
		require.FailNowf(t, "test failed", "slow valid session push failed: %v", err)
	case result := <-response:
		defer result.Body.Close()
		assert.Equal(t, http.StatusOK, result.StatusCode)
	case <-time.After(3 * time.Second):
		require.FailNow(t, "slow valid session push did not finish")
	}
}

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
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
				"/api/v1/sessions/ingest", bytes.NewBufferString("not JSON"))
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
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
			"/api/v1/sessions/ingest", bytes.NewReader(payload))
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
