package codexcloud_test

import (
	"encoding/json/jsontext"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud"
	"go.kenn.io/agentsview/internal/codexcloud/codexcloudtest"
)

const clientCanary = "test-access-token-canary-9d2e"

func dialOptions(host *codexcloudtest.Host, mode codexcloud.Mode) codexcloud.DialOptions {
	return codexcloud.DialOptions{
		Endpoint: host.URL(),
		Login: codexcloud.Login{
			Token:     codexcloud.NewToken(clientCanary),
			AccountID: "account-a",
		},
		Mode:    mode,
		Version: "1.2.3",
	}
}

func dial(t *testing.T, host *codexcloudtest.Host, mode codexcloud.Mode) *codexcloud.Conn {
	t.Helper()
	conn, err := codexcloud.Dial(t.Context(), dialOptions(host, mode))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestDialHandshake(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	dial(t, host, codexcloud.ModeFetch)

	handshakes := host.Handshakes()
	require.Len(t, handshakes, 1)
	assert.Equal(t, []string{
		"codex-app-server", "codex-client.agentsview", "openai-bearer." + clientCanary,
	}, handshakes[0].Subprotocols)
	header := handshakes[0].Header
	assert.Equal(t, "codex", header.Get("X-OpenAI-Product-Sku"))
	assert.Equal(t, "true", header.Get("X-OpenAI-Allow-Environmentless-Fallback"))
	assert.Equal(t, "agentsview", header.Get("originator"))
	assert.Equal(t, "account-a", header.Get("ChatGPT-Account-Id"))
	assert.Equal(t, "agentsview/1.2.3", header.Get("User-Agent"))
	assert.Empty(t, header.Get("Authorization"))
	for name, values := range header {
		if name == "Sec-Websocket-Protocol" {
			continue
		}
		assert.NotContains(t, strings.Join(values, ","), clientCanary, "header %s", name)
	}

	require.Eventually(t, func() bool { return len(host.Requests()) == 2 }, time.Second, 5*time.Millisecond)
	requests := host.Requests()
	assert.Equal(t, []string{"initialize", "initialized"}, host.Methods())
	assert.JSONEq(t, `{"clientInfo":{"name":"agentsview","version":"1.2.3"}}`, string(requests[0].Params))
	assert.Empty(t, requests[1].ID)
}

func TestDialRefusesRedirect(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	target := codexcloudtest.NewHost(t)
	host.Redirect(target.URL().String())

	_, err := codexcloud.Dial(t.Context(), dialOptions(host, codexcloud.ModeFetch))

	httpErr, ok := errors.AsType[*codexcloud.HTTPError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, http.StatusFound, httpErr.Status)
	assert.Len(t, host.Handshakes(), 1)
	assert.Empty(t, target.Handshakes())
}

func TestDialRefusedStatus(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	host.RefuseHandshake(http.StatusTooManyRequests,
		http.Header{"Retry-After": {"30"}}, "openai-bearer."+clientCanary)

	_, err := codexcloud.Dial(t.Context(), dialOptions(host, codexcloud.ModeFetch))

	httpErr, ok := errors.AsType[*codexcloud.HTTPError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, http.StatusTooManyRequests, httpErr.Status)
	assert.Equal(t, 30*time.Second, httpErr.RetryAfter)
	assert.NotContains(t, err.Error(), clientCanary)
}

func TestHandshakeErrorRedacted(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	host.EchoSubprotocols()

	_, err := codexcloud.Dial(t.Context(), dialOptions(host, codexcloud.ModeFetch))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Sec-WebSocket-Protocol")
	assert.NotContains(t, err.Error(), clientCanary)
}

func TestDialRejectsUnexpectedSubprotocol(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	host.SelectSubprotocol("openai-bearer." + clientCanary)

	_, err := codexcloud.Dial(t.Context(), dialOptions(host, codexcloud.ModeFetch))

	require.ErrorIs(t, err, codexcloud.ErrProtocolChange)
	assert.NotContains(t, err.Error(), clientCanary)
}

func TestDialAcceptsNoSelectedSubprotocol(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	host.SelectSubprotocol("")
	dial(t, host, codexcloud.ModeFetch)
}

func TestAllowlist(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	fetch := dial(t, host, codexcloud.ModeFetch)
	monitor := dial(t, host, codexcloud.ModeMonitor)
	require.Eventually(t, func() bool { return len(host.Requests()) == 4 }, time.Second, 5*time.Millisecond)

	for name, call := range map[string]func() error{
		"thread/read": func() error {
			return fetch.Call(t.Context(), "thread/read", map[string]string{"threadId": "t"}, nil)
		},
		"resume in fetch mode": func() error {
			return fetch.Call(t.Context(), "thread/resume",
				codexcloud.ResumeParams{ThreadID: "t", ExcludeTurns: true}, nil)
		},
		"resume with an extra field": func() error {
			return monitor.Call(t.Context(), "thread/resume",
				map[string]any{"threadId": "t", "excludeTurns": true, "model": "x"}, nil)
		},
		"resume with turns": func() error {
			return monitor.Call(t.Context(), "thread/resume", codexcloud.ResumeParams{ThreadID: "t"}, nil)
		},
		"turn/start": func() error {
			return monitor.Call(t.Context(), "turn/start", map[string]string{"threadId": "t"}, nil)
		},
	} {
		require.ErrorIs(t, call(), codexcloud.ErrNotAllowed, name)
	}
	assert.Len(t, host.Requests(), 4, "a refused call must send nothing")

	host.Handle("thread/resume", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		return map[string]string{"resumeKind": "warm"}, nil
	})
	var reply struct {
		ResumeKind string `json:"resumeKind"`
	}
	require.NoError(t, monitor.Call(t.Context(), "thread/resume",
		codexcloud.ResumeParams{ThreadID: "t", ExcludeTurns: true}, &reply))
	assert.Equal(t, "warm", reply.ResumeKind)
	last := host.Requests()[len(host.Requests())-1]
	assert.JSONEq(t, `{"threadId":"t","excludeTurns":true}`, string(last.Params))
}

func TestServerRequestsNeverAnswered(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	release := make(chan struct{})
	host.Handle("thread/list", func(jsontext.Value) (any, *codexcloudtest.Error) {
		<-release
		return map[string]any{"data": []any{}}, nil
	})
	defer close(release)
	conn := dial(t, host, codexcloud.ModeMonitor)

	require.NoError(t, host.ServerRequest(t.Context(), "item/commandExecution/requestApproval",
		map[string]string{"threadId": "t"}))
	pending := make(chan error, 1)
	go func() { pending <- conn.Call(t.Context(), "thread/list", map[string]any{}, nil) }()
	require.Eventually(t, func() bool { return len(host.Requests()) == 3 }, time.Second, 5*time.Millisecond)
	require.NoError(t, host.ServerRequest(t.Context(), "account/chatgptAuthTokens/refresh", map[string]any{}))

	select {
	case <-conn.Done():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "connection stayed open after a refresh request")
	}
	require.ErrorIs(t, conn.Err(), codexcloud.ErrRefreshRequested)
	require.ErrorIs(t, <-pending, codexcloud.ErrRefreshRequested)
	assert.Equal(t, []string{"initialize", "initialized", "thread/list"}, host.Methods(),
		"no server request may be answered")
}

func TestNotificationsReachMonitor(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	conn := dial(t, host, codexcloud.ModeMonitor)

	require.NoError(t, host.Notify(t.Context(), "thread/tokenUsage/updated", map[string]string{"threadId": "t"}))

	select {
	case note := <-conn.Notifications():
		assert.Equal(t, "thread/tokenUsage/updated", note.Method)
		assert.JSONEq(t, `{"threadId":"t"}`, string(note.Params))
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no notification")
	}
}

func TestReadLimit(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	var size atomic.Int64
	size.Store(40 << 10)
	host.Handle("thread/items/list", func(jsontext.Value) (any, *codexcloudtest.Error) {
		return map[string]string{"blob": strings.Repeat("a", int(size.Load()))}, nil
	})
	opts := dialOptions(host, codexcloud.ModeFetch)
	opts.ReadLimit = 64 << 10
	conn, err := codexcloud.Dial(t.Context(), opts)
	require.NoError(t, err)
	defer conn.Close()

	var reply struct {
		Blob string `json:"blob"`
	}
	require.NoError(t, conn.Call(t.Context(), "thread/items/list", map[string]any{}, &reply))
	assert.Len(t, reply.Blob, 40<<10)

	size.Store(65 << 10)
	err = conn.Call(t.Context(), "thread/items/list", map[string]any{}, &reply)
	require.ErrorIs(t, err, codexcloud.ErrReplyTooLarge)
	require.ErrorIs(t, conn.Err(), codexcloud.ErrReplyTooLarge)
}

func TestRPCErrorReturned(t *testing.T) {
	host := codexcloudtest.NewHost(t)
	conn := dial(t, host, codexcloud.ModeFetch)

	err := conn.Call(t.Context(), "thread/turns/list", map[string]any{}, nil)

	rpcErr, ok := errors.AsType[*codexcloud.RPCError](err)
	require.True(t, ok, "got %v", err)
	assert.Equal(t, -32601, rpcErr.Code)
	assert.Equal(t, "Method not found: thread/turns/list", rpcErr.Message)
}

func TestDecodeCount(t *testing.T) {
	for raw, want := range map[string]int64{"4603049.0": 4603049, "12": 12, "0": 0} {
		got, err := codexcloud.DecodeCount(jsontext.Value(raw))
		require.NoError(t, err, raw)
		assert.Equal(t, want, got, raw)
	}
	for _, raw := range []string{"1.5", "-1", `"12"`, "null", "1e300"} {
		_, err := codexcloud.DecodeCount(jsontext.Value(raw))
		assert.Error(t, err, raw)
	}
}
