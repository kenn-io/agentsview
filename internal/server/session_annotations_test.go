package server_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func seedAnnotatedSession(t *testing.T, te *testEnv, id string, prs []db.PRLink) {
	t.Helper()
	require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{
		ID: id, Machine: "test", Agent: "claude", Project: "proj",
		MessageCount: 4, UserMessageCount: 2, PRLinks: prs,
	}))
}

func listedSessionIDs(t *testing.T, te *testEnv, query url.Values) []string {
	t.Helper()
	w := te.get(t, "/api/v1/sessions?"+query.Encode())
	assertStatus(t, w, http.StatusOK)
	list := decode[service.SessionList](t, w)
	return orderedSessionIDs(list.Sessions)
}

func TestSessionLabelsAPI(t *testing.T) {
	te := setup(t)
	seedAnnotatedSession(t, te, "worker", nil)
	seedAnnotatedSession(t, te, "other", nil)
	seedAnnotatedSession(t, te, "linked", []db.PRLink{{
		URL: "https://github.com/owner/repo/pull/7", Host: "github.com",
		Repository: "owner/repo", Number: 7,
	}})

	w := te.patch(t, "/api/v1/sessions/worker/labels",
		`{"clear":true,"add":["ticket=ABC-123","role=reviewer"]}`)
	assertStatus(t, w, http.StatusOK)
	labels := decode[db.SessionLabels](t, w)
	assert.True(t, labels.SessionFound)
	assert.Equal(t, []string{"role=reviewer", "ticket=ABC-123"}, labels.Labels)

	w = te.patch(t, "/api/v1/sessions/worker/labels",
		`{"add":["nightly"],"remove":["role=reviewer"]}`)
	assertStatus(t, w, http.StatusOK)

	w = te.get(t, "/api/v1/sessions/worker")
	assertStatus(t, w, http.StatusOK)
	detail := decode[service.SessionDetail](t, w)
	assert.Equal(t, []string{"nightly", "ticket=ABC-123"}, detail.Labels)

	assert.Equal(t, []string{"worker"}, listedSessionIDs(t, te, url.Values{
		"label": {"nightly", "ticket=ABC-123"},
	}))
	query := url.Values{}
	for range 1000 {
		query.Add("label", " nightly ")
	}
	assert.Equal(t, []string{"worker"}, listedSessionIDs(t, te, query))

	query = url.Values{}
	for i := range db.MaxSessionLabels {
		query.Add("label", fmt.Sprintf("label-%d", i))
	}
	for _, route := range []string{"/api/v1/sessions", "/api/v1/sessions/sidebar-index"} {
		w = te.get(t, route+"?"+query.Encode())
		assertStatus(t, w, http.StatusOK)
	}
	query.Add("label", "over-limit")
	for _, route := range []string{"/api/v1/sessions", "/api/v1/sessions/sidebar-index"} {
		w = te.get(t, route+"?"+query.Encode())
		assertStatus(t, w, http.StatusBadRequest)
		assert.Contains(t, w.Body.String(), "more than 64 labels")
	}

	w = te.patch(t, "/api/v1/sessions/worker/labels", `{"add":["=missing-key"]}`)
	assertStatus(t, w, http.StatusBadRequest)

	w = te.patch(t, "/api/v1/sessions/worker/labels", `{"clear":true}`)
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, []string{}, decode[db.SessionLabels](t, w).Labels)

	// Cleared values stay in the body so a client merging it over a cached
	// session drops them.
	w = te.get(t, "/api/v1/sessions/worker")
	assertStatus(t, w, http.StatusOK)
	assert.Contains(t, w.Body.String(), `"labels":[]`)
	assert.Contains(t, w.Body.String(), `"pr_links":[]`)

	assert.Equal(t, []string{"linked"},
		listedSessionIDs(t, te, url.Values{"pr": {"owner/repo#7"}}))

	w = te.get(t, "/api/v1/sessions?pr=not-a-repo")
	assertStatus(t, w, http.StatusBadRequest)

	w = te.get(t, "/api/v1/sessions/linked")
	assertStatus(t, w, http.StatusOK)
	detail = decode[service.SessionDetail](t, w)
	require.Len(t, detail.PRLinks, 1)
	assert.Equal(t, "https://github.com/owner/repo/pull/7", detail.PRLinks[0].URL)
}

func TestSessionParentAPI(t *testing.T) {
	te := setup(t)
	seedAnnotatedSession(t, te, "manager", nil)

	w := te.put(t, "/api/v1/sessions/worker/parent",
		`{"parent_session_id":"manager"}`)
	assertStatus(t, w, http.StatusOK)
	link := decode[db.SessionExternalParent](t, w)
	assert.False(t, link.SessionFound)
	assert.Equal(t, "subagent", link.RelationshipType)

	w = te.put(t, "/api/v1/sessions/manager/parent",
		`{"parent_session_id":"worker"}`)
	assertStatus(t, w, http.StatusBadRequest)

	// Clearing a pending parent is safe to retry.
	w = te.put(t, "/api/v1/sessions/worker/parent", `{}`)
	assertStatus(t, w, http.StatusOK)
	w = te.put(t, "/api/v1/sessions/worker/parent", `{"parent_session_id":""}`)
	assertStatus(t, w, http.StatusOK)
	assert.False(t, decode[db.SessionExternalParent](t, w).SessionFound)
	parent := "manager"
	require.NoError(t, te.db.UpsertSession(t.Context(), db.Session{ID: "native", Agent: "claude", Project: "proj", ParentSessionID: &parent, RelationshipType: "continuation"}))
	for range 2 {
		w = te.put(t, "/api/v1/sessions/native/parent", `{}`)
		assertStatus(t, w, http.StatusOK)
		assert.True(t, decode[db.SessionExternalParent](t, w).SessionFound)
	}
	native, err := te.db.GetSession(t.Context(), "native")
	require.NoError(t, err)
	assert.Equal(t, &parent, native.ParentSessionID)
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
			for _, toggle := range []string{"", "&include_one_shot=false", "&include_one_shot=true", "&include_automated=false", "&include_automated=true"} {
				w := te.get(t, route+"?"+filter+toggle)
				assertStatus(t, w, http.StatusOK)
				if toggle == "&include_one_shot=false" || toggle == "&include_automated=false" {
					assert.NotContains(t, w.Body.String(), `"id":"worker"`)
				} else {
					assert.Contains(t, w.Body.String(), `"id":"worker"`)
				}
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
