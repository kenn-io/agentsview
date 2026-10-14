package server_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/sessionwatch"
)

func seedAnnotatedSession(t *testing.T, te *testEnv, id string, prs []db.PRLink) {
	t.Helper()
	require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{
		ID: id, Machine: "test", Agent: "claude", Project: "proj",
		MessageCount: 4, UserMessageCount: 2, PRLinks: prs,
	}))
}

func TestSessionAnnotationValidationAndEmptyArrays(t *testing.T) {
	te := setup(t)
	seedAnnotatedSession(t, te, "worker", nil)
	w := te.patch(t, "/api/v1/sessions/worker/labels", `{"add":["=missing-key"]}`)
	assertStatus(t, w, http.StatusBadRequest)
	query := url.Values{}
	for i := range db.MaxSessionLabels + 1 {
		query.Add("label", fmt.Sprintf("label-%d", i))
	}
	for _, route := range []string{"/api/v1/sessions", "/api/v1/sessions/sidebar-index"} {
		w = te.get(t, route+"?"+query.Encode())
		assertStatus(t, w, http.StatusBadRequest)
	}
	w = te.get(t, "/api/v1/sessions?pr=not-a-repo")
	assertStatus(t, w, http.StatusBadRequest)
	w = te.get(t, "/api/v1/sessions/worker")
	assertStatus(t, w, http.StatusOK)
	assert.Contains(t, w.Body.String(), `"labels":[]`)
	assert.Contains(t, w.Body.String(), `"pr_links":[]`)
}

func TestSessionParentAPI(t *testing.T) {
	te := setup(t)
	seedAnnotatedSession(t, te, "manager", nil)

	w := te.put(t, "/api/v1/sessions/worker/parent",
		`{"parent_session_id":"manager"}`)
	assertStatus(t, w, http.StatusOK)

	w = te.put(t, "/api/v1/sessions/manager/parent",
		`{"parent_session_id":"worker"}`)
	assertStatus(t, w, http.StatusBadRequest)
}

func TestAnnotationFiltersHonorExplicitOneShotExclusion(t *testing.T) {
	te := setup(t)
	_, err := te.db.SetSessionLabels(t.Context(), "worker", []string{"nightly"})
	require.NoError(t, err)
	for _, route := range []string{"/api/v1/sessions", "/api/v1/sessions/sidebar-index"} {
		for _, toggle := range []string{"&include_one_shot=false", "&include_automated=false"} {
			automated := toggle == "&include_automated=false"
			userMessages := 1
			if automated {
				userMessages = 2
			}
			require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{
				ID: "worker", Machine: "test", Agent: "claude", Project: "proj",
				MessageCount: 3, UserMessageCount: userMessages, IsAutomated: automated,
			}))
			w := te.get(t, route+"?label=nightly"+toggle)
			assertStatus(t, w, http.StatusOK)
			assert.NotContains(t, w.Body.String(), `"id":"worker"`)
		}
	}
}

func TestSessionAnnotationWritesUnavailableOnReadOnlyStore(t *testing.T) {
	te := setupPGMode(t)
	w := te.patch(t, "/api/v1/sessions/any/labels", `{"add":["x"]}`)
	assertStatus(t, w, http.StatusNotImplemented)
	w = te.put(t, "/api/v1/sessions/any/parent", `{"parent_session_id":"p"}`)
	assertStatus(t, w, http.StatusNotImplemented)
}

func TestSessionAnnotationWatchWithFutureSyncMarker(t *testing.T) {
	t.Cleanup(sessionwatch.SetTimingsForTest(25*time.Millisecond, 50*time.Millisecond))
	te := setup(t)
	seedAnnotatedSession(t, te, "worker", nil)
	seedAnnotatedSession(t, te, "manager", nil)
	require.NoError(t, te.db.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), "UPDATE sessions SET sync_marker = '2999-01-01T00:00:00Z' WHERE id = 'worker'")
		return err
	}))
	for _, tt := range []struct {
		name  string
		write func() error
	}{
		{"label set", func() error { _, err := te.db.SetSessionLabels(t.Context(), "worker", []string{"nightly"}); return err }},
		{"parent set", func() error { _, err := te.db.SetSessionExternalParent(t.Context(), "worker", "manager"); return err }},
		{"pending parent set", func() error { _, err := te.db.SetSessionExternalParent(t.Context(), "worker", "missing-a"); return err }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			w := newFlushRecorder()
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/sessions/worker/watch", nil)
			done := make(chan struct{})
			go func() { defer close(done); te.handler.ServeHTTP(w, req) }()
			t.Cleanup(func() { cancel(); <-done })
			te.waitForSSEEvent(t, w, "session.timing", 3*time.Second)
			require.NoError(t, tt.write())
			te.waitForSSEEvent(t, w, "session_updated", 3*time.Second)
		})
	}
}
