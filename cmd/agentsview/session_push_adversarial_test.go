package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushSessionTransportPreservesPayloadBytes(t *testing.T) {
	payload := []byte(" {\"counter\":9007199254740993,\"cost\":0.10000000000000001} \n")
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.Equal(t, payload, body, "transport must preserve validated bytes without a numeric JSON round trip")
		_, _ = w.Write([]byte(`{"session_id":"selected","agent":"claude","status":"created"}`))
	}))
	defer remote.Close()
	_, err := sendSessionPush(t.Context(), remote.URL, "fixture-token", payload, "selected", "claude")
	require.NoError(t, err)
}

func TestPushSessionPreservesEscapedBasePath(t *testing.T) {
	var escaped string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"session_id":"selected","agent":"claude","status":"created"}`))
	}))
	defer remote.Close()
	_, err := sendSessionPush(t.Context(), remote.URL+"/tenant%2Fprivate", "fixture-token", []byte(`{}`), "selected", "claude")
	require.NoError(t, err)
	assert.Equal(t, "/tenant%2Fprivate/api/v1/sessions/ingest", escaped)
}

func TestPushSessionRedirectNeverReceivesCredentialsOrBody(t *testing.T) {
	requests := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; w.WriteHeader(http.StatusOK) }))
	defer target.Close()
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, status) }))
			defer redirect.Close()
			_, err := sendSessionPush(t.Context(), redirect.URL, "fixture-secret", []byte("selected-body"), "selected", "claude")
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "fixture-secret")
		})
	}
	assert.Zero(t, requests)
}
