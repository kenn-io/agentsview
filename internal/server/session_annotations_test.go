package server_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func seedAnnotatedSession(t *testing.T, te *testEnv, id string, prs []db.PRLink) {
	t.Helper()
	require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{
		ID: id, Machine: "test", Agent: "claude", Project: "proj",
		MessageCount: 4, UserMessageCount: 2, PRLinks: prs,
	}))
}

func TestSessionLabelsAPI(t *testing.T) {
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
	require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{
		ID: "worker", Machine: "test", Agent: "claude", Project: "proj",
		MessageCount: 2, UserMessageCount: 1, IsAutomated: true,
		PRLinks: []db.PRLink{{URL: "https://github.com/owner/repo/pull/7", Host: "github.com", Repository: "owner/repo", Number: 7}},
	}))
	_, err := te.db.SetSessionLabels(t.Context(), "worker", []string{"nightly"})
	require.NoError(t, err)
	for _, filter := range []string{"label=nightly", "pr=owner%2Frepo%237"} {
		for _, route := range []string{"/api/v1/sessions", "/api/v1/sessions/sidebar-index"} {
			for _, toggle := range []string{"&include_one_shot=false", "&include_automated=false"} {
				w := te.get(t, route+"?"+filter+toggle)
				assertStatus(t, w, http.StatusOK)
				assert.NotContains(t, w.Body.String(), `"id":"worker"`)
			}
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
