package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
)

func TestPushSessionCommandToAuthenticatedReceiver(t *testing.T) {
	sourceDir := t.TempDir()
	t.Setenv("HOME", sourceDir)
	t.Setenv("AGENTSVIEW_DATA_DIR", sourceDir)
	t.Setenv("AGENTSVIEW_PUSH_TOKEN", "fixture-token")
	source, err := db.Open(t.Context(), filepath.Join(sourceDir, "sessions.db"))
	require.NoError(t, err)
	_, err = source.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session:  db.Session{ID: "host~codex:selected", Agent: "codex", Machine: "source", ParentSessionID: new("host~codex:parent"), RelationshipType: "subagent", MessageCount: 1, UserMessageCount: 1},
		Messages: []db.Message{{SessionID: "host~codex:selected", Ordinal: 0, Role: "user", Content: "host~codex:selected"}}, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	require.NoError(t, source.Close())
	receiverDir := t.TempDir()
	receiverPath := filepath.Join(receiverDir, "sessions.db")
	target, err := db.Open(t.Context(), receiverPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.Close()) })
	receiver := server.New(config.Config{
		Host: "127.0.0.1", Port: 0, DataDir: receiverDir, DBPath: receiverPath,
		RequireAuth: true, AuthToken: "fixture-token", AcceptSessionPush: true,
		WriteTimeout: 30 * time.Second,
	}, target, nil, server.WithBasePath("/mounted"))
	t.Cleanup(func() { require.NoError(t, receiver.Shutdown(context.WithoutCancel(t.Context()))) })
	remote := httptest.NewServer(receiver.Handler())
	defer remote.Close()
	cmd := newPushSessionCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"host~codex:selected", "--to", remote.URL + "/mounted", "--agent", "codex", "--json"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	assert.JSONEq(t, `{"session_id":"host~codex:selected","agent":"codex","status":"created"}`, out.String())
	messages, err := target.GetAllMessages(t.Context(), "host~codex:selected")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "host~codex:selected", messages[0].Content)
	stored, err := target.GetSessionFull(t.Context(), "host~codex:selected")
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, new("host~codex:parent"), stored.ParentSessionID)
}

func TestPushSessionTransportRefusesUnsafeTargets(t *testing.T) {
	for _, target := range []string{"http://remote.example", "https://secret-user@example.test", "https://example.test?token=secret", "https://example.test/#secret", "//example.test", "ftp://example.test"} {
		_, err := sessionPushURL(target)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret")
	}
}

func TestPushSessionTokenSources(t *testing.T) {
	for _, tc := range []struct {
		name, env, file, want string
		fail                  bool
	}{
		{"empty environment", "", "", "", true},
		{"trim file", "", " from-file\n", "from-file", false},
		{"invalid header", "bad\r\nheader", "", "", true},
		{"oversize file", "", "valid-token" + strings.Repeat(" ", 4096) + "hidden-suffix", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SELECTED_TOKEN", tc.env)
			path := ""
			if tc.file != "" {
				path = filepath.Join(t.TempDir(), "token")
				require.NoError(t, os.WriteFile(path, []byte(tc.file), 0o600))
			}
			token, err := sessionPushToken(t.Context(), "SELECTED_TOKEN", path)
			if tc.fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, token)
			}
		})
	}
}

func TestPushSessionExplicitEmptyTokenFile(t *testing.T) {
	t.Setenv("AGENTSVIEW_PUSH_TOKEN", "fallback-must-not-be-used")
	cmd := newPushSessionCommand()
	cmd.SetArgs([]string{"sample", "--to", "https://example.test", "--token-file", ""})
	err := cmd.ExecuteContext(t.Context())
	require.ErrorContains(t, err, "token file")
}

func TestPushSessionTransportFailuresAreSanitized(t *testing.T) {
	for _, tc := range []struct {
		name            string
		status          int
		header, message string
	}{
		{"bad request", http.StatusBadRequest, "", "receiver rejected the session"},
		{"unauthorized", http.StatusUnauthorized, "", "receiver rejected the bearer token"},
		{"forbidden", http.StatusForbidden, "", "receiver is unavailable"},
		{"not found", http.StatusNotFound, "", "receiver is unavailable"},
		{"conflict", http.StatusConflict, "", "receiver cannot replace it"},
		{"too large", http.StatusRequestEntityTooLarge, "", "session exceeds receiver size limit"},
		{"internal error", http.StatusInternalServerError, "", "receiver rejected the session"},
		{"unsupported", http.StatusNotImplemented, "", "receiver does not support session pushes"},
		{"busy", http.StatusServiceUnavailable, "", "receiver is temporarily unavailable"},
		{"upgrade", http.StatusBadRequest, "future-data-version", "receiver needs an upgrade"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("X-AgentsView-Push-Error", tc.header)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("private-response-token"))
			}))
			defer remote.Close()
			_, err := sendSessionPush(t.Context(), remote.URL, "private-token", []byte(`{}`), "sample", "claude")
			require.ErrorContains(t, err, tc.message)
			assert.NotContains(t, err.Error(), "private")
			assert.NotContains(t, err.Error(), "secret")
		})
	}

	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"session_id":"wrong","agent":"claude","status":"created"}`))
	}))
	defer remote.Close()
	_, err := sendSessionPush(t.Context(), remote.URL, "fixture", nil, "sample", "claude")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "fixture")
}

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
