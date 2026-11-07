package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
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
	for _, id := range []string{"codex:selected", "unrelated"} {
		_, err = source.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
			Session:  db.Session{ID: id, Agent: "codex", Machine: "source", MessageCount: 1, UserMessageCount: 1},
			Messages: []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: id}}, ReplaceMessages: true,
		}})
		require.NoError(t, err)
	}
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
	for _, tc := range []struct {
		status string
		flags  []string
	}{
		{"created", []string{"--json"}},
		{"unchanged", []string{"--format", "json"}},
	} {
		cmd := newPushSessionCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(append([]string{"codex:selected", "--to", remote.URL + "/mounted", "--agent", "codex"}, tc.flags...))
		require.NoError(t, cmd.ExecuteContext(t.Context()))
		assert.JSONEq(t, `{"session_id":"codex:selected","agent":"codex","status":"`+tc.status+`"}`, out.String())
	}
	messages, err := target.GetAllMessages(t.Context(), "codex:selected")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "codex:selected", messages[0].Content)
	unrelated, err := target.GetSession(t.Context(), "unrelated")
	require.NoError(t, err)
	assert.Nil(t, unrelated)
}

func TestPushSessionCommandExactGraph(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	database, err := db.Open(t.Context(), filepath.Join(dir, "sessions.db"))
	require.NoError(t, err)
	for _, id := range []string{"codex:chosen", "codex:parent", "codex:child", "unrelated"} {
		_, err = database.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: db.Session{ID: id, Agent: "codex", Machine: "source", MessageCount: 1, UserMessageCount: 1}, Messages: []db.Message{{SessionID: id, Ordinal: 0, Role: "user", Content: id}}, ReplaceMessages: true}})
		require.NoError(t, err)
	}
	require.NoError(t, database.Close())
	t.Setenv("AGENTSVIEW_PUSH_TOKEN", "fixture-token")
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	requests := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		assert.Equal(t, "/mounted/api/v1/sessions/ingest", r.URL.Path)
		assert.Equal(t, "Bearer fixture-token", r.Header.Get("Authorization"))
		var packet map[string]any
		require.NoError(t, json.UnmarshalRead(r.Body, &packet))
		encoded, err := json.Marshal(packet)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "codex:parent")
		assert.NotContains(t, string(encoded), "codex:child")
		assert.NotContains(t, string(encoded), "unrelated")
		_, err = w.Write([]byte(`{"session_id":"codex:chosen","agent":"codex","status":"created"}`))
		require.NoError(t, err)
	}))
	defer remote.Close()
	cmd := newPushSessionCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"codex:chosen", "--to", remote.URL + "/mounted", "--agent", "codex", "--json"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	assert.JSONEq(t, `{"session_id":"codex:chosen","agent":"codex","status":"created"}`, out.String())
	assert.Equal(t, 1, requests)
}

func TestPushSessionTransportRefusesUnsafeTargets(t *testing.T) {
	for _, target := range []string{"http://remote.example", "https://secret-user@example.test", "https://example.test?token=secret", "https://example.test/#secret", "//example.test", "ftp://example.test"} {
		_, err := sessionPushURL(target)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "secret")
	}
	t.Setenv("AGENTSVIEW_PUSH_TOKEN", "")
	_, err := sessionPushToken(t.Context(), "AGENTSVIEW_PUSH_TOKEN", "")
	require.Error(t, err)
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(" from-file\n"), 0600))
	token, err := sessionPushToken(t.Context(), "", path)
	require.NoError(t, err)
	assert.Equal(t, "from-file", token)
	t.Setenv("SELECTED_TOKEN", "bad\r\nheader")
	_, err = sessionPushToken(t.Context(), "SELECTED_TOKEN", "")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "header")
}

func TestPushSessionRejectOversizeTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("valid-token"+strings.Repeat(" ", 4096)+"hidden-suffix"), 0600))
	_, err := sessionPushToken(t.Context(), "", path)
	require.Error(t, err, "never accept a truncated prefix of a selected credential file")
}

func TestPushSessionExplicitEmptyTokenFile(t *testing.T) {
	t.Setenv("AGENTSVIEW_PUSH_TOKEN", "fallback-must-not-be-used")
	cmd := newPushSessionCommand()
	cmd.SetArgs([]string{"sample", "--to", "https://example.test", "--token-file", ""})
	err := cmd.ExecuteContext(t.Context())
	require.ErrorContains(t, err, "token file")
}

func TestPushSessionTransportFailuresAreSanitized(t *testing.T) {
	for _, status := range []int{301, 307, 308, 400, 401, 403, 404, 409, 413, 500, 501, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "https://secret.example")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("private-response-token"))
			}))
			defer remote.Close()
			_, err := sendSessionPush(t.Context(), remote.URL, "private-token", []byte(`{}`), "sample", "claude")
			require.Error(t, err)
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
	assert.False(t, strings.Contains(err.Error(), "fixture"))
}
