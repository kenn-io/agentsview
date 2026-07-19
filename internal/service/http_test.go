package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

type recallUnavailableHTTPStore struct {
	readOnlyHTTPStore
}

func (recallUnavailableHTTPStore) GetRecallEntry(
	context.Context, string,
) (*db.RecallEntry, error) {
	return nil, db.ErrReadOnly
}

func (recallUnavailableHTTPStore) QueryRecallEntries(
	context.Context, db.RecallQuery,
) (db.RecallPage, error) {
	return db.RecallPage{}, db.ErrReadOnly
}

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

func TestHTTPBackend_ListRecallEntriesRejectsNegativeLimitLocally(t *testing.T) {
	t.Parallel()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	t.Cleanup(srv.Close)

	svc := service.NewHTTPBackend(srv.URL, "", false)
	_, err := svc.ListRecallEntries(context.Background(), service.RecallFilter{
		Limit: -1,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "limit must be non-negative")
	assert.Equal(t, 0, calls)
}

func TestHTTPBackend_ListRecallEntriesIncludesSourceEpisodeIDParam(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/recall/entries", r.URL.Path)
		assert.Equal(t, "recall-session:chunk:0001", r.URL.Query().Get("source_episode_id"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	t.Cleanup(srv.Close)

	svc := service.NewHTTPBackend(srv.URL, "", false)
	_, err := svc.ListRecallEntries(context.Background(), service.RecallFilter{
		SourceEpisodeID: "recall-session:chunk:0001",
	})

	require.NoError(t, err)
}

func TestHTTPBackend_ListRecallEntriesIncludesTrustedOnlyParam(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/recall/entries", r.URL.Path)
		assert.Equal(t, "true", r.URL.Query().Get("trusted_only"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	t.Cleanup(srv.Close)

	svc := service.NewHTTPBackend(srv.URL, "", false)
	list, err := svc.ListRecallEntries(context.Background(), service.RecallFilter{
		TrustedOnly: true,
	})

	require.NoError(t, err)
	encoded, err := json.Marshal(list)
	require.NoError(t, err)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &raw))
	require.Contains(t, raw, "trusted_only")
	var trustedOnly bool
	require.NoError(t, json.Unmarshal(raw["trusted_only"], &trustedOnly))
	assert.True(t, trustedOnly)
}

func TestHTTPBackend_QueryRecallEntriesRejectsNegativeLimitLocally(t *testing.T) {
	t.Parallel()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	t.Cleanup(srv.Close)

	svc := service.NewHTTPBackend(srv.URL, "", false)
	_, err := svc.QueryRecallEntries(context.Background(), service.RecallQuery{
		Query: "cwd",
		Limit: -1,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "limit must be non-negative")
	assert.Equal(t, 0, calls)
}

func TestHTTPBackend_QueryRecallEntriesIncludesSourceEpisodeID(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/recall/query", r.URL.Path)
		var got service.RecallQuery
		require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		assert.Equal(t, "recall-session:chunk:0001", got.SourceEpisodeID)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	t.Cleanup(srv.Close)

	svc := service.NewHTTPBackend(srv.URL, "", false)
	_, err := svc.QueryRecallEntries(context.Background(), service.RecallQuery{
		Query:           "cwd",
		SourceEpisodeID: "recall-session:chunk:0001",
	})

	require.NoError(t, err)
}

func TestHTTPBackend_QueryRecallEntriesRejectsNegativeContextMaxBytesLocally(t *testing.T) {
	t.Parallel()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"entries":[]}`))
	}))
	t.Cleanup(srv.Close)

	svc := service.NewHTTPBackend(srv.URL, "", false)
	_, err := svc.QueryRecallEntries(context.Background(), service.RecallQuery{
		Query:           "cwd",
		IncludeContext:  true,
		ContextMaxBytes: -1,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "context_max_bytes must be non-negative")
	assert.Equal(t, 0, calls)
}

func TestHTTPBackend_QueryRecallEntriesBuildsMissingSummaries(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/recall/query", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(service.RecallQueryResult{
			RecallEntries: []db.RecallResult{{
				RecallEntry: db.RecallEntry{
					ID:              "m-http-summary",
					Type:            "procedure",
					Scope:           "project",
					Status:          "accepted",
					Project:         "agentsview",
					Agent:           "codex",
					SourceSessionID: "recall-session",
					SourceEpisodeID: "recall-session:chunk:0001",
					SourceRunID:     "smoke-run",
				},
				MatchReasons: []string{"keyword", "evidence"},
			}},
			Context: "Relevant prior agentsview entries",
			ContextMeta: &service.RecallContextMeta{
				EntryCount:  1,
				IncludedIDs: []string{"m-http-summary"},
			},
		}))
	}))
	t.Cleanup(srv.Close)

	svc := service.NewHTTPBackend(srv.URL, "", false)
	got, err := svc.QueryRecallEntries(context.Background(), service.RecallQuery{
		Query:          "cwd failed reads",
		IncludeContext: true,
	})

	require.NoError(t, err)
	require.NotNil(t, got.Summary)
	assert.Equal(t, 1, got.Summary.Count)
	assert.Equal(t, 1, got.Summary.ByType["procedure"])
	assert.Equal(t, 1, got.Summary.ByMatchReason["keyword"])
	assert.Equal(t, 1, got.Summary.ByMatchReason["evidence"])
	assert.Equal(t, 1, got.Summary.BySourceRun["smoke-run"])
	assert.Equal(t, 1, got.Summary.BySourceEpisode["recall-session:chunk:0001"])
	require.Len(t, got.ContextEntries, 1)
	assert.Equal(t, "m-http-summary", got.ContextEntries[0].ID)
	require.NotNil(t, got.ContextSummary)
	assert.Equal(t, 1, got.ContextSummary.Count)
	assert.Equal(t, 1, got.ContextSummary.BySourceSession["recall-session"])
	assert.Equal(t, 1, got.ContextSummary.BySourceEpisode["recall-session:chunk:0001"])
}

func TestHTTPBackend_QueryRecallEntriesRejectsInconsistentContextEntries(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/recall/query", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(service.RecallQueryResult{
			RecallEntries: []db.RecallResult{
				{RecallEntry: db.RecallEntry{ID: "m-packed"}},
				{RecallEntry: db.RecallEntry{ID: "m-other"}},
			},
			Context: "Relevant prior agentsview entries",
			ContextMeta: &service.RecallContextMeta{
				EntryCount:  1,
				IncludedIDs: []string{"m-packed"},
			},
			ContextEntries: []db.RecallResult{
				{RecallEntry: db.RecallEntry{ID: "m-other"}},
			},
		}))
	}))
	t.Cleanup(srv.Close)

	svc := service.NewHTTPBackend(srv.URL, "", false)
	_, err := svc.QueryRecallEntries(context.Background(), service.RecallQuery{
		Query:          "cwd failed reads",
		IncludeContext: true,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"context_entries ids must match context_meta.included_ids")
}

func TestHTTPBackend_QueryRecallEntriesRejectsMissingContextRecallEntryRows(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/recall/query", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(service.RecallQueryResult{
			RecallEntries: []db.RecallResult{},
			Context:       "Relevant prior agentsview entries",
			ContextMeta: &service.RecallContextMeta{
				EntryCount:  1,
				IncludedIDs: []string{"m-packed"},
			},
		}))
	}))
	t.Cleanup(srv.Close)

	svc := service.NewHTTPBackend(srv.URL, "", false)
	_, err := svc.QueryRecallEntries(context.Background(), service.RecallQuery{
		Query:          "cwd failed reads",
		IncludeContext: true,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"context_entries ids must match context_meta.included_ids")
}

func TestHTTPBackend_QueryRecallEntriesReportsTrustedOnlyFallback(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/recall/query", r.URL.Path)
		var req service.RecallQuery
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.True(t, req.TrustedOnly)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(service.RecallQueryResult{
			RecallEntries: []db.RecallResult{},
		}))
	}))
	t.Cleanup(srv.Close)

	svc := service.NewHTTPBackend(srv.URL, "", false)
	got, err := svc.QueryRecallEntries(context.Background(), service.RecallQuery{
		Query:       "cwd failed reads",
		TrustedOnly: true,
	})

	require.NoError(t, err)
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &raw))
	require.Contains(t, raw, "trusted_only")
	var trustedOnly bool
	require.NoError(t, json.Unmarshal(raw["trusted_only"], &trustedOnly))
	assert.True(t, trustedOnly)
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

func TestHTTPBackend_ImportRecallEntries_ReadOnly(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)

	svc := env.Backend("", true)
	_, err := svc.ImportRecallEntries(
		context.Background(),
		strings.NewReader(""),
		db.RecallImportOptions{DryRun: true},
	)
	require.Error(t, err)
	// Mirrors Sync/ScanSecrets: a read-only backend short-circuits to the
	// shared sentinel instead of posting to the import endpoint.
	assert.True(t, errors.Is(err, db.ErrReadOnly),
		"want db.ErrReadOnly, got %v", err)
	assert.Contains(t, err.Error(), env.BaseURL)
}

func TestHTTPBackend_ImportRecallEntries_RemoteReadOnly(t *testing.T) {
	t.Parallel()
	// A read-only (pg serve) daemon answers write endpoints with 501. The
	// backend is not marked read-only locally, so the round-trip must surface
	// the remote's state as db.ErrReadOnly rather than a bare HTTP error.
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotImplemented)
		}))
	defer srv.Close()

	be := service.NewHTTPBackend(srv.URL, "", false)
	_, err := be.ImportRecallEntries(
		context.Background(),
		strings.NewReader(""),
		db.RecallImportOptions{},
	)
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrReadOnly),
		"want db.ErrReadOnly, got %v", err)
	assert.Contains(t, err.Error(), srv.URL)
}

func TestHTTPBackend_RecallReads_RemoteReadOnly(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t, withHTTPStore(func(d *db.DB) db.Store {
		return recallUnavailableHTTPStore{
			readOnlyHTTPStore: readOnlyHTTPStore{DB: d},
		}
	}))
	svc := env.Backend("", false)

	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "list",
			run: func() error {
				_, err := svc.ListRecallEntries(context.Background(), service.RecallFilter{})
				return err
			},
		},
		{
			name: "get",
			run: func() error {
				_, err := svc.GetRecallEntry(context.Background(), "entry-1")
				return err
			},
		},
		{
			name: "query",
			run: func() error {
				_, err := svc.QueryRecallEntries(context.Background(), service.RecallQuery{
					Query: "retry policy",
				})
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			require.ErrorIs(t, err, db.ErrReadOnly)
			assert.Contains(t, err.Error(), env.BaseURL)
		})
	}
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

func TestHTTPSearchContentSemanticSetsIntentHeader(t *testing.T) {
	t.Parallel()
	var gotIntent string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotIntent = r.Header.Get("X-AgentsView-Search-Intent")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"matches":[],"next_cursor":0}`))
		}))
	defer srv.Close()
	be := service.NewHTTPBackend(srv.URL, "", true)

	_, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
		Pattern: "needle", Mode: "semantic", Limit: 50,
	})

	require.NoError(t, err)
	assert.Equal(t, "semantic", gotIntent)
}

func TestHTTPImportRecallEntriesPassesAllowProductionImport(t *testing.T) {
	t.Parallel()
	var gotAllow string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotAllow = r.URL.Query().Get("allow_production_import")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"imported":0,"skipped":0}`))
		}))
	defer srv.Close()
	be := service.NewHTTPBackend(srv.URL, "", false)

	_, err := be.ImportRecallEntries(
		context.Background(),
		strings.NewReader(""),
		db.RecallImportOptions{AllowProductionImport: true},
	)

	require.NoError(t, err)
	assert.Equal(t, "true", gotAllow)
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

// TestHTTPSearchContent_501PreservesCauseDetail asserts that a 501 response
// carrying cause-specific remediation text (the shape searcherAdapter's
// translateSearchError produces for a still-building index) survives the
// daemon round-trip instead of being collapsed to the bare
// ErrSemanticUnavailable sentinel message.
func TestHTTPSearchContent_501PreservesCauseDetail(t *testing.T) {
	t.Parallel()
	body := service.ErrSemanticUnavailable.Error() + ": index is building: 40% complete"
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = w.Write([]byte(`{"error":"` + body + `"}`))
		}))
	defer srv.Close()

	be := service.NewHTTPBackend(srv.URL, "", true)
	_, err := be.SearchContent(context.Background(), service.ContentSearchRequest{Pattern: "needle"})
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrSemanticUnavailable)
	assert.Contains(t, err.Error(), "index is building: 40% complete")
}

func TestHTTPSearchContent_501PreservesBackendSpecificReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{
			name: "DuckDB unsupported",
			body: "semantic search not available: semantic search is not " +
				"supported by the DuckDB backend",
		},
		{
			name: "PostgreSQL vector disabled",
			body: "semantic search not available: semantic search: PostgreSQL " +
				"requires [vector] enabled with a matching [vector.embeddings] " +
				"config and a generation pushed by 'agentsview pg push'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(
				func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusNotImplemented)
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{
						"error": tt.body,
					}))
				}))
			defer srv.Close()

			be := service.NewHTTPBackend(srv.URL, "", true)
			_, err := be.SearchContent(context.Background(), service.ContentSearchRequest{
				Pattern: "needle", Mode: "semantic",
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, service.ErrSemanticUnavailable)
			assert.Equal(t, tt.body, err.Error())
			assert.NotContains(t, err.Error(), "agentsview embeddings build")
		})
	}
}

// TestHTTPSearchContent_501IdenticalToSentinelDoesNotDuplicate asserts that
// when the 501 body's message is exactly the sentinel's own text (no extra
// cause — e.g. ErrNoActiveGeneration's case), the client returns the bare
// sentinel rather than a message with the sentinel text repeated twice.
func TestHTTPSearchContent_501IdenticalToSentinelDoesNotDuplicate(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = w.Write([]byte(`{"error":"` + service.ErrSemanticUnavailable.Error() + `"}`))
		}))
	defer srv.Close()

	be := service.NewHTTPBackend(srv.URL, "", true)
	_, err := be.SearchContent(context.Background(), service.ContentSearchRequest{Pattern: "needle"})
	require.Error(t, err)
	assert.ErrorIs(t, err, service.ErrSemanticUnavailable)
	assert.Equal(t, service.ErrSemanticUnavailable.Error(), err.Error(),
		"the sentinel text must not be duplicated when the body carries no extra cause")
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
