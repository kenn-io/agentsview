package service_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
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
	score := 92
	grade := "A"
	err := d.UpdateSessionSignals("s-1", db.SessionSignalUpdate{
		Outcome:           "completed",
		OutcomeConfidence: "high",
		EndedWithRole:     "assistant",
		HealthScore:       &score,
		HealthGrade:       &grade,
		QualitySignals: db.QualitySignals{
			Version:                     db.CurrentQualitySignalVersion,
			ShortPromptCount:            1,
			UnstructuredStart:           true,
			MissingSuccessCriteriaCount: 1,
			MissingVerificationCount:    1,
			DuplicatePromptCount:        2,
			NoCodeContextCount:          1,
			RunawayToolLoopCount:        1,
		},
	})
	require.NoError(t, err)

	svc := service.NewHTTPBackend(baseURL, "", false)
	detail, err := svc.Get(context.Background(), "s-1")
	require.NoError(t, err)
	require.NotNil(t, detail)
	assert.Equal(t, "s-1", detail.ID)
	assert.Equal(t, "my-app", detail.Project)
	assert.Equal(t, 2, detail.MessageCount)
	assert.Equal(t, db.CurrentQualitySignalVersion,
		detail.QualitySignalVersion)
	assert.Equal(t, 2, detail.DuplicatePromptCount)
	assert.True(t, detail.UnstructuredStart)
	assert.Contains(t, detail.HealthScoreBasis, "prompt_quality")
	assert.NotContains(t, detail.HealthPenalties, "repeated_prompts")
	assert.Equal(t, 4,
		detail.HealthPenalties["stuck_repeated_prompts"])
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

func TestHTTPBackend_List_StarredFilterRoundtrip(t *testing.T) {
	t.Parallel()
	baseURL, d := newHTTPTestServer(t)
	dbtest.SeedSession(t, d, "starred-1", "proj", func(s *db.Session) {
		s.MessageCount = 3
	})
	dbtest.SeedSession(t, d, "plain-1", "proj", func(s *db.Session) {
		s.MessageCount = 3
	})
	ok, err := d.StarSession("starred-1")
	require.NoError(t, err)
	require.True(t, ok)

	svc := service.NewHTTPBackend(baseURL, "", false)
	list, err := svc.List(context.Background(), service.ListFilter{
		IncludeOneShot: true,
		Starred:        true,
		Limit:          10,
	})
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Len(t, list.Sessions, 1)
	assert.Equal(t, "starred-1", list.Sessions[0].ID)
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

func TestHTTPSearchContent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/search/content" {
				t.Errorf("path = %s", r.URL.Path)
			}
			if r.URL.Query().Get("pattern") != "needle" {
				t.Errorf("pattern = %s", r.URL.Query().Get("pattern"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"matches":[{"session_id":"s1","location":"message"}],"next_cursor":0}`))
		}))
	defer srv.Close()
	be := service.NewHTTPBackend(srv.URL, "", true)
	res, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
		Pattern: "needle", Limit: 50,
	})
	require.NoError(t, err)
	require.Len(t, res.Matches, 1)
	assert.Equal(t, "s1", res.Matches[0].SessionID)
}

func TestHTTPSearchContent_RealServer(t *testing.T) {
	t.Parallel()
	baseURL, d := newHTTPTestServer(t)
	// Seed a session with UserMessageCount=2 so content search includes it.
	dbtest.SeedSession(t, d, "cs-1", "search-proj", func(s *db.Session) {
		s.MessageCount = 3
		s.UserMessageCount = 2
	})
	msgs := []db.Message{
		dbtest.UserMsg("cs-1", 0, "find the needle in the haystack"),
		dbtest.AsstMsg("cs-1", 1, "here it is"),
	}
	dbtest.SeedMessages(t, d, msgs...)

	svc := service.NewHTTPBackend(baseURL, "", true)
	res, err := svc.SearchContent(context.Background(), service.ContentSearchRequest{
		Pattern: "needle", Limit: 10,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Len(t, res.Matches, 1)
	assert.Equal(t, "cs-1", res.Matches[0].SessionID)
	assert.Equal(t, "message", res.Matches[0].Location)
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
