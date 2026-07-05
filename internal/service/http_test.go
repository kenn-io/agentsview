package service_test

import (
	"context"
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
	"go.kenn.io/agentsview/internal/sessionwatch"
)

// httpBackendEnv is a running in-memory HTTP test server backed by a
// real *server.Server and SQLite DB, with a nil background sync engine
// (local serve --no-sync mode). The listener port is baked into the
// server's Host allowlist so HTTP backends can round-trip against it.
type httpBackendEnv struct {
	BaseURL string
	DB      *db.DB
}

type httpBackendOptions struct {
	cfg   config.Config
	store func(*db.DB) db.Store
}

type httpBackendEnvOpt func(*httpBackendOptions)

// withHTTPConfig overrides auth-related config (RequireAuth /
// AuthToken). Unset fields keep the env defaults.
func withHTTPConfig(cfg config.Config) httpBackendEnvOpt {
	return func(o *httpBackendOptions) { o.cfg = cfg }
}

// withHTTPStore wraps the underlying *db.DB in a custom db.Store, for
// example to present a read-only remote store to the server.
func withHTTPStore(fn func(*db.DB) db.Store) httpBackendEnvOpt {
	return func(o *httpBackendOptions) { o.store = fn }
}

// newHTTPBackendEnv builds an in-memory test server and returns its
// base URL and underlying *db.DB so callers can seed fixtures directly.
func newHTTPBackendEnv(
	t *testing.T, opts ...httpBackendEnvOpt,
) *httpBackendEnv {
	t.Helper()
	var o httpBackendOptions
	for _, opt := range opts {
		opt(&o)
	}

	d := dbtest.OpenTestDB(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port

	cfg := config.Config{
		Host:         "127.0.0.1",
		Port:         port,
		DataDir:      t.TempDir(),
		WriteTimeout: 30 * time.Second,
		RequireAuth:  o.cfg.RequireAuth,
		AuthToken:    o.cfg.AuthToken,
	}

	var store db.Store = d
	if o.store != nil {
		store = o.store(d)
	}
	srv := server.New(cfg, store, nil)
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return &httpBackendEnv{BaseURL: ts.URL, DB: d}
}

// Backend constructs an HTTP-backed SessionService pointed at this env.
func (e *httpBackendEnv) Backend(
	token string, readOnly bool,
) service.SessionService {
	return service.NewHTTPBackend(e.BaseURL, token, readOnly)
}

// SeedSession seeds a session into the env's DB.
func (e *httpBackendEnv) SeedSession(
	t *testing.T, id, project string, opts ...func(*db.Session),
) {
	t.Helper()
	dbtest.SeedSession(t, e.DB, id, project, opts...)
}

type readOnlyHTTPStore struct {
	*db.DB
}

func (readOnlyHTTPStore) ReadOnly() bool { return true }

// requireWatchEvent reads from ch until an event with the given name
// arrives, skipping other events, and returns it. It fails the test if
// the channel closes or the timeout elapses first.
func requireWatchEvent(
	t *testing.T, ch <-chan service.Event, event string, timeout time.Duration,
) service.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			require.True(t, ok, "channel closed before %q event arrived", event)
			if ev.Event != event {
				continue
			}
			return ev
		case <-deadline:
			t.Fatalf("did not receive %q event within %s", event, timeout)
		}
	}
}

// requireChannelClosed drains any pending values and asserts the
// channel closes before the timeout elapses.
func requireChannelClosed[T any](
	t *testing.T, ch <-chan T, timeout time.Duration,
) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("channel not closed within %s", timeout)
		}
	}
}

func TestHTTPBackend_Get_Roundtrip(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)
	env.SeedSession(t, "s-1", "my-app", dbtest.WithMessageCount(2))
	score := 92
	grade := "A"
	err := env.DB.UpdateSessionSignals("s-1", db.SessionSignalUpdate{
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

	svc := env.Backend("", false)
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
	env := newHTTPBackendEnv(t)

	svc := env.Backend("", false)
	// Transport-neutral contract: missing session returns (nil, nil),
	// matching directBackend.Get.
	detail, err := svc.Get(context.Background(), "does-not-exist")
	require.NoError(t, err)
	assert.Nil(t, detail)
}

func TestHTTPBackend_List_Empty(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)

	svc := env.Backend("", false)
	list, err := svc.List(context.Background(), service.ListFilter{Limit: 10})
	require.NoError(t, err)
	require.NotNil(t, list)
	assert.Equal(t, 0, list.Total)
}

func TestHTTPBackend_List_FilterRoundtrip(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)
	env.SeedSession(t, "a-1", "proj-a", dbtest.WithMessageCount(3))
	env.SeedSession(t, "b-1", "proj-b", dbtest.WithMessageCount(3))

	svc := env.Backend("", false)
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
	env := newHTTPBackendEnv(t)
	env.SeedSession(t, "starred-1", "proj", dbtest.WithMessageCount(3))
	env.SeedSession(t, "plain-1", "proj", dbtest.WithMessageCount(3))
	ok, err := env.DB.StarSession("starred-1")
	require.NoError(t, err)
	require.True(t, ok)

	svc := env.Backend("", false)
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
	env := newHTTPBackendEnv(t)

	svc := env.Backend("", false)
	_, err := svc.List(context.Background(), service.ListFilter{
		Date: "2024/01/15",
	})
	require.Error(t, err)
	// The server rejects invalid dates with 400.
	assert.Contains(t, err.Error(), "HTTP 400")
}

func TestHTTPBackend_Messages_Roundtrip(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)
	const sid = "msg-session"
	dbtest.SeedSessionWithMessages(t, env.DB, sid, "p1", []db.Message{
		dbtest.UserMsg(sid, 0, "hello"),
		dbtest.AsstMsg(sid, 1, "world"),
		dbtest.UserMsg(sid, 2, "bye"),
	}, dbtest.WithMessageCount(3))

	svc := env.Backend("", false)
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
	env := newHTTPBackendEnv(t)
	const sid = "msg-desc"
	dbtest.SeedSessionWithMessages(t, env.DB, sid, "p1",
		dbtest.UserMessagesf(sid, 3, "m%d"), dbtest.WithMessageCount(3))

	svc := env.Backend("", false)
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

func TestHTTPBackend_Messages_AroundRoundtrip(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)
	const sid = "msg-around"
	dbtest.SeedSessionWithMessages(t, env.DB, sid, "p1",
		dbtest.UserMessagesf(sid, 12, "m%d"), dbtest.WithMessageCount(12))

	svc := env.Backend("", false)
	around := 6
	list, err := svc.Messages(context.Background(), sid, service.MessageFilter{
		Around: &around,
	})
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Equal(t, 11, list.Count,
		"default before=5/after=5 around ordinal 6 spans ordinals 1..11")
	assert.Equal(t, 1, list.Messages[0].Ordinal)
	assert.Equal(t, 11, list.Messages[len(list.Messages)-1].Ordinal)
	require.NotNil(t, list.FirstOrdinal)
	require.NotNil(t, list.LastOrdinal)
	assert.Equal(t, 1, *list.FirstOrdinal)
	assert.Equal(t, 11, *list.LastOrdinal)
}

func TestHTTPBackend_Messages_RolesRoundtrip(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)
	const sid = "msg-roles"
	dbtest.SeedSessionWithMessages(t, env.DB, sid, "p1", []db.Message{
		dbtest.UserMsg(sid, 0, "u0"),
		dbtest.AsstMsg(sid, 1, "a1"),
		dbtest.UserMsg(sid, 2, "u2"),
	}, dbtest.WithMessageCount(3))

	svc := env.Backend("", false)
	zero := 0
	list, err := svc.Messages(context.Background(), sid, service.MessageFilter{
		From:  &zero,
		Limit: 100,
		Roles: []string{"user"},
	})
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Equal(t, 2, list.Count)
	for _, m := range list.Messages {
		assert.Equal(t, "user", m.Role)
	}
}

func TestHTTPBackend_Messages_AroundValidationErrorSurfaces(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)
	const sid = "msg-validation"
	dbtest.SeedSessionWithMessages(t, env.DB, sid, "p1",
		dbtest.UserMessagesf(sid, 3, "m%d"), dbtest.WithMessageCount(3))

	svc := env.Backend("", false)
	around, from := 1, 0
	_, err := svc.Messages(context.Background(), sid, service.MessageFilter{
		Around: &around,
		From:   &from,
	})
	require.Error(t, err)
}

func TestHTTPBackend_ToolCalls_Empty(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)
	const sid = "tc-empty"
	dbtest.SeedSessionWithMessages(t, env.DB, sid, "p1",
		[]db.Message{dbtest.UserMsg(sid, 0, "hi")},
		dbtest.WithMessageCount(1))

	svc := env.Backend("", false)
	list, err := svc.ToolCalls(context.Background(), sid)
	require.NoError(t, err)
	require.NotNil(t, list)
	assert.Equal(t, 0, list.Count)
	assert.Empty(t, list.ToolCalls)
}

func TestHTTPBackend_Sync_ReadOnly(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)

	svc := env.Backend("", true)
	_, err := svc.Sync(context.Background(), service.SyncInput{
		Path: "/tmp/whatever",
	})
	// Sentinel matches the direct-backend error so callers can
	// errors.Is it regardless of transport.
	require.ErrorIs(t, err, db.ErrReadOnly)
	assert.Contains(t, err.Error(), env.BaseURL)
}

func TestHTTPBackend_Sync_RemoteReadOnly(t *testing.T) {
	t.Parallel()
	// The test server uses a Store that is not a local *db.DB, so
	// the remote's Sync returns a 501. The httpBackend is not marked
	// read-only locally, so the round-trip surfaces the remote's
	// read-only state as db.ErrReadOnly.
	env := newHTTPBackendEnv(t, withHTTPStore(func(d *db.DB) db.Store {
		return readOnlyHTTPStore{DB: d}
	}))

	svc := env.Backend("", false)
	_, err := svc.Sync(context.Background(), service.SyncInput{
		Path: "/tmp/whatever",
	})
	require.ErrorIs(t, err, db.ErrReadOnly)
}

func TestHTTPBackend_Watch_ReceivesSessionUpdated(t *testing.T) {
	const watchPoll = 25 * time.Millisecond
	t.Cleanup(sessionwatch.SetTimingsForTest(
		watchPoll, 50*time.Millisecond,
	))

	env := newHTTPBackendEnv(t)
	env.SeedSession(t, "s-watch", "my-app", dbtest.WithMessageCount(1))

	svc := env.Backend("", false)
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
	time.Sleep(2 * watchPoll)
	env.SeedSession(t, "s-watch", "my-app", dbtest.WithMessageCount(2))

	// The watch stream now
	// also emits an initial session.timing snapshot on connect plus
	// follow-up session.timing events alongside session_updated;
	// skip past them and assert on session_updated specifically.
	ev := requireWatchEvent(t, ch, "session_updated", 2*time.Second)
	assert.Equal(t, "s-watch", ev.Data)
}

func TestHTTPBackend_Watch_CancelClosesChannel(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)
	env.SeedSession(t, "s-cancel", "my-app")

	svc := env.Backend("", false)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := svc.Watch(ctx, "s-cancel")
	require.NoError(t, err)
	require.NotNil(t, ch)

	cancel()
	// After context cancel the goroutine must close the channel
	// promptly. Drain any final event and assert closure.
	requireChannelClosed(t, ch, 3*time.Second)
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
	env := newHTTPBackendEnv(t)
	// Seed a session with UserMessageCount=2 so content search includes it.
	dbtest.SeedSessionWithMessages(t, env.DB, "cs-1", "search-proj", []db.Message{
		dbtest.UserMsg("cs-1", 0, "find the needle in the haystack"),
		dbtest.AsstMsg("cs-1", 1, "here it is"),
	}, dbtest.WithMessageCounts(3, 2))

	svc := env.Backend("", true)
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
	env := newHTTPBackendEnv(t)
	env.SeedSession(t, "trim-s", "p1")

	// Caller passes a baseURL with trailing slash; constructor
	// must normalize so the concatenated path does not have a
	// double slash.
	svc := service.NewHTTPBackend(env.BaseURL+"/", "", false)
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
	env := newHTTPBackendEnv(t, withHTTPConfig(config.Config{
		RequireAuth: true,
		AuthToken:   goodToken,
	}))
	env.SeedSession(t, "auth-s", "p1")

	t.Run("good token succeeds", func(t *testing.T) {
		svc := env.Backend(goodToken, false)
		detail, err := svc.Get(context.Background(), "auth-s")
		require.NoError(t, err)
		require.NotNil(t, detail)
		assert.Equal(t, "auth-s", detail.ID)
	})

	t.Run("missing token returns 401 error", func(t *testing.T) {
		svc := env.Backend("", false)
		_, err := svc.Get(context.Background(), "auth-s")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "401")
	})

	t.Run("wrong token returns 401 error", func(t *testing.T) {
		svc := env.Backend("wrong-token", false)
		_, err := svc.Get(context.Background(), "auth-s")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "401")
	})
}
