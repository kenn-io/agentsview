package service_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wesm/agentsview/internal/config"
	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/dbtest"
	"github.com/wesm/agentsview/internal/server"
	"github.com/wesm/agentsview/internal/service"
)

// newHTTPTestServer builds an in-memory SQLite DB, constructs a
// real *server.Server on top of it with a nil sync engine (so
// Sync returns the read-only error), and starts an httptest
// server whose listener port is baked into the server's Host
// allowlist. Returns the base URL and the underlying *db.DB so
// callers can seed fixtures directly.
func newHTTPTestServer(t *testing.T) (string, *db.DB) {
	t.Helper()
	return newHTTPTestServerWithCfg(t, config.Config{})
}

// newHTTPTestServerWithCfg builds an in-memory test server and lets
// callers override auth-related config (RequireAuth / AuthToken).
// Unset fields are filled with the same defaults as newHTTPTestServer.
func newHTTPTestServerWithCfg(
	t *testing.T, extra config.Config,
) (string, *db.DB) {
	t.Helper()
	d := dbtest.OpenTestDB(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := config.Config{
		Host:         "127.0.0.1",
		Port:         port,
		DataDir:      t.TempDir(),
		WriteTimeout: 30 * time.Second,
		RequireAuth:  extra.RequireAuth,
		AuthToken:    extra.AuthToken,
	}
	srv := server.New(cfg, d, nil)
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return ts.URL, d
}

func TestHTTPBackend_Get_Roundtrip(t *testing.T) {
	t.Parallel()
	baseURL, d := newHTTPTestServer(t)
	dbtest.SeedSession(t, d, "s-1", "my-app", func(s *db.Session) {
		s.MessageCount = 2
	})

	svc := service.NewHTTPBackend(baseURL, "", false)
	detail, err := svc.Get(context.Background(), "s-1")
	require.NoError(t, err)
	require.NotNil(t, detail)
	assert.Equal(t, "s-1", detail.ID)
	assert.Equal(t, "my-app", detail.Project)
	assert.Equal(t, 2, detail.MessageCount)
}

func TestHTTPBackend_Get_NotFound(t *testing.T) {
	t.Parallel()
	baseURL, _ := newHTTPTestServer(t)

	svc := service.NewHTTPBackend(baseURL, "", false)
	// Transport-neutral contract: missing session returns (nil, nil),
	// matching directBackend.Get.
	detail, err := svc.Get(context.Background(), "does-not-exist")
	require.NoError(t, err)
	assert.Nil(t, detail)
}

func TestHTTPBackend_List_Empty(t *testing.T) {
	t.Parallel()
	baseURL, _ := newHTTPTestServer(t)

	svc := service.NewHTTPBackend(baseURL, "", false)
	list, err := svc.List(context.Background(), service.ListFilter{Limit: 10})
	require.NoError(t, err)
	require.NotNil(t, list)
	assert.Equal(t, 0, list.Total)
}

func TestHTTPBackend_List_FilterRoundtrip(t *testing.T) {
	t.Parallel()
	baseURL, d := newHTTPTestServer(t)
	dbtest.SeedSession(t, d, "a-1", "proj-a", func(s *db.Session) {
		s.MessageCount = 3
	})
	dbtest.SeedSession(t, d, "b-1", "proj-b", func(s *db.Session) {
		s.MessageCount = 3
	})

	svc := service.NewHTTPBackend(baseURL, "", false)
	list, err := svc.List(context.Background(), service.ListFilter{
		Project:        "proj-a",
		IncludeOneShot: true,
		Limit:          10,
	})
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Len(t, list.Sessions, 1)
	assert.Equal(t, "a-1", list.Sessions[0].ID)
	assert.Equal(t, "proj-a", list.Sessions[0].Project)
}

func TestHTTPBackend_List_InvalidDate(t *testing.T) {
	t.Parallel()
	baseURL, _ := newHTTPTestServer(t)

	svc := service.NewHTTPBackend(baseURL, "", false)
	_, err := svc.List(context.Background(), service.ListFilter{
		Date: "2024/01/15",
	})
	require.Error(t, err)
	// The server rejects invalid dates with 400.
	assert.Contains(t, err.Error(), "HTTP 400")
}

func TestHTTPBackend_Messages_Roundtrip(t *testing.T) {
	t.Parallel()
	baseURL, d := newHTTPTestServer(t)
	const sid = "msg-session"
	dbtest.SeedSession(t, d, sid, "p1", func(s *db.Session) {
		s.MessageCount = 3
	})
	msgs := []db.Message{
		dbtest.UserMsg(sid, 0, "hello"),
		dbtest.AsstMsg(sid, 1, "world"),
		dbtest.UserMsg(sid, 2, "bye"),
	}
	dbtest.SeedMessages(t, d, msgs...)

	svc := service.NewHTTPBackend(baseURL, "", false)
	zero := 0
	list, err := svc.Messages(context.Background(), sid, service.MessageFilter{
		From:  &zero,
		Limit: 100,
	})
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Equal(t, 3, list.Count)
	assert.Equal(t, 0, list.Messages[0].Ordinal)
	assert.Equal(t, "hello", list.Messages[0].Content)
	assert.Equal(t, 2, list.Messages[2].Ordinal)
	assert.Equal(t, "bye", list.Messages[2].Content)
}

func TestHTTPBackend_Messages_DescDirection(t *testing.T) {
	t.Parallel()
	baseURL, d := newHTTPTestServer(t)
	const sid = "msg-desc"
	dbtest.SeedSession(t, d, sid, "p1", func(s *db.Session) {
		s.MessageCount = 3
	})
	msgs := make([]db.Message, 0, 3)
	for i := range 3 {
		msgs = append(msgs, dbtest.UserMsg(sid, i, fmt.Sprintf("m%d", i)))
	}
	dbtest.SeedMessages(t, d, msgs...)

	svc := service.NewHTTPBackend(baseURL, "", false)
	list, err := svc.Messages(context.Background(), sid, service.MessageFilter{
		Direction: "desc",
		Limit:     100,
	})
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Equal(t, 3, list.Count)
	assert.Equal(t, 2, list.Messages[0].Ordinal,
		"desc iteration should return highest ordinal first")
}

func TestHTTPBackend_ToolCalls_Empty(t *testing.T) {
	t.Parallel()
	baseURL, d := newHTTPTestServer(t)
	const sid = "tc-empty"
	dbtest.SeedSession(t, d, sid, "p1", func(s *db.Session) {
		s.MessageCount = 1
	})
	dbtest.SeedMessages(t, d, dbtest.UserMsg(sid, 0, "hi"))

	svc := service.NewHTTPBackend(baseURL, "", false)
	list, err := svc.ToolCalls(context.Background(), sid)
	require.NoError(t, err)
	require.NotNil(t, list)
	assert.Equal(t, 0, list.Count)
	assert.Empty(t, list.ToolCalls)
}

func TestHTTPBackend_Sync_ReadOnly(t *testing.T) {
	t.Parallel()
	baseURL, _ := newHTTPTestServer(t)

	svc := service.NewHTTPBackend(baseURL, "", true)
	_, err := svc.Sync(context.Background(), service.SyncInput{
		Path: "/tmp/whatever",
	})
	require.Error(t, err)
	// Sentinel matches the direct-backend error so callers can
	// errors.Is it regardless of transport.
	assert.True(t, errors.Is(err, db.ErrReadOnly),
		"want db.ErrReadOnly, got %v", err)
	assert.Contains(t, err.Error(), baseURL)
}

func TestHTTPBackend_Sync_RemoteReadOnly(t *testing.T) {
	t.Parallel()
	// The test server is built with a nil engine, so the remote's
	// Sync returns a 501. The httpBackend is not marked read-only
	// locally, so the round-trip surfaces the remote's read-only
	// state as db.ErrReadOnly.
	baseURL, _ := newHTTPTestServer(t)

	svc := service.NewHTTPBackend(baseURL, "", false)
	_, err := svc.Sync(context.Background(), service.SyncInput{
		Path: "/tmp/whatever",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrReadOnly),
		"want db.ErrReadOnly, got %v", err)
}

func TestHTTPBackend_Watch_ReceivesSessionUpdated(t *testing.T) {
	t.Parallel()
	baseURL, d := newHTTPTestServer(t)
	dbtest.SeedSession(t, d, "s-watch", "my-app", func(s *db.Session) {
		s.MessageCount = 1
	})

	svc := service.NewHTTPBackend(baseURL, "", false)
	ctx, cancel := context.WithTimeout(
		context.Background(), 10*time.Second,
	)
	defer cancel()
	ch, err := svc.Watch(ctx, "s-watch")
	require.NoError(t, err)
	require.NotNil(t, ch)

	// Bump message count so the session monitor detects a version
	// change and emits a session_updated event. Give the server
	// handler a moment to start polling before we mutate so the
	// new baseline matches the pre-update count.
	time.Sleep(200 * time.Millisecond)
	dbtest.SeedSession(t, d, "s-watch", "my-app", func(s *db.Session) {
		s.MessageCount = 2
	})

	// PollInterval is 1.5s. Allow up to 6s before giving up so the
	// test is robust against scheduling jitter. The watch stream now
	// also emits an initial session.timing snapshot on connect plus
	// follow-up session.timing events alongside session_updated;
	// skip past them and assert on session_updated specifically.
	deadline := time.After(6 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			require.True(t, ok, "channel closed before event arrived")
			if ev.Event != "session_updated" {
				continue
			}
			assert.Equal(t, "s-watch", ev.Data)
			return
		case <-deadline:
			t.Fatal("did not receive session_updated event in time")
		}
	}
}

func TestHTTPBackend_Watch_CancelClosesChannel(t *testing.T) {
	t.Parallel()
	baseURL, d := newHTTPTestServer(t)
	dbtest.SeedSession(t, d, "s-cancel", "my-app")

	svc := service.NewHTTPBackend(baseURL, "", false)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := svc.Watch(ctx, "s-cancel")
	require.NoError(t, err)
	require.NotNil(t, ch)

	cancel()
	// After context cancel the goroutine must close the channel
	// promptly. Drain any final event and assert closure.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed after context cancel")
		}
	}
}

func TestNewHTTPBackend_TrimsTrailingSlash(t *testing.T) {
	t.Parallel()
	baseURL, d := newHTTPTestServer(t)
	dbtest.SeedSession(t, d, "trim-s", "p1")

	// Caller passes a baseURL with trailing slash; constructor
	// must normalize so the concatenated path does not have a
	// double slash.
	svc := service.NewHTTPBackend(baseURL+"/", "", false)
	detail, err := svc.Get(context.Background(), "trim-s")
	require.NoError(t, err)
	assert.Equal(t, "trim-s", detail.ID)
}

// TestHTTPBackend_AuthToken verifies that a daemon running with
// require_auth accepts Get requests when the backend is
// constructed with the same bearer token, and rejects requests
// with a missing or wrong token as 401.
func TestHTTPBackend_AuthToken(t *testing.T) {
	t.Parallel()
	const goodToken = "correct-horse-battery-staple"
	baseURL, d := newHTTPTestServerWithCfg(t, config.Config{
		RequireAuth: true,
		AuthToken:   goodToken,
	})
	dbtest.SeedSession(t, d, "auth-s", "p1")

	t.Run("good token succeeds", func(t *testing.T) {
		svc := service.NewHTTPBackend(baseURL, goodToken, false)
		detail, err := svc.Get(context.Background(), "auth-s")
		require.NoError(t, err)
		require.NotNil(t, detail)
		assert.Equal(t, "auth-s", detail.ID)
	})

	t.Run("missing token returns 401 error", func(t *testing.T) {
		svc := service.NewHTTPBackend(baseURL, "", false)
		_, err := svc.Get(context.Background(), "auth-s")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "401")
	})

	t.Run("wrong token returns 401 error", func(t *testing.T) {
		svc := service.NewHTTPBackend(baseURL, "wrong-token", false)
		_, err := svc.Get(context.Background(), "auth-s")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "401")
	})
}
