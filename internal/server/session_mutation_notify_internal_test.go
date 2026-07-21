package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// TestSessionMutationRoutesNotify pins that the routes changing a session's
// lifecycle — trash, restore, permanent delete — fire the mutation notifier
// on success and stay silent on failure. Consumers (the extraction
// scheduler's retraction pass) otherwise only learn about these changes
// from sync activity that may never come.
func TestSessionMutationRoutesNotify(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database := dbtest.OpenTestDBAt(t, dbPath)
	var notified atomic.Int32
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {dir}},
		Machine:   "test",
	})
	s := New(config.Config{
		Host: "127.0.0.1", Port: 0, DataDir: dir, DBPath: dbPath,
	}, database, engine, WithSessionMutationNotifier(func() {
		notified.Add(1)
	}))
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "sess-1", Project: "proj", Machine: "local", Agent: "claude",
	}))

	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, reader)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		return w
	}

	w := do(http.MethodPost, "/api/v1/sessions/batch-delete",
		`{"session_ids":["sess-1"]}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	require.Equal(t, int32(1), notified.Load(), "trashing must notify")

	w = do(http.MethodPost, "/api/v1/sessions/sess-1/restore", "")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	require.Equal(t, int32(2), notified.Load(), "restoring must notify")

	w = do(http.MethodPost, "/api/v1/sessions/missing/restore", "")
	require.Equal(t, http.StatusNotFound, w.Code)
	require.Equal(t, int32(2), notified.Load(),
		"a failed restore must not notify")

	w = do(http.MethodPost, "/api/v1/sessions/batch-delete",
		`{"session_ids":["sess-1"]}`)
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	w = do(http.MethodDelete, "/api/v1/sessions/sess-1/permanent", "")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	require.Equal(t, int32(4), notified.Load(),
		"permanent deletion must notify")

	w = do(http.MethodDelete, "/api/v1/trash", "")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, int32(5), notified.Load(), "emptying trash must notify")

	require.NoError(t, database.UpsertSession(db.Session{
		ID: "sess-2", Project: "proj", Machine: "local", Agent: "claude",
	}))
	w = do(http.MethodDelete, "/api/v1/sessions/sess-2", "")
	require.Equal(t, http.StatusNoContent, w.Code, "body: %s", w.Body.String())
	require.Equal(t, int32(6), notified.Load(),
		"single-session deletion must notify")

	// A daemon-delegated secret scan changes eligibility in both
	// directions: new findings retract generated entries, and fresh clean
	// stamps make sessions extractable.
	w = do(http.MethodPost, "/api/v1/secrets/scan", "")
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, int32(7), notified.Load(),
		"a completed daemon scan must notify")
}

// cancelOnProgressWriter cancels the request context as soon as the
// first SSE progress event is written, simulating a client that
// disconnects mid-scan.
type cancelOnProgressWriter struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (w *cancelOnProgressWriter) Write(b []byte) (int, error) {
	if bytes.Contains(b, []byte("progress")) {
		w.cancel()
	}
	return w.ResponseRecorder.Write(b)
}

// TestSecretScanNotifiesOnPartialCommit pins that a scan interrupted after
// committing per-session results still notifies extraction scheduling:
// each session's findings and stamp land as the scan walks, so a
// cancellation mid-scan has already changed eligibility even though the
// scan itself returns an error.
func TestSecretScanNotifiesOnPartialCommit(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	database := dbtest.OpenTestDBAt(t, dbPath)
	var notified atomic.Int32
	engine := sync.NewEngine(database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {dir}},
		Machine:   "test",
	})
	s := New(config.Config{
		Host: "127.0.0.1", Port: 0, DataDir: dir, DBPath: dbPath,
	}, database, engine, WithSessionMutationNotifier(func() {
		notified.Add(1)
	}))
	// Progress reports every 50 sessions; 60 guarantee the cancellation
	// lands mid-scan with sessions still queued behind it.
	for i := range 60 {
		id := fmt.Sprintf("sess-%02d", i)
		require.NoError(t, database.UpsertSession(db.Session{
			ID: id, Project: "proj", Machine: "local", Agent: "claude",
			MessageCount: 1,
		}))
		require.NoError(t, database.InsertMessages([]db.Message{{
			SessionID: id, Ordinal: 0, Role: "user", Content: "hello",
		}}))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(
		http.MethodPost, "/api/v1/secrets/scan?backfill=true", nil,
	).WithContext(ctx)
	w := &cancelOnProgressWriter{
		ResponseRecorder: httptest.NewRecorder(), cancel: cancel,
	}
	s.mux.ServeHTTP(w, req)

	require.Contains(t, w.Body.String(), "error",
		"the cancelled scan must surface the error event")
	require.Equal(t, int32(1), notified.Load(),
		"a scan that committed work before failing must notify "+
			"extraction scheduling; the committed stamps already "+
			"changed eligibility")
}
