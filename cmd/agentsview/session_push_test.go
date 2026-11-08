package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
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
	type capturedPush struct {
		receipt db.SessionPushReceipt
		write   db.SessionBatchWrite
		err     error
	}
	database, err := db.Open(t.Context(), filepath.Join(dir, "sessions.db"))
	require.NoError(t, err)
	for _, id := range []string{"codex:chosen", "codex:parent", "codex:child", "unrelated"} {
		message := db.Message{SessionID: id, Ordinal: 0, Role: "user", Content: "unrelated transcript marker"}
		session := db.Session{ID: id, Agent: "codex", Machine: "source", MessageCount: 1, UserMessageCount: 1}
		switch id {
		case "codex:parent":
			message.Role = "assistant"
			message.Content = "parent transcript marker"
			message.HasToolUse = true
			message.ToolCalls = []db.ToolCall{{ToolName: "Agent", Category: "Task", SubagentSessionID: "codex:chosen"}}
			session.UserMessageCount = 0
		case "codex:chosen":
			message.Role = "assistant"
			message.Content = "selected transcript marker"
			message.HasToolUse = true
			message.ToolCalls = []db.ToolCall{{ToolName: "Agent", Category: "Task", SubagentSessionID: "codex:child"}}
			session.ParentSessionID = new("codex:parent")
			session.RelationshipType = "subagent"
			session.UserMessageCount = 0
		case "codex:child":
			message.Content = "child transcript marker"
		case "unrelated":
			message.Content = "unrelated transcript marker"
		}
		_, err = database.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
			Session: session, Messages: []db.Message{message}, ReplaceMessages: true,
		}})
		require.NoError(t, err)
	}
	require.NoError(t, database.Close())
	t.Setenv("AGENTSVIEW_PUSH_TOKEN", "fixture-token")
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	var requests atomic.Int32
	received := make(chan capturedPush, 1)
	capture := func(result capturedPush) {
		select {
		case received <- result:
		default: // The request count assertion reports duplicate requests.
		}
	}
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/mounted/api/v1/sessions/ingest" ||
			r.Header.Get("Authorization") != "Bearer fixture-token" {
			capture(capturedPush{err: fmt.Errorf("unexpected push request path %q or authorization header", r.URL.Path)})
			http.Error(w, "unexpected push request", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			capture(capturedPush{err: err})
			http.Error(w, "cannot read pushed session", http.StatusBadRequest)
			return
		}
		receipt, write, err := artifact.DecodeSessionPush(body)
		if err != nil {
			capture(capturedPush{err: err})
			http.Error(w, "invalid pushed session", http.StatusBadRequest)
			return
		}
		capture(capturedPush{receipt: receipt, write: write})
		_, _ = w.Write([]byte("{\"session_id\":\"codex:chosen\",\"agent\":\"codex\",\"status\":\"created\"}"))
	}))
	defer remote.Close()
	cmd := newPushSessionCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"codex:chosen", "--to", remote.URL + "/mounted", "--agent", "codex", "--json"})
	require.NoError(t, cmd.ExecuteContext(t.Context()))
	assert.JSONEq(t, "{\"session_id\":\"codex:chosen\",\"agent\":\"codex\",\"status\":\"created\"}", out.String())
	var got capturedPush
	select {
	case got = <-received:
	case <-time.After(time.Second):
		require.FailNow(t, "receiver did not capture the pushed session")
	}
	require.NoError(t, got.err)
	assert.Equal(t, int32(1), requests.Load())
	assert.Equal(t, "codex:chosen", got.receipt.SessionID)
	assert.Equal(t, "codex:chosen", got.write.Session.ID)
	require.NotNil(t, got.write.Session.ParentSessionID)
	assert.Equal(t, "codex:parent", *got.write.Session.ParentSessionID)
	require.Len(t, got.write.Messages, 1)
	message := got.write.Messages[0]
	assert.Equal(t, "selected transcript marker", message.Content)
	assert.NotContains(t, message.Content, "parent transcript marker")
	assert.NotContains(t, message.Content, "child transcript marker")
	assert.NotContains(t, message.Content, "unrelated transcript marker")
	require.Len(t, message.ToolCalls, 1)
	assert.Equal(t, "codex:child", message.ToolCalls[0].SubagentSessionID)
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
	require.NoError(t, os.WriteFile(path, []byte(" from-file\n"), 0o600))
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
	require.NoError(t, os.WriteFile(path, []byte("valid-token"+strings.Repeat(" ", 4096)+"hidden-suffix"), 0o600))
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
	for _, status := range []int{
		http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusInternalServerError,
		http.StatusNotImplemented, http.StatusServiceUnavailable,
	} {
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
	assert.NotContains(t, err.Error(), "fixture")
}
